import { connectorActions } from "./connectors";
import { useState } from "react";
import { api, enc, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  AsyncButton,
  Empty,
  ResourceState,
  OneTime,
  Status,
  JSONView,
} from "./components";
const roles = (
  <>
    <option>viewer</option>
    <option>editor</option>
    <option>admin</option>
  </>
);
export function Connections({ admin }: { admin: boolean }) {
  const list = useResource("/connections"),
    providers = useResource("/providers"),
    kinds = useResource("/integrations");
  const [kind, setKind] = useState("smtp"),
    [form, setForm] = useState(false),
    [result, setResult] = useState<RecordData | null>(null),
    [editing, edit] = useState<RecordData | null>(null);
  const descriptor = kinds.data?.items.find((k: RecordData) => k.kind === kind);
  const oauth = new URLSearchParams(location.search);
  return (
    <>
      {oauth.get("error") && (
        <p className="error" role="alert">
          {oauth.get("error")}
        </p>
      )}
      {oauth.get("connected") && (
        <p className="success" role="status">
          Connected {oauth.get("connected")}
        </p>
      )}
      <div className="toolbar">
        <p className="muted">
          Configure databases and services once. Reuse their actions in
          workflows and agents. Credentials stay in your vault.
        </p>
        {admin && (
          <button
            className="primary"
            onClick={() => {
              setForm(!form);
              edit(null);
            }}
          >
            Add connector
          </button>
        )}
      </div>
      <ResourceState
        error={list.error || providers.error}
        loading={!list.data || !providers.data}
      />
      <div className="cards">
        {[
          ...(list.data?.items || []),
          ...(providers.data?.items || []).map((p: RecordData) => ({
            ...p,
            kind: p.provider,
            provider: true,
          })),
        ].map((c: RecordData) => (
          <article className="panel card" key={c.id}>
            <span className="eyebrow">{c.kind}</span>
            <h2>{c.name}</h2>
            <small>
              {c.account ||
                c.secret_hint ||
                c.base_url ||
                "Credentials encrypted"}
            </small>
            {!c.provider && (
              <div className="connector-actions">
                {connectorActions(kinds.data?.items, c.kind).map((action) => (
                  <small key={action.name}>
                    {action.title} · {action.read_only ? "Read only" : "Writes"}
                  </small>
                ))}
              </div>
            )}
            <div className="row">
              {admin && (
                <>
                  <AsyncButton
                    run={async () =>
                      setResult(
                        await api(
                          c.provider
                            ? `/providers/${c.id}/test`
                            : `/connections/${enc(c.name)}/test`,
                          "POST",
                        ),
                      )
                    }
                  >
                    Test connection
                  </AsyncButton>
                  {!c.provider &&
                    !kinds.data?.items.find(
                      (k: RecordData) => k.kind === c.kind,
                    )?.oauth && (
                      <button
                        onClick={() => {
                          edit(c);
                          setKind(c.kind);
                          setForm(true);
                        }}
                      >
                        Edit settings
                      </button>
                    )}
                  <AsyncButton
                    danger
                    run={async () => {
                      if (confirm(`Delete ${c.name}?`)) {
                        await api(
                          c.provider
                            ? `/providers/${c.id}`
                            : `/connections/${enc(c.name)}`,
                          "DELETE",
                        );
                        list.reload();
                        providers.reload();
                      }
                    }}
                  >
                    Delete
                  </AsyncButton>
                </>
              )}
            </div>
          </article>
        ))}
      </div>
      {list.data &&
        providers.data &&
        !list.data.items.length &&
        !providers.data.items.length && (
          <Empty>Add a provider or service connection to get started.</Empty>
        )}
      {result && (
        <section className="notice">
          <div className="row spread">
            <h3>Connection test passed</h3>
            <button onClick={() => setResult(null)}>Dismiss</button>
          </div>
          <JSONView value={result} />
        </section>
      )}
      {form && admin && (
        <section className="panel">
          <div className="row spread">
            <h2>{editing ? `Edit ${editing.name}` : "Add connector"}</h2>
            <button onClick={() => setForm(false)}>Close</button>
          </div>
          <Field label="Integration">
            <select
              value={kind}
              disabled={!!editing}
              onChange={(e) => setKind(e.target.value)}
            >
              {kinds.data?.items.map((k: RecordData) => (
                <option key={k.kind} value={k.kind}>
                  {k.title}
                </option>
              ))}
              <option value="llm">Language model provider</option>
            </select>
          </Field>
          <ActionForm
            key={`${kind}-${editing?.name || ""}`}
            label={descriptor?.oauth ? "Connect account" : "Test and save"}
            submit={async (data) => {
              const name = String(data.get("name"));
              if (descriptor?.oauth) {
                const r = await api("/connections/oauth/start", "POST", {
                  kind,
                  name,
                });
                window.location.assign(r.authorize_url);
                return;
              }
              if (kind === "llm") {
                await api("/providers", "POST", {
                  name,
                  provider: data.get("provider"),
                  api_key: data.get("api_key"),
                  base_url: data.get("base_url"),
                });
              } else {
                const config: RecordData = {},
                  secret: Record<string, string> = {};
                descriptor?.fields.forEach((f: RecordData) => {
                  const v = data.get(f.name);
                  if (v === null || v === "") return;
                  if (f.secret) secret[f.name] = String(v);
                  else
                    config[f.name] =
                      f.type === "int"
                        ? Number(v)
                        : f.type === "bool"
                          ? v === "true"
                          : v;
                });
                await api(
                  editing
                    ? `/connections/${enc(editing.name)}`
                    : "/connections",
                  editing ? "PUT" : "POST",
                  { name, kind, config, secret },
                );
              }
              setForm(false);
              edit(null);
              list.reload();
              providers.reload();
            }}
          >
            <Field label="Connection name">
              <input
                name="name"
                required
                defaultValue={editing?.name}
                readOnly={!!editing}
                placeholder="my-connection"
              />
            </Field>
            {kind === "llm" ? (
              <>
                <Field label="Provider">
                  <select name="provider">
                    <option>anthropic</option>
                    <option>openai</option>
                    <option>gemini</option>
                    <option>openai_compatible</option>
                  </select>
                </Field>
                <Field label="API key">
                  <input
                    name="api_key"
                    type="password"
                    required
                    autoComplete="off"
                  />
                </Field>
                <Field
                  label="Base URL"
                  hint="Required only for an OpenAI-compatible provider."
                >
                  <input name="base_url" type="url" />
                </Field>
              </>
            ) : (
              descriptor?.fields?.map((f: RecordData) => (
                <Field
                  key={f.name}
                  label={f.title}
                  hint={
                    f.secret && editing
                      ? "Leave blank to retain the saved credential."
                      : f.help
                  }
                >
                  {f.type === "bool" || f.type === "enum" ? (
                    <select
                      name={f.name}
                      defaultValue={String(
                        editing?.config?.[f.name] ?? f.default ?? "",
                      )}
                    >
                      <option value="">Default</option>
                      {(f.type === "bool" ? ["true", "false"] : f.enum).map(
                        (v: string) => (
                          <option key={v}>{v}</option>
                        ),
                      )}
                    </select>
                  ) : (
                    <input
                      name={f.name}
                      type={
                        f.secret
                          ? "password"
                          : f.type === "int"
                            ? "number"
                            : "text"
                      }
                      required={f.required && !(f.secret && editing)}
                      defaultValue={
                        f.secret
                          ? ""
                          : (editing?.config?.[f.name] ?? f.default ?? "")
                      }
                      autoComplete={f.secret ? "off" : undefined}
                    />
                  )}
                </Field>
              ))
            )}
          </ActionForm>
        </section>
      )}
    </>
  );
}
export function Vault({ admin }: { admin: boolean }) {
  const list = useResource("/secrets");
  const [form, setForm] = useState(false);
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Write-only secrets available to your workflow nodes.
        </p>
        {admin && (
          <button className="primary" onClick={() => setForm(!form)}>
            Add or replace secret
          </button>
        )}
      </div>
      <ResourceState error={list.error} loading={!list.data} />
      <section className="panel">
        {list.data?.items?.map((s: RecordData) => (
          <div className="resource-row" key={s.name}>
            <strong className="mono">{s.name}</strong>
            <small>Key version {s.key_version}</small>
            {admin && (
              <AsyncButton
                danger
                run={async () => {
                  if (confirm(`Delete secret ${s.name}?`)) {
                    await api(`/secrets/${enc(s.name)}`, "DELETE");
                    list.reload();
                  }
                }}
              >
                Delete
              </AsyncButton>
            )}
          </div>
        ))}
        {list.data && !list.data.items.length && (
          <Empty>Your vault is empty.</Empty>
        )}
      </section>
      {form && admin && (
        <section className="panel">
          <h2>Add or replace secret</h2>
          <ActionForm
            submit={async (data) => {
              await api(`/secrets/${enc(String(data.get("name")))}`, "PUT", {
                value: data.get("value"),
              });
              setForm(false);
              list.reload();
            }}
          >
            <Field label="Secret name">
              <input name="name" required pattern="[A-Za-z_][A-Za-z0-9_]*" />
            </Field>
            <Field label="Value">
              <input name="value" required type="password" autoComplete="off" />
            </Field>
          </ActionForm>
        </section>
      )}
    </>
  );
}
export function Webhooks({ admin }: { admin: boolean }) {
  const list = useResource("/triggers"),
    workflows = useResource("/workflows");
  const [form, setForm] = useState(false),
    [created, setCreated] = useState<RecordData | null>(null);
  function expose(r: RecordData) {
    setCreated({
      url: `${location.origin}/hooks/${r.token}`,
      ...(r.signing_secret
        ? {
            signing_secret: r.signing_secret,
            signature_header:
              "X-Workflow-Signature: sha256=<HMAC-SHA256 of raw request body>",
          }
        : {}),
    });
  }
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Start workflows from external events. Send JSON and an optional
          Idempotency-Key.
        </p>
        {admin && (
          <button className="primary" onClick={() => setForm(!form)}>
            New webhook
          </button>
        )}
      </div>
      {created && (
        <OneTime
          title="Webhook credentials"
          value={created}
          close={() => setCreated(null)}
        />
      )}
      <ResourceState error={list.error} loading={!list.data} />
      <div className="cards">
        {list.data?.items?.map((t: RecordData) => (
          <article className="panel card" key={t.id}>
            <span className="eyebrow">
              {t.signed ? "HMAC SIGNED" : "TOKEN AUTHENTICATED"}
            </span>
            <h2>{t.name}</h2>
            <p>
              {t.workflow} · {t.input_mode}
            </p>
            <Status value={t.enabled ? "enabled" : "disabled"} />
            {admin && (
              <div className="row">
                <AsyncButton
                  run={async () => {
                    await api(`/triggers/${t.id}`, "PATCH", {
                      enabled: !t.enabled,
                    });
                    list.reload();
                  }}
                >
                  {t.enabled ? "Disable" : "Enable"}
                </AsyncButton>
                <AsyncButton
                  run={async () => {
                    if (confirm("Rotate token? The old URL will stop working."))
                      expose(
                        await api(`/triggers/${t.id}/rotate-token`, "POST"),
                      );
                  }}
                >
                  Rotate token
                </AsyncButton>
                <AsyncButton
                  danger
                  run={async () => {
                    if (confirm(`Delete ${t.name}?`)) {
                      await api(`/triggers/${t.id}`, "DELETE");
                      list.reload();
                    }
                  }}
                >
                  Delete
                </AsyncButton>
              </div>
            )}
          </article>
        ))}
      </div>
      {list.data && !list.data.items.length && <Empty>No webhooks yet.</Empty>}
      {form && admin && (
        <section className="panel">
          <h2>Create webhook</h2>
          <ActionForm
            label="Create webhook"
            submit={async (data) => {
              expose(
                await api("/triggers", "POST", {
                  name: data.get("name"),
                  workflow: data.get("workflow"),
                  input_mode: data.get("input_mode"),
                  signed: data.get("signed") === "on",
                }),
              );
              setForm(false);
              list.reload();
            }}
          >
            <Field label="Webhook name">
              <input name="name" required />
            </Field>
            <Field label="Workflow">
              <select name="workflow" required>
                <option value="">Choose…</option>
                {workflows.data?.items.map((w: RecordData) => (
                  <option key={w.name}>{w.name}</option>
                ))}
              </select>
            </Field>
            <Field label="Input mode">
              <select name="input_mode">
                <option value="body">JSON body</option>
                <option value="envelope">Envelope: body, headers, query</option>
              </select>
            </Field>
            <label className="check">
              <input type="checkbox" name="signed" defaultChecked />
              Require HMAC signature
            </label>
          </ActionForm>
        </section>
      )}
    </>
  );
}
export function Team({ admin, selfID }: { admin: boolean; selfID: string }) {
  const members = useResource("/members"),
    invites = useResource(admin ? "/invites" : null);
  const [created, setCreated] = useState<RecordData | null>(null);
  const [roleError, setRoleError] = useState("");
  return (
    <>
      {created && (
        <OneTime
          title="Invite link"
          value={{
            accept_url:
              created.accept_url ||
              `${location.origin}/ui/accept-invite?token=${created.token}`,
          }}
          close={() => setCreated(null)}
        />
      )}
      <ResourceState error={members.error} loading={!members.data} />
      <section className="panel">
        <h2>Members</h2>
        {roleError && (
          <p className="error" role="alert">
            {roleError}
          </p>
        )}
        {members.data?.items?.map((m: RecordData) => (
          <div className="resource-row" key={m.user_id}>
            <div>
              <strong>{m.name || m.email}</strong>
              <small>{m.email}</small>
            </div>
            {admin ? (
              <select
                aria-label={`Role for ${m.email}`}
                value={m.role}
                onChange={async (e) => {
                  try {
                    await api(`/members/${m.user_id}`, "PATCH", {
                      role: e.target.value,
                    });
                    members.reload();
                  } catch (e) {
                    setRoleError((e as Error).message);
                  }
                }}
              >
                {roles}
                <option>owner</option>
              </select>
            ) : (
              <span>{m.role}</span>
            )}
            {(admin || m.user_id === selfID) && (
              <AsyncButton
                danger
                run={async () => {
                  if (confirm(`Remove ${m.email} from this organization?`)) {
                    await api(`/members/${m.user_id}`, "DELETE");
                    members.reload();
                    if (m.user_id === selfID) location.reload();
                  }
                }}
              >
                {m.user_id === selfID ? "Leave" : "Remove"}
              </AsyncButton>
            )}
          </div>
        ))}
      </section>
      {admin && (
        <div className="split">
          <section className="panel">
            <h2>Invite a teammate</h2>
            <ActionForm
              label="Create invite"
              submit={async (data) => {
                setCreated(
                  await api("/invites", "POST", {
                    email: data.get("email"),
                    role: data.get("role"),
                  }),
                );
                invites.reload();
              }}
            >
              <Field label="Email">
                <input type="email" name="email" required />
              </Field>
              <Field label="Role">
                <select name="role" defaultValue="editor">
                  {roles}
                </select>
              </Field>
            </ActionForm>
          </section>
          <section className="panel">
            <h2>Invites</h2>
            <ResourceState error={invites.error} loading={!invites.data} />
            {invites.data?.items?.map((i: RecordData) => (
              <div className="resource-row" key={i.id}>
                <div>
                  <strong>{i.email}</strong>
                  <small>
                    {i.role} · expires{" "}
                    {new Date(i.expires_at).toLocaleDateString()}
                  </small>
                </div>
                <AsyncButton
                  danger
                  run={async () => {
                    await api(`/invites/${i.id}`, "DELETE");
                    invites.reload();
                  }}
                >
                  Revoke
                </AsyncButton>
              </div>
            ))}
          </section>
        </div>
      )}
    </>
  );
}
export function APIKeys() {
  const list = useResource("/api-keys");
  const [created, setCreated] = useState<RecordData | null>(null);
  return (
    <>
      {created && (
        <OneTime
          title="API key"
          value={{ token: created.token }}
          close={() => setCreated(null)}
        />
      )}
      <div className="split">
        <section className="panel">
          <h2>API keys</h2>
          <ResourceState error={list.error} loading={!list.data} />
          {list.data?.items?.map((k: RecordData) => (
            <div className="resource-row" key={k.id}>
              <div>
                <strong>{k.name}</strong>
                <small>
                  {k.prefix}… · {k.role}
                </small>
              </div>
              <AsyncButton
                danger
                run={async () => {
                  if (confirm(`Revoke ${k.name}?`)) {
                    await api(`/api-keys/${k.id}`, "DELETE");
                    list.reload();
                  }
                }}
              >
                Revoke
              </AsyncButton>
            </div>
          ))}
        </section>
        <section className="panel">
          <h2>Create API key</h2>
          <ActionForm
            label="Create key"
            submit={async (data) => {
              setCreated(
                await api("/api-keys", "POST", {
                  name: data.get("name"),
                  role: data.get("role"),
                }),
              );
              list.reload();
            }}
          >
            <Field label="Name">
              <input name="name" required />
            </Field>
            <Field label="Role">
              <select name="role" defaultValue="editor">
                {roles}
              </select>
            </Field>
          </ActionForm>
        </section>
      </div>
    </>
  );
}
export function Usage() {
  const usage = useResource("/usage"),
    plans = useResource("/plans");
  return (
    <>
      <ResourceState error={usage.error} loading={!usage.data} />
      {usage.data && (
        <section className="panel">
          <span className="eyebrow">CURRENT PLAN</span>
          <h2>{usage.data.plan.name}</h2>
          <div className="split">
            {[
              [
                "Workflows",
                usage.data.usage.workflows,
                usage.data.plan.limits.workflows,
              ],
              [
                "Runs this month",
                usage.data.usage.runs_this_month,
                usage.data.plan.limits.runs_per_month,
              ],
            ].map(([name, used, limit]) => (
              <div className="stat" key={name}>
                <span>{name}</span>
                <strong>
                  {used}{" "}
                  <small>
                    / {limit == null || Number(limit) < 0 ? "Unlimited" : limit}
                  </small>
                </strong>
                {Number(limit) > 0 && (
                  <progress value={Number(used)} max={Number(limit)} />
                )}
              </div>
            ))}
          </div>
        </section>
      )}
      <h2>Available plans</h2>
      <ResourceState error={plans.error} loading={!plans.data} />
      <div className="cards">
        {plans.data?.items?.map((p: RecordData) => (
          <article className="panel card" key={p.id}>
            <h2>{p.name}</h2>
            {Object.entries(p.limits).map(([key, value]) => (
              <p key={key}>
                {key.replaceAll("_", " ")}{" "}
                <strong>
                  {value == null || Number(value) < 0
                    ? "Unlimited"
                    : String(value)}
                </strong>
              </p>
            ))}
          </article>
        ))}
      </div>
      <p className="muted">
        Contact your platform administrator to change your plan.
      </p>
    </>
  );
}
export function Tasks({ editable }: { editable: boolean }) {
  const [status, setStatus] = useState(""),
    [cursor, setCursor] = useState("");
  const list = useResource(`/tasks?status=${enc(status)}&after=${enc(cursor)}`);
  return (
    <>
      <div className="toolbar">
        <Field label="Task status">
          <select
            value={status}
            onChange={(e) => {
              setStatus(e.target.value);
              setCursor("");
            }}
          >
            <option value="">All</option>
            <option>pending</option>
            <option>claimed</option>
            <option>completed</option>
          </select>
        </Field>
        <button onClick={list.reload}>Refresh</button>
      </div>
      <ResourceState error={list.error} loading={!list.data} />
      {list.data?.items?.map((t: RecordData) => (
        <section className="panel" key={t.id}>
          <div className="row spread">
            <h2>
              {t.workflow} · {t.node_id}
            </h2>
            <Status value={t.status} />
          </div>
          <p>
            Queue: {t.queue} ·{" "}
            {t.claimed_by ? `Claimed by ${t.claimed_by}` : "Unclaimed"}
          </p>
          <JSONView value={t.payload} />
          {editable &&
            !["completed", "cancelled", "timed_out"].includes(t.status) && (
              <>
                <div className="row">
                  <AsyncButton
                    run={async () => {
                      await api(`/tasks/${t.id}/claim`, "POST");
                      list.reload();
                    }}
                  >
                    Claim
                  </AsyncButton>
                  <AsyncButton
                    run={async () => {
                      await api(`/tasks/${t.id}/release`, "POST");
                      list.reload();
                    }}
                  >
                    Release
                  </AsyncButton>
                </div>
                <ActionForm
                  label="Complete task"
                  submit={async (data) => {
                    await api(`/tasks/${t.id}/complete`, "POST", {
                      payload: JSON.parse(String(data.get("payload"))),
                    });
                    list.reload();
                  }}
                >
                  <Field label="Completion payload (JSON)">
                    <textarea name="payload" defaultValue="{}" required />
                  </Field>
                </ActionForm>
              </>
            )}
        </section>
      ))}
      {list.data && !list.data.items.length && (
        <Empty>No tasks match this status.</Empty>
      )}
      {list.data?.next && (
        <button onClick={() => setCursor(list.data!.next)}>Older tasks</button>
      )}
      {cursor && <button onClick={() => setCursor("")}>Newest tasks</button>}
    </>
  );
}
