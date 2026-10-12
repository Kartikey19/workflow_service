import { useEffect, useRef, useState } from "react";
import { api, enc, terminal, type RecordData } from "./api";
import {
  useResource,
  Field,
  JSONView,
  Status,
  AsyncButton,
  Empty,
  ResourceState,
} from "./components";
import { attemptFacts, failedStep, useRunWatch } from "./run-watch";

// How a run is labelled: draft tests have no published version number.
export const versionLabel = (r: RecordData) =>
  r.test ? "draft test" : `v${r.version}`;

export function Runs({
  initialID,
  editable,
  onOpenStep,
}: {
  initialID: string;
  editable: boolean;
  onOpenStep?: (workflow: string, step: string) => void;
}) {
  const [status, setStatus] = useState(""),
    [workflow, setWorkflow] = useState(""),
    [cursor, setCursor] = useState(""),
    [selected, setSelected] = useState(initialID),
    [open, setOpen] = useState<Record<string, boolean>>({});
  const list = useResource(
    `/runs?limit=30&status=${enc(status)}&workflow=${enc(workflow)}&after=${enc(cursor)}`,
  );
  const { detail, nodes, steps, events, streamError } = useRunWatch(selected);
  const stepList = useRef<HTMLDivElement>(null),
    focused = useRef("");
  useEffect(() => {
    setSelected(initialID);
  }, [initialID]);
  function select(id: string) {
    setSelected(id);
    history.pushState({}, "", `/ui/runs/${enc(id)}`);
  }
  // A failed run opens on the step that failed, so the cause is in view.
  const failing = failedStep(detail.data, steps);
  useEffect(() => {
    setOpen({});
    focused.current = "";
  }, [selected]);
  useEffect(() => {
    if (!failing || focused.current === selected) return;
    focused.current = selected;
    setOpen((o) => ({ ...o, [failing.id]: true }));
  }, [failing?.id, selected]);
  function showStep(id: string) {
    setOpen((o) => ({ ...o, [id]: true }));
    requestAnimationFrame(() =>
      stepList.current
        ?.querySelector(`[data-attempt="${id}"]`)
        ?.scrollIntoView({ block: "center", behavior: "smooth" }),
    );
  }
  // When the open run changes status, its row in the list follows.
  useEffect(() => {
    if (detail.data?.status) list.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detail.data?.status]);
  const declared =
    detail.data?.output && Object.keys(detail.data.output).length > 0;
  // Without declared outputs, a run's result is what its last steps produced.
  const finals = steps.filter(
    (n) =>
      n.status === "succeeded" &&
      !steps.some(
        (m) => m.started_at > n.started_at && m.status === "succeeded",
      ),
  );
  return (
    <div className={`runs-layout ${selected ? "has-detail" : ""}`}>
      <section className="panel runs-list" aria-label="Runs">
        <div className="runs-filters">
          <Field label="Status">
            <select
              value={status}
              onChange={(e) => {
                setStatus(e.target.value);
                setCursor("");
              }}
            >
              <option value="">All statuses</option>
              {[
                "pending",
                "running",
                "waiting",
                "succeeded",
                "failed",
                "cancelled",
                "timed_out",
              ].map((s) => (
                <option key={s} value={s}>
                  {s.replace("_", " ")}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Workflow">
            <input
              placeholder="Workflow name"
              value={workflow}
              onChange={(e) => {
                setWorkflow(e.target.value);
                setCursor("");
              }}
            />
          </Field>
          <button onClick={list.reload}>Refresh</button>
        </div>
        <ResourceState error={list.error} loading={!list.data} />
        <div className="run-rows">
          {list.data?.items?.map((r: RecordData) => (
            <button
              key={r.id}
              className="run-row"
              aria-current={r.id === selected || undefined}
              onClick={() => select(r.id)}
            >
              <span className="run-row-main">
                <strong>
                  {r.workflow} <small>{versionLabel(r)}</small>
                </strong>
                <small>
                  {new Date(r.created_at).toLocaleString(undefined, {
                    day: "numeric",
                    month: "short",
                    hour: "2-digit",
                    minute: "2-digit",
                  })}
                </small>
              </span>
              <Status value={r.status} />
            </button>
          ))}
        </div>
        {list.data && !list.data.items.length && (
          <Empty>No runs match these filters.</Empty>
        )}
        <div className="row">
          {cursor && <button onClick={() => setCursor("")}>Newest runs</button>}
          {list.data?.next && (
            <button onClick={() => setCursor(list.data!.next)}>
              Older runs
            </button>
          )}
        </div>
      </section>
      {selected ? (
        <section className="panel run-detail" aria-label="Run details">
          <ResourceState error={detail.error} loading={!detail.data} />
          {detail.data && (
            <>
              <div className="run-head">
                <div>
                  <h2>
                    {detail.data.workflow}{" "}
                    <small>{versionLabel(detail.data)}</small>
                  </h2>
                  <p className="mono muted">{selected}</p>
                  {detail.data.parent_run_id &&
                    ["resume", "retry"].includes(detail.data.trigger) && (
                      <p className="muted">
                        {detail.data.trigger === "resume"
                          ? "Resumed from "
                          : "Retry of "}
                        <button
                          className="link mono"
                          onClick={() => select(detail.data!.parent_run_id)}
                        >
                          {detail.data.parent_run_id.slice(0, 8)}
                        </button>
                      </p>
                    )}
                </div>
                <Status value={detail.data.status} />
                <div className="row">
                  {editable && !terminal(detail.data.status) && (
                    <AsyncButton
                      danger
                      run={async () => {
                        await api(`/runs/${selected}/cancel`, "POST", {
                          reason: "Cancelled from web UI",
                        });
                        detail.reload();
                        list.reload();
                      }}
                    >
                      Cancel run
                    </AsyncButton>
                  )}
                  {editable &&
                    ["failed", "cancelled"].includes(detail.data.status) && (
                      <AsyncButton
                        primary
                        run={async () => {
                          const r = await api(
                            `/runs/${selected}/resume`,
                            "POST",
                          );
                          select(r.id);
                          list.reload();
                        }}
                      >
                        Resume from failed step
                      </AsyncButton>
                    )}
                  {editable && terminal(detail.data.status) && (
                    <AsyncButton
                      run={async () => {
                        const r = await api(`/runs/${selected}/retry`, "POST");
                        select(r.id);
                        list.reload();
                      }}
                    >
                      Retry as new run
                    </AsyncButton>
                  )}
                  <button onClick={() => select("")}>Close</button>
                </div>
              </div>
              {detail.data.error ? (
                <div className="run-result failed">
                  <h3>Why it failed</h3>
                  <p>
                    {detail.data.error.node_id && (
                      <strong>{detail.data.error.node_id}: </strong>
                    )}
                    {detail.data.error.message}
                  </p>
                  {detail.data.error.node_code === "E_CONNECTION_REAUTH" && (
                    <p className="notice">
                      The account this step uses lost access. Reconnect it on
                      the Connectors page, then resume the run.
                    </p>
                  )}
                  {failing && (
                    <div className="row">
                      <button onClick={() => showStep(failing.id)}>
                        Show failed step
                      </button>
                      {onOpenStep && editable && !detail.data.test && (
                        <button
                          onClick={() =>
                            onOpenStep(detail.data!.workflow, failing.node_id)
                          }
                        >
                          Open step in editor
                        </button>
                      )}
                    </div>
                  )}
                </div>
              ) : declared ? (
                <div className="run-result">
                  <h3>Output</h3>
                  <JSONView value={detail.data.output} />
                </div>
              ) : (
                terminal(detail.data.status) &&
                finals.length > 0 && (
                  <div className="run-result">
                    <h3>Result of {finals.map((n) => n.node_id).join(", ")}</h3>
                    {finals.map((n) =>
                      typeof n.output?.text === "string" ? (
                        <p className="answer" key={n.id}>
                          {n.output.text}
                        </p>
                      ) : (
                        <JSONView key={n.id} value={n.output} />
                      ),
                    )}
                  </div>
                )
              )}
              {Object.keys(detail.data.input || {}).length > 0 && (
                <details className="run-input">
                  <summary>Input</summary>
                  <JSONView value={detail.data.input} />
                </details>
              )}
            </>
          )}
          <h3>Steps</h3>
          <ResourceState error={nodes.error} loading={!nodes.data} />
          <div ref={stepList}>
            {steps.map((n) => (
              <details
                className={`node-attempt ${failing?.id === n.id ? "is-failure" : ""}`}
                key={n.id}
                data-attempt={n.id}
                open={!!open[n.id]}
                onToggle={(e) => {
                  const isOpen = (e.currentTarget as HTMLDetailsElement).open;
                  setOpen((o) =>
                    o[n.id] === isOpen ? o : { ...o, [n.id]: isOpen },
                  );
                }}
              >
                <summary>
                  <span className="node-name">
                    {n.node_id}
                    {n.branch_key ? ` [${n.branch_key}]` : ""}
                  </span>
                  <small>
                    {n.node_type}
                    {n.attempt > 1 ? `, attempt ${n.attempt}` : ""}
                  </small>
                  <small className="node-facts">
                    {attemptFacts(n.metrics)}
                  </small>
                  <Status value={n.status} />
                </summary>
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
              </details>
            ))}
          </div>
          <details className="event-log">
            <summary>Event log ({events.length})</summary>
            {streamError && (
              <p className="muted" role="status">
                {streamError}
              </p>
            )}
            <ol className="timeline">
              {events.map((e) => (
                <li key={e.seq}>
                  <span className="mono muted">#{e.seq}</span>
                  <strong>{e.type}</strong>
                  <span>{e.node_id}</span>
                  <time>{new Date(e.created_at).toLocaleTimeString()}</time>
                  <details>
                    <summary>Payload</summary>
                    <JSONView value={e.payload} />
                  </details>
                </li>
              ))}
            </ol>
          </details>
        </section>
      ) : (
        <section className="panel run-empty">
          <p className="muted">
            Choose a run to see each step, its output and what it cost.
          </p>
        </section>
      )}
    </div>
  );
}
