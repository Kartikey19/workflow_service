import { useEffect, useState } from "react";
import { api, terminal, type RecordData } from "./api";
import { useResource } from "./components";

export const eventTypes = [
  "run.created",
  "run.started",
  "run.succeeded",
  "run.failed",
  "run.cancelled",
  "run.timed_out",
  "node.scheduled",
  "node.signalled",
  "node.timed_out",
  "node.reused",
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

// Follows one run: its detail and step attempts, plus the event log, streamed
// while the run is live and loaded once it has finished.
export function useRunWatch(runID: string) {
  const [events, setEvents] = useState<RecordData[]>([]),
    [streamError, setStreamError] = useState("");
  const detail = useResource(runID ? `/runs/${runID}` : null),
    nodes = useResource(runID ? `/runs/${runID}/nodes?include=input` : null);
  const isTerminal = !!detail.data && terminal(detail.data.status);
  useEffect(() => {
    setEvents([]);
    setStreamError("");
    if (!runID) return;
    const abort = new AbortController();
    let stream: EventSource | undefined;
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    if (isTerminal) {
      api(`/runs/${runID}/events?limit=1000`, "GET", undefined, abort.signal)
        .then((r) => {
          if (!abort.signal.aborted) setEvents(r.items);
        })
        .catch((e) => {
          if (!abort.signal.aborted) setStreamError(e.message);
        });
    } else {
      stream = new EventSource(`/be/v1/runs/${runID}/events`);
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [runID, isTerminal]);
  // The final node states can land after the terminal event: load them once.
  useEffect(() => {
    if (isTerminal) nodes.reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isTerminal]);
  const steps: RecordData[] = nodes.data?.items || [];
  return { detail, nodes, steps, events, streamError, isTerminal };
}

// The latest attempt of every step outside foreach branches, by step name.
export function latestByStep(steps: RecordData[]) {
  const latest: Record<string, RecordData> = {};
  for (const n of steps) {
    if (n.branch_key) continue;
    if (!latest[n.node_id] || latest[n.node_id].attempt < n.attempt)
      latest[n.node_id] = n;
  }
  return latest;
}

// The step that ended the run, or the first failed step.
export function failedStep(run: RecordData | null, steps: RecordData[]) {
  const latest = latestByStep(steps);
  const named = run?.error?.node_id && latest[run.error.node_id];
  return (
    named || Object.values(latest).find((n) => n.status === "failed") || null
  );
}

// Facts about one attempt: how long it took and what it cost.
export function attemptFacts(m: RecordData | null) {
  if (!m) return "";
  if (m.reused_from_run) return "reused from an earlier run";
  return [
    m.duration_ms !== undefined && `${m.duration_ms} ms`,
    m.iterations &&
      `${m.iterations} model ${m.iterations === 1 ? "call" : "calls"}`,
    m.tool_calls &&
      `${m.tool_calls} tool ${m.tool_calls === 1 ? "call" : "calls"}`,
    m.tokens_in !== undefined && `${m.tokens_in + (m.tokens_out || 0)} tokens`,
    m.cost_known && `$${Number(m.cost_usd).toFixed(4)}`,
  ]
    .filter(Boolean)
    .join(", ");
}
