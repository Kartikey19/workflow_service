import { useEffect, useState } from "react";
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
const eventTypes = [
  "run.created",
  "run.started",
  "run.succeeded",
  "run.failed",
  "run.cancelled",
  "run.timed_out",
  "node.scheduled",
  "node.signalled",
  "node.timed_out",
  "admin.node_retried",
  "admin.node_skipped",
  "node.started",
  "node.succeeded",
  "node.failed",
  "node.cancelled",
  "node.skipped",
  "node.waiting",
  "node.retrying",
  "node.fanout",
];
export function Runs({
  initialID,
  editable,
}: {
  initialID: string;
  editable: boolean;
}) {
  const [status, setStatus] = useState(""),
    [workflow, setWorkflow] = useState(""),
    [cursor, setCursor] = useState(""),
    [selected, select] = useState(initialID),
    [events, setEvents] = useState<RecordData[]>([]),
    [streamError, setStreamError] = useState("");
  const list = useResource(
      `/runs?limit=30&status=${enc(status)}&workflow=${enc(workflow)}&after=${enc(cursor)}`,
    ),
    detail = useResource(selected ? `/runs/${selected}` : null),
    nodes = useResource(
      selected ? `/runs/${selected}/nodes?include=input` : null,
    );
  useEffect(() => {
    if (initialID) select(initialID);
  }, [initialID]);
  const isTerminal = !!detail.data && terminal(detail.data.status);
  useEffect(() => {
    setEvents([]);
    setStreamError("");
    if (!selected) return;
    const abort = new AbortController();
    let stream: EventSource | undefined;
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    if (isTerminal) {
      api(`/runs/${selected}/events?limit=1000`, "GET", undefined, abort.signal)
        .then((r) => setEvents(r.items))
        .catch((e) => {
          if (!abort.signal.aborted) setStreamError(e.message);
        });
    } else {
      stream = new EventSource(`/be/v1/runs/${selected}/events`);
      eventTypes.forEach((type) =>
        stream!.addEventListener(type, (event) => {
          const e = JSON.parse((event as MessageEvent).data);
          setEvents((old) =>
            [...old.filter((item) => item.seq !== e.seq), e]
              .sort((a, b) => a.seq - b.seq)
              .slice(-1000),
          );
          if (!refreshTimer)
            refreshTimer = setTimeout(() => {
              refreshTimer = undefined;
              detail.reload();
              nodes.reload();
            }, 200);
        }),
      );
      stream.onerror = () =>
        setStreamError(
          "Live updates interrupted. The stream will reconnect automatically.",
        );
      stream.onopen = () => setStreamError("");
    }
    return () => {
      abort.abort();
      stream?.close();
      clearTimeout(refreshTimer);
    };
  }, [selected, isTerminal]);
  // When the open run changes status, its row in the list follows.
  useEffect(() => {
    if (detail.data?.status) list.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detail.data?.status]);
  const steps: RecordData[] = nodes.data?.items || [];
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
  const facts = (m: RecordData | null) =>
    m &&
    [
      m.duration_ms !== undefined && `${m.duration_ms} ms`,
      m.iterations &&
        `${m.iterations} model ${m.iterations === 1 ? "call" : "calls"}`,
      m.tool_calls &&
        `${m.tool_calls} tool ${m.tool_calls === 1 ? "call" : "calls"}`,
      m.tokens_in !== undefined &&
        `${m.tokens_in + (m.tokens_out || 0)} tokens`,
      m.cost_known && `$${Number(m.cost_usd).toFixed(4)}`,
    ]
      .filter(Boolean)
      .join(", ");
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
                  {r.workflow} <small>v{r.version}</small>
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
                    {detail.data.workflow} <small>v{detail.data.version}</small>
                  </h2>
                  <p className="mono muted">{selected}</p>
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
          {steps.map((n) => (
            <details className="node-attempt" key={n.id}>
              <summary>
                <span className="node-name">
                  {n.node_id}
                  {n.branch_key ? ` [${n.branch_key}]` : ""}
                </span>
                <small>
                  {n.node_type}
                  {n.attempt > 1 ? `, attempt ${n.attempt}` : ""}
                </small>
                <small className="node-facts">{facts(n.metrics)}</small>
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
