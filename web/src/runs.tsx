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
      stream = new EventSource(`/v1/runs/${selected}/events`);
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
  return (
    <>
      <div className="toolbar">
        <div className="row">
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
                <option key={s}>{s}</option>
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
        </div>
        <button onClick={list.reload}>Refresh</button>
      </div>
      <section className="panel">
        <ResourceState error={list.error} loading={!list.data} />
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Workflow</th>
                <th>Status</th>
                <th>Started</th>
                <th>Run ID</th>
              </tr>
            </thead>
            <tbody>
              {list.data?.items?.map((r: RecordData) => (
                <tr key={r.id}>
                  <td>
                    <button className="link" onClick={() => select(r.id)}>
                      {r.workflow} <small>v{r.version}</small>
                    </button>
                  </td>
                  <td>
                    <Status value={r.status} />
                  </td>
                  <td>{new Date(r.created_at).toLocaleString()}</td>
                  <td className="mono">{r.id.slice(0, 8)}</td>
                </tr>
              ))}
            </tbody>
          </table>
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
      {selected && (
        <section className="panel run-detail">
          <div className="row spread">
            <h2>Run details</h2>
            <button onClick={() => select("")}>Close</button>
          </div>
          <ResourceState error={detail.error} loading={!detail.data} />
          {detail.data && (
            <>
              <div className="row">
                <strong>
                  {detail.data.workflow} v{detail.data.version}
                </strong>
                <Status value={detail.data.status} />
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
              </div>
              <p className="mono muted">{selected}</p>
              <div className="split">
                <div>
                  <h3>Input</h3>
                  <JSONView value={detail.data.input} />
                </div>
                <div>
                  <h3>Output</h3>
                  <JSONView value={detail.data.output || detail.data.error} />
                </div>
              </div>
            </>
          )}
          <h3>Node attempts</h3>
          <ResourceState error={nodes.error} loading={!nodes.data} />
          {nodes.data?.items?.map((n: RecordData) => (
            <details className="node-attempt" key={n.id}>
              <summary>
                <span className="mono">
                  {n.node_id}
                  {n.branch_key ? ` [${n.branch_key}]` : ""}
                </span>{" "}
                · {n.node_type} · attempt {n.attempt}{" "}
                <Status value={n.status} />
              </summary>
              <div className="split">
                <div>
                  <h4>Input</h4>
                  <JSONView value={n.input} />
                </div>
                <div>
                  <h4>Output / error</h4>
                  <JSONView value={n.error || n.output} />
                </div>
              </div>
              <JSONView value={n.metrics} />
            </details>
          ))}
          <h3>Event timeline</h3>
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
        </section>
      )}
    </>
  );
}
