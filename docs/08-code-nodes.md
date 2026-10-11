# 08 — Custom code in workflows (`code` node)

**Status:** scoped, not started (proposed milestone M20)
**Languages:** Python, TypeScript, Go
**Decision needed before build:** the sandbox backend (§5). Everything else here
holds whichever backend is chosen.

## 1. Why, and what it is not

Workflow authors keep hitting the edge of `transform` (pure CEL, no I/O) and
`http`: parse an awkward file, score a record, call a library, reshape a payload
in ten lines of real code. A `code` node lets an author write a small function in
Python, TypeScript or Go, give it JSON, and get JSON back, as one step of a
durable workflow.

It is **not**:

- A way to run long-lived services, servers or background daemons.
- A package manager. Version 1 has the language standard library only (§7).
- Shell access, and it has no filesystem that outlives the step.
- Trusted code. Every line is treated as hostile, because tenants write it
  (§6). This is the part that makes the feature hard, and most of this document
  is about it.

## 2. What the author writes

```yaml
- id: score_lead
  type: code
  needs: [fetch_lead]
  timeout: 30s                       # per attempt, as for every node
  config:
    language: python                 # python | typescript | go
    source: |
      def main(input):
          text = input["notes"].lower()
          return {"score": 10 * text.count("budget") + 5 * text.count("urgent")}
    input: ${nodes.fetch_lead.output}      # any JSON; passed to main()
    env:                                   # optional; strings, where secrets go
      CRM_TOKEN: ${secrets.CRM_TOKEN}
      LLM_KEY: ${secrets.open_api_settings.api_key}   # one key of a secret group
    network: none                          # none (default) | [api.example.com, ...]
    limits: {memory_mb: 256, cpu_seconds: 10}   # may only lower the plan's caps
```

Entry points, one per language, so the contract is the same everywhere: take one
JSON value, return one JSON value.

| Language | Entry point | Notes |
|---|---|---|
| Python 3.12 | `def main(input): ...` (sync or `async def`) | stdlib only |
| TypeScript | `export default async function main(input: unknown) { ... }` | type-stripped, then run on a JS runtime; types are not checked at run time |
| Go | `func Main(input map[string]any) (any, error)` in `package main` | wrapped by a generated `main` that reads and writes JSON; compiled per run (cached by source hash) |

Output of a successful node:

```json
{"result": <what main returned>, "logs": "<stdout+stderr, last 64 KiB>"}
```

It is an ordinary node output, so downstream steps read
`${nodes.score_lead.output.result.score}`.

The engine treats every string in `config` as a template, `source` included, so
a literal `${` in code (a TypeScript template literal, say) is written `$${`.
`ValidateConfig` rejects `secrets` expressions in `source`: a secret compiled
into the program would end up in the Go build cache, keyed by a hash of it. Put
secrets in `env`; `input` works too but mixes them into the data.

## 3. How it fits the engine

The node is one more `Executor` (`internal/core/executor/code`). Nothing in the
engine changes.

- **Types and registry.** `Type() == "code"`, with `Describe()` so the UI node
  catalog lists it.
- **Validation at publish.** `ValidateConfig` checks shape only (language is one
  of three, `source` non-empty and at most 64 KiB, `network` is `none` or a host
  list, `limits` are numbers). It must not do I/O, per the executor contract, so
  it cannot compile anything. A separate `POST /be/v1/code/check` endpoint, used
  by the editor and by the dry-run lint, runs syntax and, for Go, a real compile.
- **Config resolution.** The engine already resolves `${...}` in `config`,
  hands the executor real values and stores the node input with each vault value
  of 8 or more characters replaced by `***`. It also scrubs those values from the
  node's output and error, so `logs` and `result` are covered even if the code
  prints a secret; the executor adds nothing for this. (`Request.Redact` is for
  credentials an executor fetches itself, from a connection, and a code node
  fetches none.) Two limits follow from the engine's rules: a group key under 8
  characters, such as an effort, is a setting and is neither masked nor scrubbed;
  and scrubbing matches exact text, so code that encodes or slices a secret
  before printing it gets past it. Redaction protects the run record from
  accidents, not from the author, who could use the secret anyway.
- **Delivery semantics.** The engine is at-least-once, so a code node can run
  twice. `Request.IdempotencyKey` is passed to the program as
  `WORKFLOW_IDEMPOTENCY_KEY`; code that calls a non-idempotent API should forward it.
