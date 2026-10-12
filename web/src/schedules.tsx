import { useEffect, useState } from "react";
import { api, enc, type RecordData } from "./api";
import {
  AsyncButton,
  Empty,
  Field,
  InputFields,
  ResourceState,
  Status,
  readInput,
  useResource,
} from "./components";

const presets: [string, string][] = [
  ["0 * * * *", "Every hour"],
  ["0 9 * * *", "Every day at 09:00"],
  ["0 9 * * 1-5", "Weekdays at 09:00"],
  ["0 9 * * 1", "Every Monday at 09:00"],
  ["0 9 1 * *", "On the 1st of each month at 09:00"],
  ["*/15 * * * *", "Every 15 minutes"],
];

export const describeCron = (cron: string) =>
  presets.find(([c]) => c === cron)?.[1] || cron;

const outcomes: Record<string, string> = {
  started: "Started a run",
  skipped: "Skipped",
  missed: "Missed",
  error: "Could not start",
};

function zones(): string[] {
  try {
    return (
      Intl as unknown as { supportedValuesOf(k: string): string[] }
    ).supportedValuesOf("timeZone");
  } catch {
    return ["UTC"];
  }
}

const browserZone = () => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
};

export function when(value: string | null | undefined, zone?: string) {
  if (!value) return "—";
  return new Date(value).toLocaleString(undefined, {
    weekday: "short",
    day: "numeric",
    month: "short",
    hour: "2-digit",
    minute: "2-digit",
    timeZone: zone,
    timeZoneName: "short",
  });
}

// Shows the next start times as the cron expression and zone are typed.
function NextRuns({ cron, timezone }: { cron: string; timezone: string }) {
  const [next, setNext] = useState<string[]>([]),
    [error, setError] = useState("");
  useEffect(() => {
    if (!cron.trim()) {
      setNext([]);
      setError("");
      return;
    }
    const abort = new AbortController();
    const timer = setTimeout(() => {
      api(
        "/schedules/preview",
        "POST",
        { cron, timezone, count: 5 },
        abort.signal,
      )
        .then((r) => {
          setNext(r.next || []);
          setError("");
        })
        .catch((e) => {
          if (!abort.signal.aborted) {
            setNext([]);
            setError(e.message);
          }
        });
    }, 300);
    return () => {
      clearTimeout(timer);
      abort.abort();
    };
  }, [cron, timezone]);
  return (
    <div className="next-runs" aria-live="polite">
      <strong>Next runs</strong>
      {error ? (
        <p className="error" role="alert">
          {error}
        </p>
      ) : (
        <ol>
          {next.map((t) => (
            <li key={t}>{when(t, timezone)}</li>
          ))}
        </ol>
      )}
    </div>
  );
}

