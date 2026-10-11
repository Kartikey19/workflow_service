# Debug the support-ticket workflow

`support-ticket.yaml` creates a support ticket, then alerts on-call only when
priority is `high`. Both requests carry stable request-derived idempotency keys.
The example URLs are placeholders for local ticket/alert services on port 9090;
no such services are started by this example. A ticket service must return JSON
such as `{"ticket_id":"TKT-42"}` and honor the idempotency key.

## Debug the compiler now

Open the `workflow_service` folder in Cursor. The Go extension is recommended
in `.vscode/extensions.json`; Go and Delve must be installed.

1. Set a breakpoint at `Compile` in `internal/spec/compile.go`.
2. Select **Support ticket compiler** under Run and Debug, then press F5.
3. Step through decoding, defaults, validation, graph construction, and CEL compilation.
4. Break in `internal/spec/example_test.go` to inspect resolved configs and the
   alert condition for `normal` and `high` input.

This test reads the exact example YAML. It supplies the ticket output as a fixture;
it does not perform HTTP calls, publish a database version, or run workers.

## Debug publishing after implementing the API

The current server has only `/healthz` and `/readyz`. The following requests are
for the planned API in `docs/04-api.md` and currently return 404.

Keep required application/database settings in the project-root `.env`.
Select **Workflow API** and press F5; this configuration uses API mode and port
8081. Set a breakpoint in your publish handler and in `spec.Compile`.

From the project root, validate without saving:

```sh
curl -i -X POST 'http://127.0.0.1:8081/be/v1/workflows?dry_run=true' \
  -H 'Content-Type: application/x-yaml' \
  --data-binary @examples/support-ticket.yaml
```

Publish a saved version once persistence is implemented:

```sh
curl -i -X POST 'http://127.0.0.1:8081/be/v1/workflows' \
  -H 'Content-Type: application/x-yaml' \
  --data-binary @examples/support-ticket.yaml
```

In Postman use **Import → Raw text** with the cURL. If the body file is not
imported, choose **Body → raw**, paste the YAML, and keep the YAML content-type.

After the run API, worker, and HTTP executor exist, trigger with:

```sh
curl -i -X POST 'http://127.0.0.1:8081/be/v1/runs' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: support-REQ-1001' \
  --data '{"workflow":"support-ticket","input":{"request_id":"REQ-1001","customer_email":"customer@example.com","subject":"Payment failed","priority":"high"}}'
```

Use a new request ID and idempotency key for a new run. Reusing the same key and
body should return the existing run. API/worker execution remains unimplemented;
compiler success does not validate executor config or input-schema enforcement.
