# Operations runbook

Deployment files are a tested baseline, not a selected production hosting plan.
No remote host or account has been provisioned. Discuss the hosting provider,
region, budget, database placement, and recovery requirements before using them
in production. The same image can run on a Docker host regardless of provider.

## Local application

Install Go from `go.mod`, Node 24, and PostgreSQL 16. Copy `.env.example` to
`.env`; set the database URL, bootstrap account, and a persistent random vault
key. Keep `.env` private. Run:

```sh
make ui-install ui-build migrate
go run ./cmd/api
# In a second terminal with the same environment:
go run ./cmd/worker
```

Open http://localhost:8080/ui/ and sign in. The first API process creates the
bootstrap account only on an empty user database. API replicas serialize this
operation. Remove the bootstrap password from the environment after initial
setup. Node is needed for frontend changes; the committed embedded bundle lets
a Go-only checkout build and serve the UI. `make ui-check` verifies that bundle.

## Deployment prerequisites and first start

The proposed Compose layout has PostgreSQL, two API replicas, scalable workers,
and Caddy. Only Caddy publishes ports. There is no selected server size or
provider. For a host you have chosen, install Docker Engine and Compose v2.24+
(the local test needs `!override`), point the domain's DNS at it, allow inbound
80/TCP and 443/TCP+UDP, and clone this repository onto the host. Use an operator
account permitted to access Docker. If the GHCR image is private, authenticate
Docker on the host using a package read credential before starting.

From the repository root, the documented first-start command is:

```sh
./deploy/bootstrap.sh
```

It prompts for domain, email, image, and initial account password; writes a
private `deploy/.env` with random database credentials and a vault key; applies
migrations; and starts the application. The password prompt does not echo.
Caddy obtains a public certificate when DNS and port reachability are correct.
Open `https://<domain>` and verify sign-in, a workflow run, and its events.
Actual public certificate issuance and a fresh remote host remain unverified.

Store `deploy/.env` and every required vault key separately from the host and
from the database backup. A database dump alone cannot recover encrypted
secrets. Do not put this file in Git or print `docker compose config` with it;
use `docker compose config --quiet` for validation.

## Release and rollback

Build locally with `docker build -f deploy/Dockerfile -t workflow-service:test .`.
The **Build and optional production release** GitHub Actions workflow is manual.
Its default builds/pushes only; production deployment requires selecting its
`deploy` input and configuring the `production` environment. Configure protected
branches/reviewers as appropriate to the chosen release process. Required
secrets are `DEPLOY_HOST`, `DEPLOY_USER`, `DEPLOY_SSH_KEY`, and
`DEPLOY_KNOWN_HOSTS` (a host key verified through a trusted channel). The SSH user
must own `/opt/workflow/deploy`, access Docker, and have a prepared private
`.env`. Configuration uploads exclude `.env`. Private image pulls also require
host registry authentication. No workflow has been dispatched or image pushed
as part of local verification.

For a release initiated on the host:

```sh
cd deploy
WORKER_REPLICAS=2 ./deploy.sh ghcr.io/<owner>/<repository>@sha256:<digest>
```

Use an immutable image digest. The script migrates first, replaces one API at a
time, reloads Caddy, starts a temporary worker pool, replaces the normal pool,
and drains/removes the temporary pool. The successful image is recorded in
`.env`. During replacement Caddy retries GET/HEAD against a healthy replica;
mutation requests are never automatically replayed. Clients should supply
Idempotency-Key for retriable run creation and webhook requests. SSE clients
reconnect/replay after API shutdown.

Migrations must be additive and compatible with the previous image while it is
still running. To roll back code, pass the previous digest to the same script.
Do not roll back schema migrations. Check compatibility before rollback. Keep
at least one healthy API and enough workers during changes. A failed release
leaves already started replicas in place; inspect `docker compose ps`, resolve
the failure, and rerun using the intended digest.

Set worker replicas with `WORKER_REPLICAS` when invoking the script. Capacity is
bounded by database connections and upstream limits: the default pool is eight
connections per process, PostgreSQL permits 120, and replacement temporarily
adds a second worker pool. Leave room for migrations, SSE LISTEN connections,
Alloy scrapes, backups, and interactive administration. Tune PostgreSQL memory
and worker concurrency against the host selected later.

## Backups and restore

Configure a dedicated S3-compatible bucket/prefix and credentials in
`deploy/.env` using `deploy/env.example`. Enable the backup service:

```sh
cd deploy
docker compose --profile backups up -d --build backup
# Produce and verify a snapshot immediately:
docker compose --profile backups exec -T backup python3 /usr/local/bin/backup.py
```

The cron runs at 02:00 UTC. It creates a custom-format dump, records SHA-256,
uploads with server-side encryption and checksum metadata, verifies remote
length/metadata, and retains 14 days by default. Retention deletes only matching
`workflow-YYYYMMDDTHHMMSSZ.dump` objects under the explicitly configured prefix.
Local copies are retained in a private volume. Use least-privilege bucket
permissions scoped to this prefix and a bucket lifecycle policy as a second
retention mechanism. For versioned buckets, lifecycle rules must expire
noncurrent versions; deleting a key alone creates a delete marker.