function ScheduleForm({
  initial,
  workflows,
  onSaved,
  onCancel,
}: {
  initial: RecordData | null;
  workflows: RecordData[];
  onSaved: () => void;
  onCancel: () => void;
}) {
  const editing = !!initial;
  const [workflow, setWorkflow] = useState(initial?.workflow || ""),
    [cron, setCron] = useState(initial?.cron || "0 9 * * 1-5"),
    [timezone, setTimezone] = useState(initial?.timezone || browserZone()),
    [error, setError] = useState(""),
    [busy, setBusy] = useState(false);
  const detail = useResource(workflow ? `/workflows/${enc(workflow)}` : null);
  const inputs: RecordData = detail.data?.latest?.spec?.inputs || {};
  const hasInputs = Object.keys(inputs).length > 0;
  const preset = presets.some(([c]) => c === cron) ? cron : "custom";
  const schema = Object.fromEntries(
    Object.entries(inputs).map(([k, v]) => [
      k,
      initial?.input?.[k] !== undefined
        ? { ...(v as RecordData), default: initial.input[k] }
        : v,
    ]),
  );
  return (
    <section className="panel">
      <h2>{editing ? `Edit ${initial.name}` : "New schedule"}</h2>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          const data = new FormData(e.currentTarget);
          setBusy(true);
          setError("");
          try {
            const input = hasInputs
              ? readInput(data, inputs)
              : JSON.parse(String(data.get("input") || "{}"));
            const body = {
              cron,
              timezone,
              input,
              overlap: data.get("overlap"),
              catch_up: data.get("catch_up"),
            };
            if (editing) await api(`/schedules/${initial.id}`, "PATCH", body);
            else
              await api("/schedules", "POST", {
                ...body,
                name: data.get("name"),
                workflow,
                enabled: data.get("enabled") === "on",
              });
            onSaved();
          } catch (e) {
            setError((e as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        {!editing && (
          <Field
            label="Schedule name"
            hint="Lowercase letters, digits, - and _."
          >
            <input
              name="name"
              required
              pattern="[a-z0-9][a-z0-9_\-]{0,62}"
              spellCheck={false}
            />
          </Field>
        )}
        <Field label="Workflow">
          <select
            value={workflow}
            disabled={editing}
            required
            onChange={(e) => setWorkflow(e.target.value)}
          >
            <option value="">Choose…</option>
            {workflows.map((w) => (
              <option key={w.name}>{w.name}</option>
            ))}
          </select>
        </Field>
        <div className="split">
          <Field label="Repeats">
            <select
              value={preset}
              onChange={(e) => {
                if (e.target.value !== "custom") setCron(e.target.value);
              }}
            >
              {presets.map(([c, label]) => (
                <option key={c} value={c}>
                  {label}
                </option>
              ))}
              <option value="custom">Custom cron expression</option>
            </select>
          </Field>
          <Field
            label="Cron expression"
            hint="minute hour day-of-month month day-of-week, or @daily"
          >
            <input
              className="mono"
              value={cron}
              spellCheck={false}
              required
              onChange={(e) => setCron(e.target.value)}
            />
          </Field>
        </div>
        <Field
          label="Timezone"
          hint="Daylight-saving changes follow this zone."
        >
          <select
            value={timezone}
            onChange={(e) => setTimezone(e.target.value)}
          >
            {[...new Set(["UTC", timezone, ...zones()])].map((z) => (
              <option key={z}>{z}</option>
            ))}
          </select>
        </Field>
        <NextRuns cron={cron} timezone={timezone} />
        {workflow && (
          <fieldset className="schedule-input" key={workflow}>
            <legend>Input for each run</legend>
            <ResourceState error={detail.error} loading={!detail.data} />
            {detail.data &&
              (hasInputs ? (
                <InputFields schema={schema} />
              ) : (
                <Field label="Input (JSON)">
                  <textarea
                    name="input"
                    className="code"
                    defaultValue={JSON.stringify(initial?.input || {})}
                  />
                </Field>
              ))}
          </fieldset>
        )}
        <div className="split">
          <Field label="If the previous run is still going">
            <select name="overlap" defaultValue={initial?.overlap || "skip"}>
              <option value="skip">Skip this slot</option>
              <option value="allow">Start another run anyway</option>
            </select>
          </Field>
          <Field
            label="If slots were missed"
            hint="For example while the service was down."
          >
            <select
              name="catch_up"
              defaultValue={initial?.catch_up || "latest"}
            >
              <option value="latest">Run once to catch up</option>
              <option value="skip">Skip them</option>
            </select>
          </Field>
        </div>
        {!editing && (
          <label className="check">
            <input type="checkbox" name="enabled" defaultChecked />
            Turn on now
          </label>
        )}
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <div className="row">
          <button className="primary" type="submit" disabled={busy}>
            {busy ? "Working…" : editing ? "Save schedule" : "Create schedule"}
          </button>
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        </div>
      </form>
    </section>
  );
}

export function Schedules({
  editable,
  onRun,
}: {
  editable: boolean;
  onRun: (id: string) => void;
}) {
  const list = useResource("/schedules"),
    workflows = useResource("/workflows");
  const [form, setForm] = useState<RecordData | "new" | null>(null);
  const published = (workflows.data?.items || []).filter(
    (w: RecordData) => !w.archived_at,
  );
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Start a workflow on a recurring schedule. Each slot starts at most one
          run, even with several workers.
        </p>
        {editable && (
          <button
            className="primary"
            onClick={() => setForm(form === "new" ? null : "new")}
          >
            New schedule
          </button>
        )}
      </div>
      {form && (
        <ScheduleForm
          key={form === "new" ? "new" : form.id}
          initial={form === "new" ? null : form}
          workflows={published}
          onCancel={() => setForm(null)}
          onSaved={() => {
            setForm(null);
            list.reload();
          }}
        />
      )}
      <ResourceState error={list.error} loading={!list.data} />
      <div className="cards">
        {list.data?.items?.map((s: RecordData) => (
          <article className="panel card schedule-card" key={s.id}>
            <span className="eyebrow">{describeCron(s.cron)}</span>
            <h2>{s.name}</h2>
            <p>
              {s.workflow} · <span className="mono">{s.cron}</span> ·{" "}
              {s.timezone}
            </p>
            <Status value={s.enabled ? "enabled" : "disabled"} />
            <dl className="facts">
              <dt>Next run</dt>
              <dd>{s.enabled ? when(s.next_run_at, s.timezone) : "Off"}</dd>
              <dt>Last slot</dt>
              <dd>
                {s.last_outcome ? (
                  <>
                    {outcomes[s.last_outcome] || s.last_outcome}
                    {s.last_slot_at && ` · ${when(s.last_slot_at, s.timezone)}`}
                    {s.last_run_id && (
                      <>
                        {" · "}
                        <button
                          className="link"
                          onClick={() => onRun(s.last_run_id)}
                        >
                          View run
                        </button>
                      </>
                    )}
                  </>
                ) : (
                  "Not run yet"
                )}
              </dd>
            </dl>
            {s.last_error && (
              <p className={s.last_outcome === "skipped" ? "muted" : "error"}>
                {s.last_error}
              </p>
            )}
            {editable && (
              <div className="row wrap">
                <AsyncButton
                  run={async () => {
                    const r = await api(`/schedules/${s.id}/run`, "POST");
                    onRun(r.id);
                  }}
                >
                  Run now
                </AsyncButton>
                <button onClick={() => setForm(s)}>Edit</button>
                <AsyncButton
                  run={async () => {
                    await api(`/schedules/${s.id}`, "PATCH", {
                      enabled: !s.enabled,
                    });
                    list.reload();
                  }}
                >
                  {s.enabled ? "Turn off" : "Turn on"}
                </AsyncButton>
                <AsyncButton
                  danger
                  run={async () => {
                    if (confirm(`Delete the schedule ${s.name}?`)) {
                      await api(`/schedules/${s.id}`, "DELETE");
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
      {list.data && !list.data.items.length && (
        <Empty>
          No schedules yet. Choose New schedule to run a workflow every hour,
          day or week.
        </Empty>
      )}
    </>
  );
}

// What starts this workflow, shown on its Start step.
export function WorkflowTriggers({
  workflow,
  onNavigate,
}: {
  workflow: string;
  onNavigate?: (page: string) => void;
}) {
  const schedules = useResource(
    workflow ? `/schedules?workflow=${enc(workflow)}` : null,
  );
  const items: RecordData[] = schedules.data?.items || [];
  return (
    <div className="workflow-triggers">
      <h4>Starts</h4>
      <ul>
        <li>By hand, from a test run, the API or another workflow</li>
        {items.map((s) => (
          <li key={s.id}>
            <strong>{s.name}</strong>: {describeCron(s.cron)} ({s.timezone})
            {s.enabled
              ? `, next ${when(s.next_run_at, s.timezone)}`
              : ", turned off"}
          </li>
        ))}
      </ul>
      {onNavigate && (
        <div className="row wrap">
          <button type="button" onClick={() => onNavigate("schedules")}>
            {items.length ? "Manage schedules" : "Add a schedule"}
          </button>
          <button type="button" onClick={() => onNavigate("webhooks")}>
            Webhooks
          </button>
        </div>
      )}
    </div>
  );
}
