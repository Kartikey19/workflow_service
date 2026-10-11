# workflow_service

Durable YAML workflows with PostgreSQL leasing/fencing, agents and tool loops,
human tasks, sub-workflows, fan-out/fan-in, webhooks, and a React web workspace.
Go 1.27; PostgreSQL 16; Node 24 for frontend builds. The API and workers scale
independently. No CLI or broker is required.

The frontend lives under `/ui/` and the HTTP API under `/be/v1`.

Start with [the documentation](docs/README.md), [workflow specification](docs/02-workflow-spec.md),
and [HTTP API](docs/04-api.md). Local setup and deployment packaging are in
[the operations runbook](docs/06-runbook.md).

For local development, prepare `.env` from `.env.example`, then:

```sh
make ui-install ui-build migrate
go run ./cmd/api
# In another terminal:
go run ./cmd/worker
```

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

The deployment files are provisional while production hosting is discussed.
No remote server has been provisioned or deployed. Real OAuth, SMTP, LLM, S3,
and Grafana Cloud account checks remain separate acceptance work. Preserve the
vault keyring with database backups, including historical keys needed by older
snapshots. Non-Anthropic pricing needs `LLM_PRICING_JSON`.
