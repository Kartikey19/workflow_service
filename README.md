# Weft

Weft is the product name; the repository and Go module are still called `workflow_service`.

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

Sign in at http://localhost:8080/ui/ using the bootstrap account. The bootstrap
admin is ensured on every API start, whether or not other users exist. It is
created if its email is missing. If the account already exists, its password is
never reset from the environment, but it is restored to platform admin,
re-enabled, unlocked, and given an organization if it has none. Keep
`BOOTSTRAP_ADMIN_EMAIL` set in every environment that should keep this admin.

Inviting people. Invites are links, not emails (nothing is sent automatically):

- **Teammates in an organization you belong to:** Team page, enter an email and
  role, then copy the link shown once and send it. They set a password on that
  page and join that organization.
- **A new customer organization:** sign in as the platform admin, open the
  Platform page, enter the owner's email and an organization name, and send the
  link. The invitee becomes owner of a new, empty organization. The same page
  lists organizations and changes their plan.

Invite links work once and expire after 7 days. To test as a customer, accept a
platform invite with a second email address (a private browser window avoids
replacing your admin session). Connect a model
provider (including a custom OpenAI-compatible one), create an agent, and build a
workflow on the visual canvas: add a step, type its details, and the canvas and checks
update as you type. Steps connect after the selected one; agents, saved connectors and
built-in steps are all steps. Keep related credentials and settings in a Vault secret
group (for example `open_api_settings` with `api_key`, `effort` and `temperature`) and
insert a key into any step as `${secrets.open_api_settings.api_key}`. The
assistant creates reviewable drafts using tenant resources; it never publishes
automatically.

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

Follow [the deployment guide](docs/07-deployment.md) for DigitalOcean App
Platform, Container Registry, Neon and the one-time GitHub settings. The default
release path is:

1. Configure `DIGITALOCEAN_ACCESS_TOKEN` and `DO_REGISTRY_NAME` in GitHub. Run
   **Build and deploy App Platform** with deployment disabled to publish the
   first image.
2. Create the App Platform app using [.do/app.yaml](.do/app.yaml), fill its
   runtime secrets privately, and set `DO_APP_ID` in GitHub's **production**
   environment.
3. Every push to `main` passes CI, builds the embedded UI/API/worker image,
   pushes it to DOCR and deploys its exact digest to App Platform. One container
   runs the API and worker and applies migrations at startup.
4. Use **Deploy production** with a previous retained DOCR digest for rollback.
   Optional **Deploy staging** targets a separate app and database.

[The operations runbook](docs/06-runbook.md) covers migrations,
rollback, backups and key rotation. The repository deployment setup is ready;
cloud resources and GitHub credentials still need the one-time setup. Real OAuth, SMTP, LLM, S3,
and Grafana Cloud account checks remain separate acceptance work. Preserve the
vault keyring with database backups, including historical keys needed by older
snapshots. Non-Anthropic pricing needs `LLM_PRICING_JSON`.
