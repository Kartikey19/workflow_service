# Operations runbook

The cost-focused remote setup is described in [Deployment](07-deployment.md):
one API and worker, Neon, GHCR, Caddy, and separate staging/production
configuration. Staging is on demand and can be stopped independently. No cloud
resource or deployment has been created by the local tests.

## Local application

Install the Go version in `go.mod`, Node 24, Python 3, and Docker with Compose. From the
repository root:

```sh
make local-setup
make local-api
# In a second terminal in the same directory:
make local-worker
```

`local-setup` creates a private repository `.env` if it does not exist, starts
local PostgreSQL, installs/builds the embedded UI and applies migrations.
Existing `.env` values are preserved. Open http://localhost:8080/ui/ and sign in
using its `BOOTSTRAP_ADMIN_EMAIL` and `BOOTSTRAP_ADMIN_PASSWORD`. API routes
begin `/be/v1/`. Stop API and worker with Ctrl-C; `make local-db-stop` stops
PostgreSQL while retaining its volume. Run `make local-db` to restart it.

The first API creates the bootstrap administrator only when the user database
is empty. Remove the bootstrap password from remote environment files after
initial setup. The committed embedded frontend bundle supports Go-only builds;
Node is needed to rebuild frontend changes.

## Remote startup, release and rollback

Follow [the deployment setup checklist](07-deployment.md#1-create-the-neon-databases)
for Neon direct connections with verified TLS, private environment files,
GoDaddy records, the shared Caddy edge and GitHub environment secrets. The
remote configuration lives in `deploy/environments/staging/.env` and
`deploy/environments/production/.env`; do not use the local repository `.env`.
API/worker ports are unpublished. Start the shared `deploy/edge` stack once,
then deploy each application environment independently.

**Build release image** validates the project and publishes a digest to GHCR.
Run **Deploy staging** with that digest, verify sign-in/workflows/events, then
run **Deploy production** with the same digest. Production is manual and honors
configured environment reviewers. Run **Deploy staging**, action `stop`, when
testing ends so its Neon database can suspend. Auto staging deployment is off
unless the repository variable `STAGING_AUTO_DEPLOY=true` is explicitly set.

The equivalent host commands, from `/opt/workflow/deploy`, are:

```sh
./env-deploy.sh staging ghcr.io/<owner>/<repository>@sha256:<digest>
./env-deploy.sh production ghcr.io/<owner>/<repository>@sha256:<digest>
./env-deploy.sh staging stop
```

The script validates configuration, pulls the digest, runs migrations before
replacement and waits for API/worker readiness. A migration failure leaves the
application untouched. This inexpensive single-replica layout has a brief API
interruption during replacement; it does not provide rolling availability.
If readiness fails after replacement, the old successful digest remains in
`.env`, but containers may already run the failed image. Inspect the stack and
redeploy the previous compatible digest. Successful releases record the old
and new digests in the selected environment's `.release-history`.

Migrations must remain additive and compatible with the previous image. Roll
back **code** through the same workflow/script using a previous digest; do not
reverse deployed schema migrations. Verify sign-in, a small known run and its
events after every release. Execution can repeat external side effects after
failures; use connector idempotency and request Idempotency-Key where supported.
SSE clients reconnect/replay after API shutdown.

The remote template limits each pool to four connections and each worker to
four concurrent nodes initially. Dedicated database listeners also consume
connections. Tune capacity against Neon, upstream services and measured host
memory before scaling. Continuous polling/readiness keeps Neon active; a full
month at 0.25 CU exceeds Free's 100 CU-hour allowance. See the deployment guide's
[budget calculation](07-deployment.md#neon-free-and-always-on-workers).

### Original self-hosted PostgreSQL deployment

`deploy/compose.yaml`, `bootstrap.sh` and `deploy.sh` remain the larger baseline
and the target of the disposable local HTTPS/backup drill. They run local
PostgreSQL, two API replicas, workers and Caddy, with a separate private
`deploy/.env`. They are an alternative to the Neon setup, not the environment
workflows' runtime stack. `bootstrap.sh` initializes that file and database;
`deploy.sh <digest>` replaces API replicas in sequence and drains worker pools.
Do not start its Caddy beside `deploy/edge` on the same ports. The backup and
Alloy commands below apply to this original layout unless stated otherwise.

## Backups and restore

For Neon, decide recovery retention and off-host storage before production.
Use the direct endpoint and verified TLS for dumps/restores, with separate
staging/production storage prefixes and a compatible PostgreSQL client version.
The backup scripts accept standard `PG*` variables; the original Compose
backup profile below points at its local `postgres` service and requires
adaptation before use with Neon. Always recover into a separate empty database.

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
  and recent releases. For the single-instance Neon deployment, redeploy a
  compatible previous digest or repair/restart the API. In the larger baseline,
  keep the healthy replica serving while repairing another.
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
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
bash -n deploy/env-deploy.sh
python3 deploy/test-runtime.py
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
