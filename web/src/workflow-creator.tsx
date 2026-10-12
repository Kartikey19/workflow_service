import { useEffect, useRef, useState } from "react";
import { parse, stringify } from "yaml";
import { api, APIError, type RecordData } from "./api";
import { Field } from "./components";
import { SetupModal } from "./setup";

export function WorkflowCreator({
  source,
  onAccept,
  onClose,
}: {
  source: string;
  onAccept: (yaml: string, id: string) => void;
  onClose: () => void;
}) {
  const [approved, setApproved] = useState(source);
  const [requirements, setRequirements] = useState("");
  const [guidance, setGuidance] = useState("");
  const [proposal, setProposal] = useState<RecordData | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef<AbortController | null>(null);
  useEffect(() => () => request.current?.abort(), []);
  const accepted = (() => {
    try {
      return approved ? parse(approved)?.nodes?.length || 0 : 0;
    } catch {
      return 0;
    }
  })();
  const node = proposal?.step && parse(proposal.yaml)?.nodes?.at(-1);
  async function propose() {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setBusy(true);
    setError("");
    setProposal(null);
    try {
      let result: RecordData;
      try {
        result = await api(
          "/assistant/workflows",
          "POST",
          {
            requirements,
            step_instructions: guidance,
            stepwise: true,
            approved_yaml: approved,
          },
          controller.signal,
        );
      } catch (e) {
        if (
          e instanceof APIError &&
          e.status === 422 &&
          e.body.yaml !== undefined
        )
          result = e.body;
        else throw e;
      }
      if (!controller.signal.aborted) setProposal(result);
    } catch (e) {
      if (!controller.signal.aborted)
        setError(e instanceof Error ? e.message : "Could not propose a step.");
    } finally {
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <SetupModal title="Create with AI" onClose={onClose}>
      <p className="muted">GLM 5.3 Flash · OpenRouter</p>
      <p>
        Describe the whole workflow. Review and accept one step at a time before
        asking for the next.
      </p>
      <p className="muted">
        Stored credentials and existing step values stay private. Your
        description is sent to OpenRouter; use account or secret names, and keep
        passwords and keys out of it.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          void propose();
        }}
      >
        <Field
          label="Workflow context"
          hint="Include the goal, inputs, outputs, and which accounts or agents to use."
        >
          <textarea
            required
            maxLength={16000}
            rows={5}
            value={requirements}
            disabled={busy}
            onChange={(e) => {
              setRequirements(e.target.value);
              setProposal(null);
            }}
            placeholder="When a support ticket arrives, classify its urgency and notify Slack for urgent tickets…"
          />
        </Field>
        <Field
          label="This step"
          hint="Optional instructions, or feedback to revise the pending proposal."
        >
          <textarea
            rows={2}
            maxLength={4000}
            value={guidance}
            disabled={busy}
            onChange={(e) => {
              setGuidance(e.target.value);
              setProposal(null);
            }}
          />
        </Field>
        <div className="row spread">
          <span className="muted" role="status">
            {accepted} accepted {accepted === 1 ? "step" : "steps"}
          </span>
          <button className="primary" disabled={busy || !requirements.trim()}>
            {busy
              ? "Proposing…"
              : proposal
                ? "Revise proposal"
                : accepted
                  ? "Propose next step"
                  : "Propose first step"}
          </button>
        </div>
      </form>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {proposal && (
        <section className="creator-proposal" aria-label="Proposed step">
          <h3>
            {node ? `${node.id} · ${node.type}` : "Proposal needs revision"}
          </h3>
          {node && <pre>{stringify(node)}</pre>}
          {(proposal.diagnostics || []).map((d: RecordData, i: number) => (
            <p key={i} className={d.severity === "warning" ? "muted" : "error"}>
              {d.message}
            </p>
          ))}
          <p className="muted">
            {proposal.valid
              ? "Validated. Accept to add it to your unpublished draft."
              : "This proposal did not pass validation. Add feedback and try again."}
          </p>
          <div className="row">
            <button
              type="button"
              className="primary"
              disabled={!proposal.valid || busy}
              onClick={() => {
                onAccept(proposal.yaml, proposal.step.id);
                setApproved(proposal.yaml);
                setProposal(null);
                setGuidance("");
              }}
            >
              Accept step
            </button>
            <button
              type="button"
              onClick={() => setProposal(null)}
              disabled={busy}
            >
              Discard proposal
            </button>
          </div>
        </section>
      )}
      <div className="row creator-finish">
        <button type="button" onClick={onClose}>
          {accepted ? "Finish editing" : "Close"}
        </button>
        <span className="muted">Nothing runs or publishes here.</span>
      </div>
    </SetupModal>
  );
}