- **Size limits.** Input and result are each capped by the engine's
  `DefaultMaxPayloadBytes` (256 KiB). A larger result fails the node
  with `output_too_large` instead of being truncated, because a silently cut JSON
  document is worse than an error.
- **Error classification** (this decides retries):

  | Situation | Error | Retried |
  |---|---|---|
  | The author's code raises, panics or exits non-zero | `PermanentError{code_error}` | no |
  | Result is not valid JSON, or too large | `PermanentError{invalid_output}` | no |
  | Out of memory or process limit | `PermanentError{limit_exceeded}` | no |
  | Wall-clock timeout | `RetryableError{timeout}` | per `retry:` |
  | Runner or sandbox unavailable, 5xx | `RetryableError{runner_unavailable}` | per `retry:` |

- **Fencing.** A stale worker's zero-row result write is discarded as usual. A
  sandbox that is still running for a lost lease is killed when its request context
  is cancelled; the runner must also enforce its own hard deadline so an orphaned
  sandbox cannot outlive the lease.

## 4. The runner boundary

The worker never executes user code itself. It calls a `Runner` through a small
interface in `internal/core/executor/code`, so the backend can change without
touching the executor:

```go
type Runner interface {
    Run(ctx context.Context, job Job) (Outcome, error)
}
type Job struct {
    Language string          // python | typescript | go
    Source   string
    Input    json.RawMessage
    Env      map[string]string
    Network  []string        // empty means none
    Limits   Limits          // memory, cpu seconds, wall seconds, output bytes
    TenantID uuid.UUID       // isolation key; never share a sandbox across tenants
    JobID    string          // idempotency key; lets the runner dedupe a retried call
}
```

Config: `CODE_RUNNER=disabled|docker|http`, default `disabled`. Production refuses
`docker` (config validation, like `COOKIE_SECURE`). A disabled runner makes
publishing a workflow with a `code` node fail with a clear message, so the
feature cannot half-work.

## 5. Where the code runs (the decision)