`BACKUP_SSE=AES256` is the default. For a compatible KMS service use
`BACKUP_SSE=aws:kms` and `BACKUP_KMS_KEY_ID`. Endpoints that do not support the
chosen encryption mechanism must be configured/tested before enabling backups.
`BACKUP_LOCAL_ONLY=true` is available for isolated local tests; production
requires independent off-host copies. S3 credentials and Grafana credentials
have not been exercised against real accounts.

Create a new empty database for every restore drill, then run:

```sh
cd deploy
docker compose exec -T postgres createdb -U workflow workflow_restore_drill
docker compose --profile backups run --rm --no-deps \
  -e RESTORE_DATABASE=workflow_restore_drill backup \
  python3 /usr/local/bin/restore.py s3:workflow-YYYYMMDDTHHMMSSZ.dump
```

A local dump path under `/backups` also works. Restore verifies SHA-256 and uses
one transaction; it refuses the production database and any populated target.
Do not overwrite production. Bring up an isolated API against the restored
URL with the corresponding vault keyring, check users/workflows/runs, and verify
secret decryption (for example a signed webhook). Pause triggers and workers
before a recovery cutover, retain the old database, change DATABASE_URL, and
restart services. Decide whether pending runs may safely repeat external side
effects: execution is at least once, and recovery may replay work that happened
after the snapshot. Reconcile those effects using stable idempotency keys.

The local acceptance script exercises the actual snapshot/restore commands,
checks restored row counts, refuses a second restore into a populated database,
and verifies a signed webhook using the restored encrypted signing secret.
A production bucket and last-night operational backup still require a drill
once hosting/storage have been chosen. Monitor backup job completion separately;
application metrics do not currently report backup age.

## Observability

Set `GRAFANA_METRICS_URL`, `GRAFANA_METRICS_USER`, `GRAFANA_LOKI_URL`,
`GRAFANA_LOKI_USER`, and a scoped `GRAFANA_TOKEN` in `deploy/.env`, then:

```sh
cd deploy
docker compose --profile observability up -d alloy
```

Alloy discovers labeled containers, scrapes API and worker metrics, and ships
container logs. It is internal to the Docker network. The Docker socket mount
is privileged access to the daemon even when mounted read-only; only trusted
operator-managed images/configuration should receive it. Public Caddy requests
to `/metrics` return 404. Application logs record route patterns, not webhook
credential paths; Caddy access logging is disabled by default.

Import `deploy/observability/dashboard.json` into Grafana and select its
Prometheus datasource. Import `alerts.yaml` into the Prometheus-compatible
ruler, or recreate those rules in Grafana-managed alerting and assign a contact
point. The dashboard uses max for database-backed gauges that every process
reports, so replica counts do not multiply the queue. Alerts cover unavailable
APIs/workers, growing backlog, expired leases, and excessive run failures.
Test a notification end to end before relying on it. The Alloy configuration
has been validated with its container binary; cloud delivery is unverified.

## Vault rotation

Generate a new key and increase VAULT_KEY_VERSION. Add the previous
`version:base64` pair to VAULT_OLD_KEYS on **every API and worker replica** before
starting rotation. Deploy the full keyring, invoke `POST /be/v1/admin/vault/rotate`
using platform-admin authentication, and inspect `GET /be/v1/admin/vault` until the
old-version counts reach zero. Rotation reseals named secrets, LLM provider
keys, service connection credentials, and webhook signing secrets. Fenced
updates prevent a concurrent writer from being overwritten.

Keep old keys while old replicas or pre-rotation backups need them. Removing a
key after live counts reach zero does not make an older backup decryptable.
Test recovery with the keyring appropriate to the snapshot before discarding
historical keys under an agreed retention policy.

## Incident response

- API unavailable: inspect container health, PostgreSQL readiness, Caddy DNS/TLS,
  and recent releases. Keep the healthy replica serving while repairing another.
- Backlog growing: inspect `wf_ready_backlog`, active runs, worker health, node
  duration, and upstream rate limits. Scale workers only within database limits.
- Expired leases: investigate process crashes, database disconnects, and CPU
  pressure. Let fencing and the sweeper reclaim work; never force stale writes.
- Failed nodes: inspect run/node errors and provider connectivity in the UI.
  Cancel a run cooperatively, retry as a new run, or use platform node repair
  endpoints after evaluating external effects.
- Lost host/database: restore into a new database, recover the vault keyring,
  verify the isolated application, and deliberately switch traffic/workers.

## Verification commands

```sh
make ui-install ui-check
npx --prefix web playwright install chromium
make check                 # uses DATABASE_URL, MYSQL_URL and optional MONGO_URL for real tests
make ui-browser            # actual UI/API/worker/PostgreSQL stack
# Disposable containers/volumes and locally issued HTTPS certificate only:
docker build -f deploy/Dockerfile -t workflow-service:acceptance .
python3 deploy/test-local.py
```

The HTTPS drill uses a disposable local Caddy CA and disables verification only
inside its test client. It never changes the host trust store or provisions a
remote server. Containers and volumes are removed in its cleanup path.

Configuration references: [Compose startup order](https://docs.docker.com/compose/how-tos/startup-order),
[Caddy reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy),
and [Alloy Docker discovery](https://grafana.com/docs/alloy/latest/reference/components/discovery/discovery.docker/).
