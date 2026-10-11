# Deployment: DigitalOcean App Platform and Container Registry

The default deployment is DigitalOcean **App Platform** with an image in
DigitalOcean Container Registry (DOCR). GitHub Actions tests the project, builds
`deploy/Dockerfile` for `linux/amd64`, pushes a commit-tagged image, and deploys
its immutable digest directly to production on every push to `main`.

One container serves the embedded React UI and API and runs the queue worker:
`API_RUNS_WORKER=true`. `MIGRATE_ON_START=true` applies pending migrations before
HTTP startup. PostgreSQL is external; this guide uses Neon. App Platform owns
HTTPS and ingress. No SSH host, Caddy, Node runtime or separate UI service is
needed for this path. The template is [.do/app.yaml](../.do/app.yaml).

## 1. Prepare the registry and database

Create a primary Container Registry in the same DigitalOcean team as the app.
Choose its name yourself; registry names are globally unique. Select a plan
with room for release history, and retain previous successful digests for
rollback. CI does not delete images or run registry garbage collection.
[Registry setup](https://docs.digitalocean.com/products/container-registry/how-to/create-registry/)

Create a Neon project/database near your App Platform region. In **Connect**,
turn pooling off and use the direct hostname (no `-pooler` suffix), with
`sslmode=verify-full`. Migrations use a session advisory lock and the engine
uses persistent `LISTEN` connections, so transaction pooling is unsuitable.
The database role must be able to create the schema for migrations.
[Neon pooling](https://neon.com/docs/connect/connection-pooling)

Save the database URL privately. Generate a persistent 32-byte base64 vault key
(`openssl rand -base64 32`) and store it in your password manager alongside the
vault key version. Preserve the keyring with database backups.

## 2. Configure GitHub and publish the first image

Under **Settings → Secrets and variables → Actions**, configure:

| Kind | Name | Value |
| --- | --- | --- |
| Repository secret | `DIGITALOCEAN_ACCESS_TOKEN` | A DigitalOcean token with `registry:read`, `registry:update`, `app:read` and `app:update` scopes, plus required supporting read scopes |
| Repository variable | `DO_REGISTRY_NAME` | Your primary registry's name, without `registry.digitalocean.com/` |
| Repository variable, optional | `DO_IMAGE_REPOSITORY` | `workflow-service` by default; a single lowercase repository name |

Use a token belonging to the team that owns both resources. CI logs into DOCR
with credentials expiring after one hour. Application database/vault credentials
stay in App Platform, not in the build. See
[DigitalOcean tokens](https://docs.digitalocean.com/reference/api/create-personal-access-token/).
These resource-type scopes cover the team; they are not restricted to one
project. Renew expiring tokens and update the GitHub secret before expiry.

Create a GitHub environment named **production**. Later, set its `DO_APP_ID`
variable to the production app UUID and optionally `DEPLOY_URL` to the public
HTTPS URL. Keep `DO_APP_ID` environment-specific; do not put it at repository
scope, where staging could inherit production's ID. Environment reviewers are
optional: configuring them makes automatic releases wait for review.

Once these repository changes are on GitHub, run **Build and deploy App
Platform** manually on `main` with **deploy unchecked**. This runs all checks
and publishes the initial image without requiring an existing app. Copy the
release image from the workflow summary:

```text
registry.digitalocean.com/<registry>/workflow-service@sha256:<digest>
```

The registry also contains a tag equal to the workflow's full commit SHA.
Ordinary pushes to `main` deploy automatically after bootstrap; manual runs on
other branches only publish images.

## 3. Create the App Platform app once

In DigitalOcean, create an App Platform app from **DigitalOcean Container
Registry**, selecting the published repository and commit tag. Grant the app
access to the registry and disable registry auto-deploy. GitHub Actions is the
release controller. Configure the service as **web**, port **8080**, one
instance, using `.do/app.yaml` as the configuration template.
For this deployment, select the existing **shopxp** project.

The template app name is `weft` and region is `blr` (Bengaluru, India).
Use Bengaluru for DOCR as well. The Neon console currently offers Singapore
as the nearest database region for this deployment.
Its `image.repository` must match `DO_IMAGE_REPOSITORY`. Replace the template's
`REPLACE_WITH_FIRST_RELEASE_COMMIT_SHA` tag with the published SHA, or remove
`tag` and set `digest` to the published `sha256:…` value. DOCR uses the team's
primary registry with the `registry` field omitted.
[App spec reference](https://docs.digitalocean.com/products/app-platform/reference/app-spec/)

Set these **runtime** environment values in App Platform before launching:

| Variable | Value |
| --- | --- |
| `DATABASE_URL` (secret) | The direct PostgreSQL URL with verified TLS |
| `VAULT_MASTER_KEY` (secret) | Your saved 32-byte base64 key |
| `VAULT_KEY_VERSION` | `1` initially |
| `BOOTSTRAP_ADMIN_EMAIL` | Your administrator email |
| `BOOTSTRAP_ADMIN_PASSWORD` (secret) | A strong initial password |
| `APP_ENV` | `production` |
| `COOKIE_SECURE` | `true` |
| `PUBLIC_URL` | `${APP_URL}` initially, or your custom HTTPS origin |
| `API_RUNS_WORKER` | `true` |
| `MIGRATE_ON_START` | `true` |

The remaining sizing, worker and health-check settings are in the template.
Optional provider/OAuth variables can be configured now or later. Do not submit
the template's empty secret fields over an already-configured app: they are
placeholders, not a release configuration. For CLI creation, use a private,
completed copy of the spec with real runtime values and run
`doctl apps create --spec /private/path/app.yaml`; never commit that copy.

The distroless image starts `/app/api` through its Docker entrypoint. Leave
`run_command` unset. Startup migrations use a two-minute timeout and PostgreSQL
session lock, including during deployment overlap. The readiness check is
`/readyz`, with a two-minute initial delay. Container termination allows 60
seconds for the app's 30-second graceful shutdown.

After creation, copy the app UUID into **GitHub → Environments → production →
Variables → DO_APP_ID**. Open the app's HTTPS URL at `/ui/`, sign in and remove
**both** bootstrap email and password from the app once the admin exists.

## 4. Automatic releases and rollback

Each push to `main` runs the reusable CI (Go, integration, UI and container
acceptance), builds and pushes the image, then deploys production. The deploy
job retrieves the app's current spec, changes only `web.image` to the release
digest, disables registry auto-deploy, and calls `doctl apps update
--update-sources --wait`. Runtime secrets, domains, sizing and other settings
configured in DigitalOcean are preserved. Encrypted values are passed directly
over stdin; no app-spec artifacts are uploaded.
[doctl app updates](https://docs.digitalocean.com/reference/doctl/reference/apps/update/)

The workflow succeeds only when App Platform finishes the deployment. Check
`/readyz`, sign-in, a small workflow and its events after the first release.
Use App Platform's deployment and runtime logs to diagnose failures. If a CI
job times out, check the cloud deployment status before starting another one.

For rollback or a manual redeploy, run **Deploy production** with a retained
DOCR `image@sha256:digest`. Migrations remain forward-only and must be compatible
with the previous application image; a code rollback does not undo schema
changes. Releases are serialized per environment. CI updates images only:
apply subsequent runtime/spec changes through App Platform separately.

Optional staging uses a separate App Platform app and PostgreSQL database,
with distinct credentials and vault key. Create a **staging** GitHub environment
with its own `DO_APP_ID` and `DEPLOY_URL`, then run **Deploy staging** with the
same immutable image. There is no automatic staging release or Droplet `stop`
action in the App Platform workflows.

## Costs and operations

The template starts with one 512 MiB container. Confirm current
[App Platform pricing](https://docs.digitalocean.com/products/app-platform/details/pricing/)
and [registry plans](https://www.digitalocean.com/products/container-registry)
before choosing resources. Size the service from actual workload measurements.
The worker polls every second and readiness checks also touch PostgreSQL;
continuous operation keeps Neon active, so budget for always-on database compute
rather than assuming it will suspend. See [Neon compute behavior](https://neon.com/docs/manage/endpoints/).

App Platform does not run the repository's Docker Compose backup or Alloy
services. Configure database recovery/backups and cloud logs/alerts separately.
Read [the operations runbook](06-runbook.md) for restore and vault rotation.

## Retained Droplet tooling

`deploy/compose.yaml`, `bootstrap.sh`, `deploy.sh`, `env-deploy.sh`, `edge/`,
backup scripts and observability assets remain for self-hosting and CI's local
HTTPS/rolling-replacement/restore drill. The GitHub release workflows now target
App Platform and DOCR; they no longer publish GHCR images or upload files over
SSH. A Droplet operator must supply a compatible image and use the host scripts
manually. Removing that alternative and changing the acceptance drill can be a
separate cleanup.