The hosting target matters. DigitalOcean App Platform containers run
unprivileged and have no Docker socket, and the platform has no volumes (a DigitalOcean
staff reply in the community forum; the platform [limits page](https://docs.digitalocean.com/products/app-platform/details/limits/)
lists the 2 GiB ephemeral disk and no volumes). A worker on App Platform therefore
**cannot start nested containers**, so user code must run somewhere else.

| Option | Isolation | Languages | Ops burden | Notes |
|---|---|---|---|---|
| **A. Hosted microVM sandbox API** (E2B, Modal, Daytona and similar) | Strong: VM per run | all three | none | Pay per second; code and data leave your infrastructure; check each vendor's terms and pricing before committing. |
| **B. Your own runner on a Droplet** (Docker with gVisor `runsc`, or Firecracker) | Strong if configured well | all three | high: patching, images, scaling | Fixed cost; full control and data residency. Needs a second host and a private link from the workers. |
| **C. DigitalOcean Functions** | Medium: warm containers are reused and a namespace is shared, so cross-tenant residue is possible | Python, Node, Go (verify current runtime list) | low | Limits per the [Functions docs](https://docs.digitalocean.com/products/functions/details/limits/): 15 min timeout, 128 MB–1 GB, 1 MB payload each way, 120 concurrent and 600 invocations a minute per namespace. Go and TypeScript cannot be compiled per run. Reasonable only for code your own team writes. |
| **D. In-process WebAssembly** (wazero) | Strong, no extra host | TypeScript/JS via a QuickJS build, maybe Python | none | wazero has page-based memory caps and context deadlines, and needs no cgo. Go cannot run this way, since compiling Go needs the toolchain, and Python on WASI is heavy and limited. A good later fit for tiny expression-style snippets. |
| **E. Deno subprocess** | Weak alone | TypeScript only | low | [Deno permissions](https://docs.deno.com/runtime/fundamentals/security) block file, network and env access but do not cap memory or CPU, and Deno itself advises an OS or microVM sandbox for fully untrusted code. |

**Recommendation.** Build against the `Runner` interface and ship **A** first. It is
the only option that gives microVM isolation for all three languages without
running a second fleet, which is the right trade for a small team. Add the
`docker` backend for local development only (flagged unsafe for multi-tenant), and
revisit **B** when volume makes per-second pricing expensive or a customer needs
data to stay on your infrastructure. Use **C** only for first-party snippets if at
all.

## 6. Threat model and required controls

Assume the author is hostile and the code is adversarial.

| Threat | Control |
|---|---|
| Escape to the host | microVM or gVisor boundary; non-root user, no capabilities, seccomp, read-only root filesystem, no host mounts. |
| Reading another tenant's data | A fresh sandbox per run, never reused across tenants; temp storage destroyed afterwards; the runner never sees any tenant's other secrets. |
| Reaching internal services (the database, the metadata address, this API) | `network: none` by default. When hosts are listed, traffic goes through an egress proxy that applies the same rules as `pkg/netguard` (private, loopback, link-local and metadata ranges blocked, checked at dial time) plus the host allowlist. |
| Stealing secrets | Only the secrets the step's own config refers to (in `env` or `input`) exist inside the sandbox; the vault is never mounted, and the worker never passes the rest of the organization's secrets. |
| CPU, memory, disk, process or output exhaustion | Limits per run (memory, CPU seconds, wall time, pids, output bytes), capped by plan; a per-tenant concurrency limit so one tenant cannot fill the runner. |
| Mining or abuse | CPU-second metering per organization, a monthly quota per plan, and a platform-admin kill switch for an organization. |
| Tampering with results | The result is parsed as JSON by the worker and size-checked before it is stored. |
| Evidence after an incident | The node execution records the language, the SHA-256 of the source, limits, runner job id and duration. Source itself stays in the workflow version. |

Policy defaults: the node is off for every organization until an owner turns on
`code_enabled`; it is unavailable on the Free plan; only editors and above can
publish a workflow containing it.

## 7. Dependencies (explicitly deferred)

Version 1 offers each language's standard library only. Real use will ask for
`requests`, `pandas`, npm packages and Go modules. Each of those adds supply-chain
and network risk and a build step. The path later is a curated, pinned set baked
into the runner images, then maybe per-organization lockfiles. It is deliberately
out of scope for M20.

## 8. Limits (initial)

| Limit | Default | Maximum (Team plan) |
|---|---|---|
| Source size | 64 KiB | 64 KiB |
| Input / result size | 256 KiB | 256 KiB |
| Wall time (`timeout:`) | 30 s | 5 min |
| CPU seconds | 10 | 120 |
| Memory | 256 MB | 1 GB |
| Log capture | 64 KiB | 64 KiB |
| Concurrent code nodes per organization | 4 | 16 |

Plan rows live in the `plans` table, so the numbers change without a deploy, as
for the existing limits.

## 9. Milestones

**C1 — Contract and plumbing (3 days).** The `code` executor, the `Runner`
interface, a scripted fake runner, `ValidateConfig`, `Describe`, config flags,
plan gating, error mapping, redaction, size limits, and the `code_enabled` switch.
*Accepted when:* a workflow with a `code` node publishes and runs against the fake
runner; every row of the §3 error table is covered by a test; a secret printed by
the code never appears in stored output; a `secrets` expression in `source` is
rejected at publish.

**C2 — Local Docker backend (3–4 days).** One image per language, no network,
read-only root, memory, cpu and pids limits, kill on deadline. Development only.
*Accepted when, per language:* hello-world round-trips JSON; an infinite loop times
out and the container is removed; a memory bomb returns `limit_exceeded`; a fork
bomb is contained; writing outside the temp directory fails; an outbound connection
fails; printing 10 MB is truncated to the log cap.

**C3 — Production runner (3–4 days).** The HTTP backend for the chosen option, the
job protocol, the egress proxy for allowlisted hosts, per-tenant concurrency.
*Accepted when:* all C2 cases pass against staging; a second tenant's job cannot
see the first one's files or environment; an allowlisted host is reachable and the
metadata address is not; a killed worker's sandbox is gone within the lease TTL.

**C4 — Editor experience (2–3 days).** A code step in the visual builder with a
syntax-highlighting editor, `POST /be/v1/code/check`, and a logs panel on the run
detail page. *Accepted when* a browser test writes a Python step, sees a syntax
error inline, fixes it, runs the workflow, and reads the logs.

**C5 — Metering (2 days).** CPU-second usage per organization and month, quota
enforcement by plan, a usage line on the Usage page, and the admin kill switch.

## 10. Open questions

1. **Backend:** is A (hosted microVM API) acceptable for data leaving your
   infrastructure? If not, plan for B.
2. **Network from code:** is `none` plus an allowlist enough, or do authors
   need open internet access? Open access needs the egress proxy anyway.
3. **Packages:** is a standard-library-only first release acceptable?
4. **Who may author:** editors and above, or admins only?
5. **Billing:** does code time need its own price, or is it covered by the plan's
   run allowance?
