# workflow_service

Durable YAML workflows with PostgreSQL leasing/fencing, agents and tool loops,
human tasks, sub-workflows, fan-out/fan-in, webhooks, and a React web workspace.
Go 1.27; PostgreSQL 16; Node 24 for frontend builds. The API and workers scale
independently. No CLI or broker is required.

The frontend lives under `/ui/` and the HTTP API under `/be/v1`.

Start with [the documentation](docs/README.md), [workflow specification](docs/02-workflow-spec.md),
and [HTTP API](docs/04-api.md). Local setup and deployment packaging are in
[the operations runbook](docs/06-runbook.md).

Local prerequisites are Go 1.27, Node 24/npm, Python 3 and Docker Compose v2.
From the repository root, run the first-time setup:

```sh
make local-setup
```

This creates a private, gitignored `.env` if one does not exist, starts PostgreSQL
16 on `127.0.0.1:55433` with its own persistent volume, installs/builds the embedded
React UI, and applies migrations. It generates a database password, bootstrap
admin password and vault key once; existing `.env` files are preserved. The
bootstrap email is `admin@localhost.test`; read `BOOTSTRAP_ADMIN_PASSWORD` in
`.env` for the password. Keep that file and the database volume together across
restarts. A local database is separate from the test databases and Neon.

Start the two processes in separate terminals, both from the repository root:

```sh
# Terminal 1: HTTP API and embedded React UI
make local-api

# Terminal 2: workflow worker (required to execute runs)
make local-worker
```

Open [the local UI](http://localhost:8080/ui/). Check the API and worker:

```sh
curl -fsS http://localhost:8080/readyz
curl -fsS http://localhost:9091/readyz
```

After a restart, run `make local-db`, then start both processes again. After code
updates, run `make ui-build migrate` before starting them. Stop each foreground
process with Ctrl+C; `make local-db-stop` stops PostgreSQL while retaining data.
Changing bootstrap credentials does not reset an existing user's password.

For an existing local/remote database, prepare `.env` from `.env.example`, set
`DATABASE_URL`, bootstrap credentials and a persistent `VAULT_MASTER_KEY`, then
run `make ui-install ui-build migrate` and start the same two processes. Skip
`make local-db` when using your own database. Shell environment values override
`.env`; unset stale shell values if they point to another environment. Local
setup permits private-address connectors for testing; remote environments keep
that permission disabled.

Sign in at http://localhost:8080/ui/ using the bootstrap account. Connect a model
provider, create an agent, and use its form to publish/run a workflow without
editing YAML. Build on the visual canvas with saved connector and agent steps, editable
dependencies and action forms. Advanced workflows have a YAML editor with live
compiler diagnostics. The assistant creates reviewable drafts using tenant
resources; it never publishes automatically.

The Connectors page configures PostgreSQL, MySQL, MongoDB, Gmail, Drive, Docs,
Sheets, Calendar, Slack, Notion, email and MCP accounts once. Workflows and agents
reuse their named connections and the same action implementations. Agents choose
explicit read/write permissions; analytics agents can use only read actions.

Verification uses real PostgreSQL/MySQL/MongoDB and scripted external services
(set `MONGO_URL` for MongoDB acceptance; CI supplies it):

```sh
make check
npx --prefix web playwright install chromium
make ui-browser
# Optional disposable container/HTTPS/backup/rolling-release acceptance:
docker build -f deploy/Dockerfile -t workflow-service:acceptance .
python3 deploy/test-local.py
```

Follow [the deployment guide](docs/07-deployment.md) for the cost comparison,
Neon setup, GoDaddy DNS, environment-specific secrets and the staging/production
CI/CD workflows. The release process is:

1. Prepare the host, separate Neon databases and private configuration for
   `staging` and `production`, following the guide's one-time setup.
2. Configure the two GitHub environments with their own deployment variables
   and SSH secrets; add production reviewers if your GitHub plan supports them.
3. Run **Build release image**. CI checks the code and publishes an immutable
   GHCR image digest with the React UI embedded in the API binary.
4. Run **Deploy staging** with that digest and verify sign-in, connectors and
   workflow execution. Run **Deploy production** with the same tested digest.
5. Use **Deploy staging → stop** after testing. For a code rollback, deploy the
   previous digest; database migrations remain forward-only.

[The operations runbook](docs/06-runbook.md) covers migrations,
rollback, backups and key rotation. Production hosting is prepared for review;
no remote server has been provisioned or deployed. Real OAuth, SMTP, LLM, S3,
and Grafana Cloud account checks remain separate acceptance work. Preserve the
vault keyring with database backups, including historical keys needed by older
snapshots. Non-Anthropic pricing needs `LLM_PRICING_JSON`.
