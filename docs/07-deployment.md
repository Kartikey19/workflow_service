# Deployment: low cost, separate environments

The prepared default is a small DigitalOcean Droplet, Neon PostgreSQL, GHCR
images, and Caddy for HTTPS. Production runs one API and one worker. Staging
has its own configuration and database and starts only when requested.
The React UI is already embedded in the Go API image, so it needs no separate
frontend hosting. This guide prepares the setup; no cloud resources have been
created or deployed by the local development work.

## Budget and hosting choice

Prices checked 11 October 2026, in USD before taxes, LLM calls, email, backup
storage and additional transfer:

| Option | Application hosting estimate | What you operate |
| --- | --- | --- |
| 1 GiB Basic Droplet | $6/month | One API, worker and Caddy; a demo starting point requiring memory measurements |
| 2 GiB Basic Droplet | $12/month | More room for production and temporary staging on one host |
| App Platform, API + worker | $10/month per active environment at 512 MiB each | DigitalOcean operates containers, ingress and TLS |
| App Platform, staging + production | $20/month while both are active at those minimum sizes | Separate apps and credentials |

These component totals are calculated from [Droplet pricing](https://www.digitalocean.com/pricing/droplets)
and [App Platform pricing](https://docs.digitalocean.com/products/app-platform/details/pricing/).
Start with a 2 GiB Droplet if staging will run alongside production. A 1 GiB
host can be evaluated for production alone; the application's memory limits
are starting settings, not measured capacity guarantees. Build images in CI,
so Node and Go build memory is never charged to the runtime host.

GHCR container image storage and bandwidth are currently free, including
private container images. This pipeline uses GHCR and retains immutable
digests for rollback. GitHub Actions minutes and build cache have their own
allowances. [GitHub billing](https://docs.github.com/en/billing/concepts/product-billing/github-packages)
DO Container Registry also has a free 500 MiB/one-repository tier, but image
history can outgrow it; its Basic tier is $5/month. It is optional here.
[DO registry plans](https://www.digitalocean.com/products/container-registry)

Functions do not run this existing long-lived queue worker and SSE API as a
drop-in deployment. App Platform can run the API service and worker using this
same image, with a migration job, but its deployment integration would need
an App Platform spec and API deployment job; the provided SSH workflows target
Droplets. Choose that managed option if reduced host administration is worth
its component pricing.

### Neon Free and always-on workers

Neon Free currently includes 100 CU-hours and 1 GB of database storage per
project. A continuously active 0.25 CU database consumes 180 CU-hours in a
30-day month (186 in 31 days), exceeding the free allowance. On Launch,
180 CU-hours at $0.106/CU-hour is about $19.08 in database compute, plus
storage/history and any other metered usage. This is a minimum-size estimate;
autoscaling can increase it. [Neon plan details](https://neon.com/docs/introduction/plans)
The storage allowance was increased in October.
[Neon's update](https://neon.com/blog/neon-free-plan-1-gb-per-project)

This engine normally polls every 200 ms. The remote template reduces polling
to 1 second and worker concurrency to four, but polling, sweeping and readiness
checks still keep the database active. Raising the poll interval slightly
does not produce five minutes of inactivity. Neon identifies periodic database
activity as a cause of continuous compute usage.
[Neon compute behavior](https://neon.com/docs/manage/endpoints/)
Keep staging stopped when testing ends. If the Neon organization uses a paid
plan, staging activity is also metered under that plan. Free Neon is suitable for local/demo
sessions and intermittent staging; it cannot be promised for this unchanged
always-on production queue. For continuous production, plan roughly $25–31/month
for a $6–12 Droplet plus minimum Launch compute before the other costs above.
Changing the worker to sleep/wake on demand would be a separate engine change.

## What is isolated

| Environment | Deployment | Runtime configuration | Database | Public URL |
| --- | --- | --- | --- | --- |
| Local | `dev/compose.yaml` + local Go processes | repository `.env` | local PostgreSQL | `http://localhost:8080/ui/` |
| Staging | Compose project `workflow-staging` | `deploy/environments/staging/.env` | separate Neon project and role | `https://staging.example.com/ui/` |
| Production | Compose project `workflow-production` | `deploy/environments/production/.env` | separate Neon project and role | `https://app.example.com/ui/` |

Both remote environments set `APP_ENV=production`, enforcing secure cookies
and disabling development tenant-header authentication. `DEPLOY_ENV` controls
the Compose project, service label and edge alias. Credentials, accounts,
connectors, OAuth registrations, bootstrap passwords and vault keys must be
different between staging and production. Never clone production connector
tokens into staging. Use fixtures or dedicated test accounts there.

The starter saves money by sharing one host and Caddy. Docker projects and
backend networks are separate; API containers join the common ingress network.
Host access, CPU, memory, disk and ingress remain shared. When customer traffic
warrants stronger isolation, assign staging a second host and keep the same
environment workflows and secrets. A single host has a single failure domain
and this one-replica layout has a short API interruption during replacement.

## 1. Create the Neon databases

In your existing Neon account:

1. Create separate `workflow-staging` and `workflow-production` projects, in a
   region near the selected Droplet. Start with the smallest compute and check
   each project's usage after testing.
2. Create a database named `workflow` and a distinct role in each project.
   The migration role needs schema creation privileges. This baseline uses that
   environment's role for migrations and runtime; split roles later if needed.
3. In **Connect**, select the project/branch/database/role and turn **connection
   pooling off**. Copy the direct PostgreSQL URL privately to its environment
   file; set `sslmode=verify-full`. Percent-encode any password URL characters.
4. The hostname must have no `-pooler` suffix. This application uses persistent
   `LISTEN` sessions and advisory locks; transaction pooling cannot preserve
   those sessions. The deployment validator rejects pooled URLs and weaker TLS.
   [Neon connection guidance](https://neon.com/docs/connect/connection-pooling)
5. Record the project/branch/role names and recovery retention. Start staging
   from an empty database or synthetic fixtures rather than production data.

Use the direct URL for **every** API, worker and migration process in that
environment. `DB_MAX_CONNS=4` and `DB_MIN_CONNS=0` limit each pool initially;
dedicated listeners also use connections. Measure the total before scaling.

## 2. Prepare the host and private configuration

After selecting and approving the host/region, install Docker Engine, Compose
v2.24+, Python 3, rsync, and util-linux (`flock`) on the Linux host. Configure a
non-root operator with Docker access and SSH keys. Allow 80/TCP and 443/TCP+UDP
for Caddy; restrict SSH to operators. Application/database ports are unpublished.
The deployment does not install PostgreSQL on this host.

Copy the repository's `deploy/` directory to `/opt/workflow/deploy`, owned by
the deployment operator. Do this once from a reviewed checkout:

```sh
# On the host, in /opt/workflow/deploy:
cp environments/staging/env.example environments/staging/.env
cp environments/production/env.example environments/production/.env
cp edge/env.example edge/.env
chmod 600 environments/staging/.env environments/production/.env edge/.env
```

Edit each environment file privately. Set its domain, direct Neon URL,
bootstrap email/password and a **different persistent vault key**. Generate
each key with `openssl rand -base64 32` and save it directly in your password
manager/environment file. Set `VAULT_KEY_VERSION=1`. Replace the example image
with the release digest produced by CI. Add LLM, OAuth and pricing settings as
needed; tenant LLM providers can also be configured through the UI. Back up
these vault keys separately from database backups. The files are ignored by
Git and excluded from workflow uploads.

For a private GHCR image, authenticate Docker on the host as the deployment
operator using a GitHub credential with `read:packages`; use `docker login
ghcr.io --username <github-user> --password-stdin`. The build's short-lived
`GITHUB_TOKEN` is used only by CI and is not stored on the host.

## 3. Point the GoDaddy domain when ready

Keep GoDaddy as registrar/DNS provider. In its DNS editor, add `A` records:
`app` and `staging` pointing to the Droplet's IPv4 address. An existing website
can keep its current root and `www` records. Add `AAAA` records only if this
host's IPv6 routing is configured. Set the exact hostnames and ACME contact in
`edge/.env`, and the matching `DOMAIN` in each application file.

Wait for DNS propagation and verify with `dig +short app.example.com` and
`dig +short staging.example.com`. Start the shared ingress once:

```sh
cd /opt/workflow/deploy/edge
docker compose config --quiet
docker compose up -d
```

Caddy creates the `workflow-edge` network and obtains HTTPS certificates when
DNS and ports are reachable. An environment without running containers returns
a gateway error at its hostname. Edge configuration is managed separately and
is excluded from releases. After an edge/domain edit, run `docker compose exec
-T caddy caddy reload --config /etc/caddy/Caddyfile` from this directory.
OAuth callback URLs are `https://<environment-domain>/be/v1/oauth/callback`.
UI routes begin `/ui/`; API routes begin `/be/v1/`; hooks use `/hooks/{token}`.

## 4. Configure GitHub environments

In repository **Settings → Environments**, create exactly `staging` and
`production`. Add the following **to each environment**, not as shared repository
credentials:

| Type | Name | Value |
| --- | --- | --- |
| Variable | `DEPLOY_HOST` | host IP or hostname |
| Variable | `DEPLOY_USER` | SSH/Docker operator |
| Variable | `DEPLOY_PATH` | `/opt/workflow/deploy` |
| Variable | `DEPLOY_URL` | that environment's `https://.../ui/` URL |
| Secret | `DEPLOY_SSH_KEY` | dedicated deployment private SSH key |
| Secret | `DEPLOY_KNOWN_HOSTS` | host public key verified through a trusted host console |

Restrict production deployment branches to `main` and configure required
reviewers where your GitHub plan supports them. Without environment reviewers,
production still requires an explicit manual workflow dispatch; access to that
dispatch is controlled by repository permissions. Keep `STAGING_AUTO_DEPLOY`
unset/false. It is an optional **repository variable** that enables staging
deployment after successful main-branch builds when set to `true`.

Database URLs and vault keys stay in the environment's private host `.env`.
Changing them is a separate operator configuration step. The CI upload cannot
overwrite these files. On the shared budget host, workflow concurrency and
`flock` serialize releases. Staging/production application settings and data
remain distinct.

## 5. Build once, test staging, promote production

1. Merge a reviewed change to `main`, or run **Build release image** manually
   against a reviewed ref. The workflow calls the full CI suite, builds the
   embedded UI and Go binaries, and pushes one Linux/amd64 image to GHCR.
   Main runs that CI through the build workflow, avoiding a second push CI run.
2. Copy the full `ghcr.io/<owner>/<repository>@sha256:<digest>` from its summary.
   This image includes both API and worker binaries. Environment configuration
   is injected at runtime; environment images are not rebuilt separately.
3. Run **Deploy staging**, action `deploy`, with that digest. The reusable
   deployment job uploads reviewed deployment code, validates the configuration,
   pulls the digest, runs migrations and waits for API/worker readiness.
4. Sign in at staging `/ui/`, create a workflow, run it, verify events, and test
   required connectors using dedicated test credentials.
5. Run **Deploy production** with the **same** digest. Its job uses production's
   environment variables/secrets and waits for configured reviewers. Sign in,
   run a small known workflow and verify events after deployment.
6. Run **Deploy staging**, action `stop`. This gracefully stops only staging's
   API and worker. It retains configuration/database/container definitions and
   leaves production/Caddy running. With staging activity stopped, Neon can
   suspend that project's compute. Do not keep external probes querying it.

Equivalent host commands:

```sh
cd /opt/workflow/deploy
./env-deploy.sh staging ghcr.io/<owner>/<repository>@sha256:<digest>
./env-deploy.sh production ghcr.io/<owner>/<repository>@sha256:<same-digest>
./env-deploy.sh staging stop
```

For direct Compose operations, set the environment and file explicitly first:

```sh
cd /opt/workflow/deploy
DEPLOY_ENV=production WORKFLOW_ENV_FILE="$PWD/environments/production/.env" \
  docker compose --project-name workflow-production \
  --env-file environments/production/.env -f compose.neon.yaml ps
```

Never print expanded Compose configuration containing secrets; use
`config --quiet`. The deployment script also strips inherited shell application
settings, so a shell's staging database URL cannot override production's file.

## Release failure, rollback and recovery

Migrations run before application replacement and must be forward-only,
additive and compatible with the previous image. A failed migration prevents
API/worker replacement. Health failure after replacement stops the release and
leaves the last successful digest recorded in `.env`, but the containers may
already be running the failed image; inspect them and redeploy that previous
digest if its schema compatibility is known.
The script records successful digests and their predecessors in the selected
environment's `.release-history`. It does not automatically undo schema or
external connector side effects.

Roll back code through the same environment workflow using the previous digest.
Retain published release images needed for recovery. After initial account
creation, remove `BOOTSTRAP_ADMIN_PASSWORD` from the private file and redeploy.
Keep all active/historical vault keys required for encrypted backups; see
[vault rotation and recovery](06-runbook.md#vault-rotation).

Neon's short Free restore history is useful for tests but is not a complete
backup plan. For production, choose restore retention and independent encrypted
off-host dumps based on your recovery requirements. The existing backup scripts
accept normal `PGHOST`, `PGUSER`, `PGPASSWORD`, `PGDATABASE` and `PGSSLMODE`
environment variables; use the direct Neon host, verified TLS, separate
environment prefixes and a compatible `pg_dump` version. The original backup
Compose profile targets local PostgreSQL, so it must not be used unchanged
against Neon. See [the runbook](06-runbook.md#backups-and-restore) for checksum,
empty-database restore and vault-key verification. A Neon backup destination
and production restore drill remain setup steps, not already deployed services.

## Verification before the first remote release

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
bash -n deploy/env-deploy.sh
python3 deploy/test-runtime.py
EDGE_ENV_FILE=env.example docker compose --env-file deploy/edge/env.example \
  -f deploy/edge/compose.yaml config --quiet
docker build -f deploy/Dockerfile -t workflow-service:acceptance .
python3 deploy/test-local.py
```

CI checks workflow syntax, the actual resolved environment Compose configs,
direct/TLS/vault validation, migration/health failures, environment-scoped
stopping, and Caddy configuration. The existing local HTTPS drill verifies
application packaging, replacement and backup recovery with disposable local
PostgreSQL. No live Neon, public certificate, SSH host or real cloud connector
account is contacted by these checks.
