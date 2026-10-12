import { connectorActions, kindIcon } from "./connectors";
import { ConnectorForm } from "./setup";
import { useState } from "react";
import { api, enc, secretParts, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  AsyncButton,
  Empty,
  ResourceState,
  OneTime,
  InviteNotice,
  Status,
  JSONView,
  humanize,
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
      <ResourceState error={list.error} loading={!list.data} />
      <div className="cards">
        {(list.data?.items || []).map((c: RecordData) => (
          <article className="panel card" key={c.id}>
            <div className="card-head">
              <span className="card-icon">
                {(() => {
                  const Icon = kindIcon(c.kind);
                  return <Icon size={18} aria-hidden="true" />;
                })()}
              </span>
              <div>
                <h2>{c.name}</h2>
                <small>
                  {kinds.data?.items.find((k: RecordData) => k.kind === c.kind)
                    ?.title || c.kind}
                </small>
              </div>
            </div>
            <small>{c.account || "Credentials encrypted"}</small>
            {
              <ul className="connector-actions" aria-label="Actions">
                {connectorActions(kinds.data?.items, c.kind).map((action) => (
                  <li key={action.name}>
                    {action.title}
                    <span className={action.read_only ? "reads" : "writes"}>
                      {action.read_only ? "Read only" : "Writes"}
                    </span>
                  </li>
                ))}
              </ul>
            }
            <div className="row">
              {admin && (
                <>
                  <AsyncButton
                    run={async () =>
                      setResult(
                        await api(`/connections/${enc(c.name)}/test`, "POST"),
                      )
                    }
                  >
                    Test connection
                  </AsyncButton>
                  {!kinds.data?.items.find((k: RecordData) => k.kind === c.kind)
                    ?.oauth && (
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
                        await api(`/connections/${enc(c.name)}`, "DELETE");
                        list.reload();
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
      {list.data && !list.data.items.length && (
        <Empty>
          No connectors yet. Add a database, mail or chat service to use it in
          workflows and agents.
        </Empty>
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
          {!editing && (
            <div className="app-grid" role="radiogroup" aria-label="App">
              {(kinds.data?.items || []).map((k: RecordData) => {
                const Icon = kindIcon(k.kind);
                return (
                  <button
                    key={k.kind}
                    type="button"
                    role="radio"
                    aria-checked={kind === k.kind}
                    className="app-tile"
                    onClick={() => setKind(k.kind)}
                  >
                    <Icon size={18} aria-hidden="true" />
                    {k.title}
                  </button>
                );
              })}
            </div>
          )}
          {descriptor && (
            <ConnectorForm
              descriptor={descriptor}
              editing={editing}
              onSaved={() => {
                setForm(false);
                edit(null);
                list.reload();
              }}
            />
          )}
        </section>
      )}
    </>
  );
}
export function Vault({ admin }: { admin: boolean }) {
  const list = useResource("/secrets");
  const [form, setForm] = useState(false),
    [grouped, setGrouped] = useState(false),
    [rows, setRows] = useState([0]);
  // Names with a dot are keys of a group: openai.api_key belongs to openai.
  const plain: RecordData[] = [],
    groups: Record<string, RecordData[]> = {};
  for (const s of list.data?.items || []) {
    const { group, key } = secretParts(s.name);
    if (group === undefined) plain.push(s);
    else (groups[group] ||= []).push({ ...s, key });
  }
  const remove = (name: string, label: string) => (
    <AsyncButton
      danger
      run={async () => {
        if (confirm(`Delete ${label}?`)) {
          await api(`/secrets/${enc(name)}`, "DELETE");
          list.reload();
        }
      }}
    >
      Delete
    </AsyncButton>
  );
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Write-only secrets for your workflow steps. Keep related keys together
          in a group and read them as <code>{"${secrets.group.key}"}</code>.
        </p>
        {admin && (
          <button className="primary" onClick={() => setForm(!form)}>
            Add or replace secret
          </button>
        )}
      </div>
      <ResourceState error={list.error} loading={!list.data} />
      {!!plain.length && (
        <section className="panel">
          <h2>Secrets</h2>
          {plain.map((s) => (
            <div className="resource-row" key={s.name}>
              <strong className="mono">{s.name}</strong>
              <small>
                Updated {new Date(s.updated_at).toLocaleDateString()}
              </small>
              {admin && remove(s.name, `secret ${s.name}`)}
            </div>
          ))}
        </section>
      )}
      {Object.entries(groups).map(([group, keys]) => (
        <section className="panel" key={group}>
          <div className="row spread group-head">
            <h2 className="mono">{group}</h2>
            {admin && remove(group, `the group ${group} and all its keys`)}
          </div>
          {keys.map((k) => (
            <div className="resource-row" key={k.name}>
              <strong className="mono">{k.key}</strong>
              <small className="mono">{k.reference}</small>
              {admin && remove(k.name, `${group}.${k.key}`)}
            </div>
          ))}
        </section>
      ))}
      {list.data && !list.data.items.length && (
        <Empty>Your vault is empty.</Empty>
      )}
      {form && admin && (
        <section className="panel">
          <div className="row spread">
            <h2>Add or replace secret</h2>
            <div className="segmented" role="group" aria-label="Kind">
              <button aria-pressed={!grouped} onClick={() => setGrouped(false)}>
                Single secret
              </button>
              <button aria-pressed={grouped} onClick={() => setGrouped(true)}>
                Group of keys
              </button>
            </div>
          </div>
          <ActionForm
            key={String(grouped)}
            submit={async (data) => {
              const name = String(data.get("name"));
              if (grouped) {
                const keys = data.getAll("key").map(String),
                  values = data.getAll("value").map(String);
                await api(`/secrets/${enc(name)}`, "PUT", {
                  fields: Object.fromEntries(
                    keys.map((k, i) => [k, values[i]]),
                  ),
                });
              } else {
                await api(`/secrets/${enc(name)}`, "PUT", {
                  value: data.get("value"),
                });
              }
              setForm(false);
              setRows([0]);
              list.reload();
            }}
          >
            <Field
              label={grouped ? "Group name" : "Secret name"}
              hint={
                grouped
                  ? "For example open_api_settings. Workflows read each key as secrets.group.key."
                  : undefined
              }
            >
              <input
                name="name"
                required
                pattern="[A-Za-z][A-Za-z0-9_]*"
                placeholder={grouped ? "open_api_settings" : "API_KEY"}
              />
            </Field>
            {grouped ? (
              <>
                {rows.map((id, i) => (
                  <div className="split key-row" key={id}>
                    <Field label={i === 0 ? "Key" : ""}>
                      <input
                        name="key"
                        required
                        aria-label="Key"
                        pattern="[A-Za-z][A-Za-z0-9_]*"
                        placeholder={["api_key", "effort", "temperature"][i]}
                      />
                    </Field>
                    <Field label={i === 0 ? "Value" : ""}>
                      <input
                        name="value"
                        required
                        aria-label="Value"
                        type="password"
                        autoComplete="off"
                      />
                    </Field>
                    {rows.length > 1 && (
                      <button
                        type="button"
                        aria-label="Remove key"
                        onClick={() => setRows(rows.filter((r) => r !== id))}
                      >
                        ×
                      </button>
                    )}
                  </div>
                ))}
                <p>
                  <button
                    type="button"
                    onClick={() => setRows([...rows, Math.max(...rows) + 1])}
                  >
                    Add key
                  </button>
                </p>
                <small>
                  Values shorter than 8 characters, such as an effort or a
                  temperature, are treated as settings and are not masked.
                </small>
              </>
            ) : (
              <Field label="Value">
                <input
                  name="value"
                  required
                  type="password"
                  autoComplete="off"
                />
              </Field>
            )}
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
              {t.signed ? "Signed with HMAC" : "Secret token in the URL"}
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
            done=""
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
        <InviteNotice invite={created} close={() => setCreated(null)} />
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
              done=""
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
            done=""
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
          <span className="eyebrow">Current plan</span>
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
// A person decides: approve or reject with a note, which the next step reads
// as nodes.<step>.output.approved and .note. Other answers can be sent as JSON.
function TaskDecision({ task, done }: { task: RecordData; done: () => void }) {
  const [note, setNote] = useState(""),
    [custom, setCustom] = useState("{}");
  const complete = async (payload: unknown) => {
    await api(`/tasks/${task.id}/complete`, "POST", { payload });
    done();
  };
  return (
    <div className="task-decision">
      <Field label="Note">
        <textarea
          rows={2}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder="Optional reason, shown to the next step"
        />
      </Field>
      <div className="row">
        <AsyncButton primary run={() => complete({ approved: true, note })}>
          Approve
        </AsyncButton>
        <AsyncButton danger run={() => complete({ approved: false, note })}>
          Reject
        </AsyncButton>
        <AsyncButton
          run={async () => {
            await api(
              `/tasks/${task.id}/${task.claimed_by ? "release" : "claim"}`,
              "POST",
            );
            done();
          }}
        >
          {task.claimed_by ? "Release" : "Claim"}
        </AsyncButton>
      </div>
      <details>
        <summary>Send other data instead</summary>
        <Field label="Completion data (JSON)">
          <textarea
            className="code"
            value={custom}
            onChange={(e) => setCustom(e.target.value)}
          />
        </Field>
        <AsyncButton run={() => complete(JSON.parse(custom))}>
          Complete with this data
        </AsyncButton>
      </details>
    </div>
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
      {list.data?.items?.map((t: RecordData) => {
        const open = !["completed", "cancelled", "timed_out"].includes(
          t.status,
        );
        return (
          <section className="panel task" key={t.id}>
            <div className="row spread">
              <div>
                <h2>{t.workflow}</h2>
                <small>
                  Step {t.node_id} in the {t.queue} queue,{" "}
                  {t.claimed_by ? `claimed by ${t.claimed_by}` : "unclaimed"}
                </small>
              </div>
              <Status value={t.status} />
            </div>
            <dl className="task-fields">
              {Object.entries(t.payload || {}).map(([k, v]) => (
                <div key={k}>
                  <dt>{humanize(k)}</dt>
                  <dd>{typeof v === "string" ? v : JSON.stringify(v)}</dd>
                </div>
              ))}
            </dl>
            {editable && open && <TaskDecision task={t} done={list.reload} />}
          </section>
        );
      })}
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
