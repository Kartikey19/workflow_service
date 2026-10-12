import { useEffect, useMemo, useState } from "react";
import { parse } from "yaml";
import { X } from "lucide-react";
import { api, enc, terminal, type RecordData } from "./api";
import {
  AsyncButton,
  Field,
  InputFields,
  JSONView,
  ResourceState,
  Status,
  readInput,
  useResource,
} from "./components";
import {
  attemptFacts,
  failedStep,
  latestByStep,
  useRunWatch,
} from "./run-watch";

export type StepStatus = Record<string, string>;

// Steps that act on the outside world when they run. A test run executes them
// for real, so the person confirms before starting.
export function sideEffects(
  doc: RecordData | null,
  integrations: RecordData[] = [],
): { id: string; reason: string }[] {
  const readOnly = new Set(
    integrations
      .flatMap((k) => k.actions || [])
      .filter((a: RecordData) => a.read_only)
      .map((a: RecordData) => a.name),
  );
  const out: { id: string; reason: string }[] = [];
  for (const n of doc?.nodes || []) {
    const c = n.config || {};
    const method = String(c.method || "GET").toUpperCase();
    let reason = "";
    switch (n.type) {
      case "http":
        if (!["GET", "HEAD", "OPTIONS"].includes(method))
          reason = `sends an HTTP ${method} request`;
        break;
      case "email.send":
      case "gmail.send":
        reason = "sends email";
        break;
      case "db.insert":
        reason = "writes to a database";
        break;
      case "db.query":
        if (!/^\s*(select|with|show|explain)\b/i.test(String(c.sql || "")))
          reason = "may change database rows";
        break;
      case "gdrive.upload":
      case "dropbox.upload":
      case "onedrive.upload":
        reason = "uploads a file";
        break;
      case "mcp.call":
        reason = "calls an external MCP tool";
        break;
      case "workflow":
        reason = "starts another workflow";
        break;
      case "connector.call":
        if (!readOnly.has(c.action))
          reason = `runs ${c.action || "an action"} on ${c.connection || "a connected account"}`;
        break;
      case "agent":
        if (c.tools?.length || c.connections?.length)
          reason = "an AI step that may use tools";
        break;
    }
    if (reason) out.push({ id: n.id, reason });
  }
  return out;
}

function readDoc(source: string): RecordData | null {
  try {
    const doc = parse(source);
    return doc && typeof doc === "object" ? doc : null;
  } catch {
    return null;
  }
}

