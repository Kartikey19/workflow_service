import { useState } from "react";
import { api, type RecordData } from "./api";
import {
  ActionForm,
  AsyncButton,
  Empty,
  Field,
  OneTime,
  ResourceState,
  Status,
  useResource,
} from "./components";

const eventLabels: Record<string, [string, string]> = {
  "run.failed": [
    "A workflow starts failing",
    "Once per incident; later failures are counted, not repeated.",
  ],
  "run.recovered": [
    "A failing workflow recovers",
    "Sent with the number of failed runs.",
  ],
  "connection.reauth": [
    "An account needs reconnecting",
    "At most daily per step.",
  ],
  "schedule.failed": [
    "A schedule could not start or missed a run",
    "Per missed slot.",
  ],
};

const kinds: [string, string][] = [
  ["slack", "Slack incoming webhook"],
  ["webhook", "Webhook (signed JSON)"],
  ["email", "Email through a saved account"],
];

export function Alerts({
  admin,
  onRun,
}: {
  admin: boolean;
  onRun: (id: string) => void;
}) {
  const list = useResource("/alert-channels"),
    deliveries = useResource("/alert-deliveries?limit=30"),
    workflows = useResource("/workflows"),
    connections = useResource(admin ? "/connections" : null);
  const [form, setForm] = useState(false),
    [kind, setKind] = useState("slack"),
    [created, setCreated] = useState<RecordData | null>(null),
    [tested, setTested] = useState("");
  const mailAccounts = (connections.data?.items || []).filter((c: RecordData) =>
    ["smtp", "resend"].includes(c.kind),
  );
  const events: string[] = list.data?.events || Object.keys(eventLabels);
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Hear about failing workflows, accounts that need reconnecting and
          schedules that did not run. Test runs never alert.
        </p>
        {admin && (
          <button className="primary" onClick={() => setForm(!form)}>
            New alert channel
          </button>
        )}
      </div>
      {created && (
        <OneTime
          title="Webhook signing secret"
          value={created}
          close={() => setCreated(null)}
        />
      )}
      {tested && (
        <p className="success" role="status">
          {tested}
        </p>
      )}
      {form && admin && (
        <section className="panel">
          <h2>New alert channel</h2>
          <ActionForm
            label="Create channel"
            done=""
            submit={async (data) => {
              const body: RecordData = {
                name: data.get("name"),
                kind,
                events: data.getAll("events"),
                workflows: data.getAll("workflows"),
              };
              if (kind === "email") {
                body.connection = data.get("connection");
                body.to = String(data.get("to") || "")
                  .split(/[,\s]+/)
                  .filter(Boolean);
              } else body.url = data.get("url");
              const r = await api("/alert-channels", "POST", body);
              if (r.signing_secret)
                setCreated({
                  signing_secret: r.signing_secret,
                  signature_header:
                    "X-Weft-Signature: sha256=<HMAC-SHA256 of the raw body>",
                });
              setForm(false);
              list.reload();
            }}
          >
            <Field
              label="Channel name"
              hint="Lowercase letters, digits, - and _."
            >
              <input
                name="name"
                required
                pattern="[a-z0-9][a-z0-9_\-]{0,62}"
                spellCheck={false}
              />
            </Field>
            <Field label="Send to">
              <select value={kind} onChange={(e) => setKind(e.target.value)}>
                {kinds.map(([k, label]) => (
                  <option key={k} value={k}>
                    {label}
                  </option>
                ))}
              </select>
            </Field>
            {kind === "email" ? (
              <>
                <Field
                  label="Email account"
                  hint="An SMTP or Resend connection from the Connectors page."
                >
                  <select name="connection" required>
                    <option value="">Choose…</option>
                    {mailAccounts.map((c: RecordData) => (
                      <option key={c.name}>{c.name}</option>
                    ))}
                  </select>
                </Field>
                <Field
                  label="Recipients"
                  hint="Separate addresses with commas."
                >
                  <input name="to" required placeholder="ops@example.com" />
                </Field>
                {!mailAccounts.length && connections.data && (
                  <p className="notice">
                    Connect an SMTP or Resend account first.
                  </p>
                )}
              </>
            ) : (
              <Field
                label={kind === "slack" ? "Slack webhook URL" : "Endpoint URL"}
                hint={
                  kind === "slack"
                    ? "From a Slack app’s Incoming Webhooks page. It is stored encrypted and never shown again."
                    : "Receives a signed JSON POST for each alert."
                }
              >
                <input
                  name="url"
                  type="url"
                  required
                  autoComplete="off"
                  placeholder={
                    kind === "slack"
                      ? "https://hooks.slack.com/services/…"
                      : "https://example.com/weft-alerts"
                  }
                />
              </Field>
            )}
            <fieldset className="choices">
              <legend>Notify me when</legend>
              {events.map((e) => (
                <label className="check" key={e}>
                  <input
                    type="checkbox"
                    name="events"
                    value={e}
                    defaultChecked
                  />
                  <span>
                    {eventLabels[e]?.[0] || e}
                    <small className="muted"> {eventLabels[e]?.[1]}</small>
                  </span>
                </label>
              ))}
            </fieldset>
            <fieldset className="choices">
              <legend>Workflows</legend>
              <p className="muted small">
                Leave all unticked for every workflow.
              </p>
              {(workflows.data?.items || []).map((w: RecordData) => (
                <label className="check" key={w.name}>
                  <input type="checkbox" name="workflows" value={w.name} />
                  {w.name}
                </label>
              ))}
            </fieldset>
          </ActionForm>
        </section>
      )}
      <ResourceState error={list.error} loading={!list.data} />
      <div className="cards">
        {list.data?.items?.map((c: RecordData) => (
          <article className="panel card" key={c.id}>
            <span className="eyebrow">
              {kinds.find(([k]) => k === c.kind)?.[1] || c.kind}
            </span>
            <h2>{c.name}</h2>
            <p className="mono">{c.target}</p>
            <Status value={c.enabled ? "enabled" : "disabled"} />
            <p className="muted">
              {c.events
                .map((e: string) => eventLabels[e]?.[0] || e)
                .join(" · ")}
            </p>
            <p className="muted">
              {c.workflows.length
                ? `Only ${c.workflows.join(", ")}`
                : "Every workflow"}
            </p>
            {admin && (
              <div className="row wrap">
                <AsyncButton
                  run={async () => {
                    setTested("");
                    await api(`/alert-channels/${c.id}/test`, "POST");
                    setTested(`Test alert sent to ${c.name}.`);
                    deliveries.reload();
                  }}
                >
                  Send test
                </AsyncButton>
                <AsyncButton
                  run={async () => {
                    await api(`/alert-channels/${c.id}`, "PATCH", {
                      enabled: !c.enabled,
                    });
                    list.reload();
                  }}
                >
                  {c.enabled ? "Pause" : "Resume"}
                </AsyncButton>
                <AsyncButton
                  danger
                  run={async () => {
                    if (confirm(`Delete the alert channel ${c.name}?`)) {
                      await api(`/alert-channels/${c.id}`, "DELETE");
                      list.reload();
                      deliveries.reload();
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
      {list.data && !list.data.items.length && (
        <Empty>
          No alert channels yet.{" "}
          {admin
            ? "Add Slack, a webhook or email to hear when a workflow fails."
            : "Ask an administrator to add one."}
        </Empty>
      )}
      <section className="panel">
        <div className="row">
          <h2>Recent alerts</h2>
          <button onClick={deliveries.reload}>Refresh</button>
        </div>
        <ResourceState error={deliveries.error} loading={!deliveries.data} />
        {deliveries.data && !deliveries.data.items.length && (
          <p className="muted">Nothing has been sent yet.</p>
        )}
        <ol className="delivery-log">
          {deliveries.data?.items?.map((d: RecordData) => (
            <li key={d.id}>
              <Status value={d.status} />
              <span>
                <strong>{eventLabels[d.event]?.[0] || d.event}</strong>
                {d.payload?.workflow && ` · ${d.payload.workflow}`}
                <small className="muted">
                  {" "}
                  to {d.channel} · {new Date(d.created_at).toLocaleString()}
                  {d.attempts > 1 && ` · ${d.attempts} attempts`}
                </small>
                {d.last_error && d.status !== "sent" && (
                  <small className="error"> {d.last_error}</small>
                )}
                {d.status === "pending" && d.next_attempt_at && (
                  <small className="muted">
                    {" "}
                    · next try{" "}
                    {new Date(d.next_attempt_at).toLocaleTimeString()}
                  </small>
                )}
              </span>
              {d.payload?.run_id && (
                <button
                  className="link"
                  onClick={() => onRun(d.payload.run_id)}
                >
                  View run
                </button>
              )}
            </li>
          ))}
        </ol>
      </section>
    </>
  );
}
