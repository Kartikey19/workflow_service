import { useEffect, useRef, useState } from "react";
import { parse } from "yaml";
import { Send } from "lucide-react";
import { api, APIError, type RecordData } from "./api";
import { Field, useResource } from "./components";
import { StepSummary, stepLabel } from "./step-summary";
import { isBlankWorkflow } from "./problems";

export type WorkflowPreview = { yaml: string; base: string; nodeID: string };

type Entry = {
  id: number;
  role: "user" | "assistant";
  text?: string;
  draft?: RecordData;
  base?: string;
  state?: "pending" | "accepted" | "discarded" | "superseded";
};

export function WorkflowChat({
  source,
  active,
  onAccept,
  onEdit,
  onPreview,
}: {
  source: string;
  active: boolean;
  onAccept: (yaml: string, id: string, base: string) => boolean;
  onEdit: (id?: string) => void;
  onPreview: (preview: WorkflowPreview | null) => void;
}) {
  const executors = useResource("/executors"),
    connections = useResource("/connections");
  const previewCallback = useRef(onPreview);
  previewCallback.current = onPreview;
  const [entries, setEntries] = useState<Entry[]>([]);
  const [context, setContext] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  const nextID = useRef(0);
  const messages = useRef<HTMLDivElement>(null);
  const latestMessage = useRef<HTMLElement>(null);
  useEffect(() => () => request.current?.abort(), []);
  useEffect(() => {
    if (!active || !messages.current) return;
    // Start long review cards at their heading; keep messages and progress at the end.
    messages.current.scrollTop =
      entries.at(-1)?.draft && !busy
        ? latestMessage.current?.offsetTop || 0
        : messages.current.scrollHeight;
  }, [entries, busy, active]);
  let accepted = 0;
  try {
    if (!isBlankWorkflow(source)) accepted = parse(source)?.nodes?.length || 0;
  } catch {
    /* The manual editor reports unreadable YAML. */
  }
  const pending = entries.find((entry) => entry.state === "pending");
  useEffect(() => {
    previewCallback.current(
      pending?.draft?.valid && pending.base === source
        ? {
            yaml: pending.draft.yaml,
            base: source,
            nodeID: pending.draft.step.id,
          }
        : null,
    );
  }, [pending, source]);
  useEffect(() => () => previewCallback.current(null), []);
  function settle(id: number, state: Entry["state"]) {
    setEntries((current) =>
      current.map((entry) => (entry.id === id ? { ...entry, state } : entry)),
    );
  }
  async function propose(text: string, next = false) {
    const requirements = next
      ? context
      : [context, text].filter(Boolean).join("\n\nFollow-up:\n");
    if (new TextEncoder().encode(requirements).length > 16000) {
      setEntries((current) => [
        ...current,
        {
          id: nextID.current++,
          role: "assistant",
          text: "This conversation has reached the 16,000-byte context limit. Finish editing this draft in Steps, or start a new workflow.",
        },
      ]);
      return;
    }
    const base = source;
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setMessage("");
    setContext(requirements);
    setEntries((current) => [
      ...current.map((entry) =>
        entry.state === "pending"
          ? { ...entry, state: "superseded" as const }
          : entry,
      ),
      { id: nextID.current++, role: "user", text },
    ]);
    try {
      let draft: RecordData;
      try {
        draft = await api(
          "/assistant/workflows",
          "POST",
          {
            requirements,
            step_instructions: next ? text : "",
            stepwise: true,
            approved_yaml: isBlankWorkflow(base) ? "" : base,
          },
          controller.signal,
        );
      } catch (e) {
        if (
          e instanceof APIError &&
          e.status === 422 &&
          e.body.yaml !== undefined
        )
          draft = e.body;
        else throw e;
      }
      if (!controller.signal.aborted)
        setEntries((current) => [
          ...current,
          {
            id: nextID.current++,
            role: "assistant",
            draft,
            base,
            state: "pending",
          },
        ]);
    } catch (e) {
      if (!controller.signal.aborted)
        setEntries((current) => [
          ...current,
          {
            id: nextID.current++,
            role: "assistant",
            text:
              e instanceof Error
                ? e.message
                : "Could not propose a step. Send a message to try again.",
          },
        ]);
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <section className="workflow-chat" aria-label="Workflow chat">
      <header className="chat-header row spread">
        <span className="muted" role="status">
          {accepted} accepted {accepted === 1 ? "step" : "steps"}
        </span>
      </header>
      <div
        ref={messages}
        className="chat-messages"
        role="log"
        aria-label="Conversation"
        aria-live="polite"
        aria-busy={busy}
      >
        {!entries.length && (
          <div className="chat-welcome">
            <h2>What should this workflow do?</h2>
            <p>
              Give the full context: the goal, inputs, outputs, and accounts to
              use. We’ll build one step at a time, with your review.
            </p>
            {accepted > 0 && (
              <p>
                Your {accepted} existing steps are available in Steps and
                Visualization.
              </p>
            )}
            <button type="button" className="link" onClick={() => onEdit()}>
              Edit steps manually
            </button>
          </div>
        )}
        {entries.map((entry, index) => {
          const node =
            entry.draft?.step && parse(entry.draft.yaml)?.nodes?.at(-1);
          const stale = entry.state === "pending" && entry.base !== source;
          return (
            <article
              ref={index === entries.length - 1 ? latestMessage : undefined}
              key={entry.id}
              className={`chat-message chat-${entry.role}`}
              aria-label={
                entry.role === "user" ? "Your message" : "Weft response"
              }
            >
              <small className="muted">
                {entry.role === "user" ? "You" : "Weft"}
              </small>
              {entry.text && <p>{entry.text}</p>}
              {entry.draft && (
                <section className="chat-step" aria-label="Proposed step">
                  <div className="row spread">
                    <h3>
                      {node ? stepLabel(node.id) : "Proposal needs revision"}
                    </h3>
                    <span className="chat-step-state">
                      {entry.state === "pending"
                        ? "Review"
                        : entry.state === "accepted"
                          ? "Accepted"
                          : entry.state === "discarded"
                            ? "Discarded"
                            : "Revised"}
                    </span>
                  </div>
                  {node && (
                    <StepSummary
                      node={node}
                      title={
                        executors.data?.items?.find(
                          (e: RecordData) => e.type === node.type,
                        )?.title || stepLabel(node.type)
                      }
                      connectionKind={
                        connections.data?.items?.find(
                          (c: RecordData) => c.name === node.config?.connection,
                        )?.kind
                      }
                      inputs={
                        entry.base && isBlankWorkflow(entry.base)
                          ? parse(entry.draft.yaml)?.inputs
                          : undefined
                      }
                    />
                  )}
                  {entry.state === "accepted" && (
                    <button
                      type="button"
                      className="link"
                      onClick={() => onEdit(node.id)}
                    >
                      Edit step
                    </button>
                  )}
                  {(entry.draft.diagnostics || []).map(
                    (d: RecordData, i: number) => (
                      <p
                        key={i}
                        className={d.severity === "warning" ? "muted" : "error"}
                      >
                        {d.message}
                      </p>
                    ),
                  )}
                  {entry.state === "pending" && (
                    <>
                      <p className={stale ? "error" : "muted"}>
                        {stale
                          ? "The workflow changed after this proposal. Ask for a fresh proposal to keep your edits."
                          : entry.draft.valid
                            ? "Validated. Accept to add this step to your draft, or send feedback below."
                            : "This proposal did not pass validation. Send feedback below to revise it."}
                      </p>
                      <div className="row">
                        <button
                          type="button"
                          className="primary"
                          disabled={!entry.draft.valid || stale || busy}
                          onClick={() => {
                            if (
                              !onAccept(
                                entry.draft!.yaml,
                                entry.draft!.step.id,
                                entry.base!,
                              )
                            )
                              return;
                            settle(entry.id, "accepted");
                          }}
                        >
                          Accept step
                        </button>
                        <button
                          type="button"
                          disabled={busy}
                          onClick={() => settle(entry.id, "discarded")}
                        >
                          Discard proposal
                        </button>
                      </div>
                    </>
                  )}
                </section>
              )}
            </article>
          );
        })}
        {busy && (
          <p className="muted" role="status">
            Proposing a step…
          </p>
        )}
      </div>
      <footer className="chat-composer">
        {context && !pending && (
          <button
            type="button"
            disabled={busy}
            onClick={() => void propose("Propose the next step.", true)}
          >
            Propose next step
          </button>
        )}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (message.trim() && !busy) void propose(message.trim());
          }}
        >
          <Field label="Message">
            <textarea
              rows={2}
              maxLength={context ? 4000 : 16000}
              value={message}
              disabled={busy}
              onChange={(e) => setMessage(e.target.value)}
              placeholder={
                context
                  ? "Tell Weft what to change or add…"
                  : "Describe the workflow you want to build…"
              }
            />
          </Field>
          <button
            type="submit"
            className="primary"
            disabled={busy || !message.trim()}
          >
            <Send size={16} aria-hidden="true" /> Send
          </button>
        </form>
        <p className="muted">
          Use account and secret names. Messages go to OpenRouter; stored
          credentials and existing step values stay private. Steps remain
          unpublished until you publish.
        </p>
      </footer>
    </section>
  );
}