export function TestRun({
  source,
  spec,
  published,
  editable,
  onStatus,
  onSelectStep,
  onOpenRun,
  onClose,
}: {
  source: string;
  // The compiled draft from validation; null while it has problems.
  spec: RecordData | null;
  // The published version being edited, when the draft is unchanged from it.
  published: RecordData | null;
  editable: boolean;
  onStatus: (status: StepStatus) => void;
  onSelectStep: (id: string) => void;
  onOpenRun: (id: string) => void;
  onClose: () => void;
}) {
  const [runID, setRunID] = useState(""),
    [confirmed, setConfirmed] = useState(false),
    [open, setOpen] = useState(""),
    [error, setError] = useState("");
  const integrations = useResource("/integrations");
  const doc = useMemo(() => readDoc(source), [source]);
  const effects = sideEffects(doc, integrations.data?.items);
  const { detail, steps } = useRunWatch(runID);
  const run = detail.data;
  const latest = latestByStep(steps);
  const failing = failedStep(run, steps);
  const order: string[] =
    spec?.topo_order || (doc?.nodes || []).map((n: RecordData) => n.id);
  const statusKey = Object.values(latest)
    .map((n) => `${n.node_id}:${n.status}`)
    .join(",");
  useEffect(() => {
    onStatus(
      Object.fromEntries(
        Object.values(latest).map((n) => [n.node_id, n.status]),
      ),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [statusKey]);
  useEffect(() => () => onStatus({}), []); // eslint-disable-line react-hooks/exhaustive-deps
  // Jump to the failure as soon as it is known.
  useEffect(() => {
    if (failing && run && terminal(run.status)) {
      setOpen(failing.node_id);
      onSelectStep(failing.node_id);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [failing?.id, run?.status]);
  const inputs = spec?.inputs || {};
  const hasInputs = Object.keys(inputs).length > 0;
  async function start(data: FormData) {
    setError("");
    try {
      const input = hasInputs
        ? readInput(data, inputs)
        : JSON.parse(String(data.get("input") || "{}"));
      const r = published
        ? await api("/runs", "POST", {
            workflow: published.name,
            version: published.version,
            input,
          })
        : await api("/test-runs", "POST", { yaml: source, input });
      setOpen("");
      setRunID(r.id);
    } catch (e) {
      setError((e as Error).message);
    }
  }
  return (
    <aside className="test-drawer" aria-label="Test run">
      <div className="test-head">
        <h2>Test run</h2>
        <button
          className="icon-button"
          aria-label="Close test run"
          onClick={onClose}
        >
          <X size={16} aria-hidden="true" />
        </button>
      </div>
      <p className="muted">
        {published
          ? `Runs published version ${published.version} with real accounts.`
          : "Runs your unpublished draft with real accounts. Nothing is published, and the run is labelled draft test."}
      </p>
      {!spec ? (
        <p className="notice" role="status">
          Fix the workflow’s problems before testing it.
        </p>
      ) : !runID ? (
        <form
          onSubmit={(e) => {
            e.preventDefault();
            return start(new FormData(e.currentTarget));
          }}
        >
          {hasInputs ? (
            <InputFields schema={inputs} />
          ) : (
            <Field label="Input (JSON)">
              <textarea name="input" className="code" defaultValue="{}" />
            </Field>
          )}
          {effects.length > 0 && (
            <div className="effects" role="group" aria-label="Real actions">
              <strong>These steps act on real systems:</strong>
              <ul>
                {effects.map((e) => (
                  <li key={e.id}>
                    <span className="mono">{e.id}</span> {e.reason}
                  </li>
                ))}
              </ul>
              <label className="check">
                <input
                  type="checkbox"
                  checked={confirmed}
                  onChange={(e) => setConfirmed(e.target.checked)}
                />
                I understand these actions will really happen
              </label>
            </div>
          )}
          {error && (
            <p className="error" role="alert">
              {error}
            </p>
          )}
          <button
            className="primary"
            type="submit"
            disabled={!editable || (effects.length > 0 && !confirmed)}
          >
            Run test
          </button>
        </form>
      ) : (
        <>
          <ResourceState error={detail.error} loading={!run} />
          {run && (
            <div className="test-status">
              <Status value={run.status} />
              <span className="muted">
                {run.test ? "Draft test" : `Version ${run.version}`}
              </span>
              <button className="link" onClick={() => onOpenRun(runID)}>
                Open in Runs
              </button>
            </div>
          )}
          {run?.error && (
            <div className="run-result failed">
              <h3>Why it failed</h3>
              <p>
                {run.error.node_id && <strong>{run.error.node_id}: </strong>}
                {run.error.message}
              </p>
            </div>
          )}
          <ol className="test-steps" aria-label="Test steps">
            {order.map((id) => {
              const n = latest[id];
              const isOpen = open === id;
              return (
                <li
                  key={id}
                  className={`${isOpen ? "open" : ""} ${failing?.node_id === id ? "is-failure" : ""}`}
                >
                  <button
                    className="test-step"
                    aria-expanded={isOpen}
                    onClick={() => {
                      setOpen(isOpen ? "" : id);
                      onSelectStep(id);
                    }}
                  >
                    <span className="mono">{id}</span>
                    <small className="muted">
                      {n ? attemptFacts(n.metrics) : ""}
                      {n && n.attempt > 1 ? ` · attempt ${n.attempt}` : ""}
                    </small>
                    <Status value={n?.status || "not started"} />
                  </button>
                  {isOpen && n && (
                    <div className="split">
                      <div>
                        <h4>Input</h4>
                        <JSONView value={n.input} />
                      </div>
                      <div>
                        <h4>{n.error ? "Error" : "Output"}</h4>
                        <JSONView value={n.error || n.output} />
                      </div>
                    </div>
                  )}
                  {isOpen && !n && (
                    <p className="muted">This step has not run.</p>
                  )}
                </li>
              );
            })}
          </ol>
          {run && terminal(run.status) && (
            <div className="row">
              {editable && ["failed", "cancelled"].includes(run.status) && (
                <AsyncButton
                  primary
                  run={async () => {
                    const r = await api(`/runs/${enc(runID)}/resume`, "POST");
                    setOpen("");
                    setRunID(r.id);
                  }}
                >
                  Resume from failed step
                </AsyncButton>
              )}
              <button
                onClick={() => {
                  setRunID("");
                  setConfirmed(false);
                  onStatus({});
                }}
              >
                New test
              </button>
            </div>
          )}
          {run && run.test && (
            <p className="muted small">
              Resume reruns the draft that was tested. Edit the draft and choose
              New test to run your changes.
            </p>
          )}
        </>
      )}
    </aside>
  );
}
