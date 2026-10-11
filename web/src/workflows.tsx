import { useEffect, useRef, useState, lazy, Suspense } from "react";
import { ChevronDown, ChevronLeft, Check, CircleAlert } from "lucide-react";
const Builder = lazy(() =>
  import("./builder").then((m) => ({ default: m.Builder })),
);
import { parse, stringify } from "yaml";
import { isBlankWorkflow, problemsFor } from "./problems";
import { api, APIError, enc, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  InputFields,
  readInput,
  AsyncButton,
  Empty,
  ResourceState,
} from "./components";
const initial =
  "apiVersion: workflow/v1\nname: my-workflow\nnodes:\n  - id: done\n    type: noop\n    config: {}\n";
export function Workflows({
  editable,
  agentName,
  onRun,
  organizationID,
}: {
  editable: boolean;
  agentName: string;
  onRun: (id: string) => void;
  organizationID: string;
}) {
  const list = useResource("/workflows?include_archived=true"),
    providers = useResource(editable ? "/providers" : null);
  const [step, setStep] = useState(""),
    [justPublished, setJustPublished] = useState(false),
    [showProblems, setShowProblems] = useState(false);
  const runPanel = useRef<HTMLElement>(null),
    checkWrap = useRef<HTMLDivElement>(null);
  const [selected, select] = useState(""),
    [source, setSource] = useState(initial),
    [mode, setMode] = useState(agentName ? "editor" : "list"),
    [workflow, setWorkflow] = useState<RecordData | null>(null);
  const [spec, setSpec] = useState<RecordData | null>(null),
    [diagnostics, setDiagnostics] = useState<RecordData[]>([]),
    [validating, setValidating] = useState(false);
  const [requirements, setRequirements] = useState(""),
    [draft, setDraft] = useState<RecordData | null>(null),
    [published, setPublished] = useState("");
  const [version, setVersion] = useState<RecordData | null>(null);
  const detail = useResource(selected ? `/workflows/${enc(selected)}` : null);
  useEffect(() => {
    if (detail.data) {
      setWorkflow(detail.data);
      setVersion(detail.data.latest);
      setSource(detail.data.latest?.yaml_source || initial);
      setSpec(detail.data.latest?.spec || null);
      setPublished(detail.data.name);
    }
  }, [detail.data]);
  useEffect(() => {
    if (mode !== "editor" || !editable) return;
    setSpec(null);
    setDiagnostics([]);
    setValidating(true);
    const abort = new AbortController();
    const timer = setTimeout(() => {
      api("/workflows?dry_run=true", "POST", { yaml: source }, abort.signal)
        .then((r) => {
          setSpec(r.spec);
          setDiagnostics(r.warnings || []);
        })
        .catch((e) => {
          if (!abort.signal.aborted)
            setDiagnostics(
              e instanceof APIError
                ? e.body.errors || [{ message: e.message }]
                : [{ message: e.message }],
            );
        })
        .finally(() => {
          if (!abort.signal.aborted) setValidating(false);
        });
    }, 450);
    return () => {
      clearTimeout(timer);
      abort.abort();
    };
  }, [source, mode, editable]);
  function startNew() {
    select("");
    setStep("");
    setPublished("");
    setWorkflow(null);
    setSource(initial);
    setMode("editor");
  }
  function view(name: string) {
    select(name);
    setStep("");
    setWorkflow(null);
    setMode("editor");
    setPublished("");
  }
  async function publish(yaml: string) {
    const r = await api("/workflows", "POST", { yaml });
    select(r.workflow.name);
    setPublished(r.workflow.name);
    setJustPublished(true);
    list.reload();
    detail.reload();
    return r;
  }
  const docName = (() => {
    try {
      return String(parse(source)?.name || "");
    } catch {
      return "";
    }
  })();
  const problems = problemsFor(source, diagnostics),
    blocking = problems.filter((p) => !p.warning).length || 1,
    blank = isBlankWorkflow(source);
  useEffect(() => {
    if (!showProblems) return;
    const away = (e: PointerEvent) => {
      if (!checkWrap.current?.contains(e.target as Node))
        setShowProblems(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [showProblems]);
  // After publishing, bring the run form into view so the next step is obvious.
  useEffect(() => {
    if (justPublished && runPanel.current) {
      runPanel.current.scrollIntoView({ block: "center", behavior: "smooth" });
      setJustPublished(false);
    }
  }, [justPublished, version, published]);
  function rename(name: string) {
    try {
      const doc = parse(source);
      if (doc && typeof doc === "object" && name && name !== doc.name) {
        setSource(stringify({ ...doc, name }));
        setPublished("");
      }
    } catch {
      /* The builder reports an unreadable definition. */
    }
  }
  const when = (value?: string) =>
    value
      ? new Date(value).toLocaleDateString(undefined, {
          day: "numeric",
          month: "short",
        })
      : "—";
  return (
    <>
      {mode === "list" && (
        <section className="overview">
          <div className="toolbar">
            <p className="muted">
              Each workflow keeps every published version, ready to run.
            </p>
            {editable && (
              <div className="row">
                <button onClick={() => setMode("assistant")}>
                  Ask assistant
                </button>
                <button className="primary" onClick={startNew}>
                  New workflow
                </button>
              </div>
            )}
          </div>
          <ResourceState error={list.error} loading={!list.data} />
          {!!list.data?.items?.length && (
            <div className="panel table-list">
              <div className="table-row table-headings" aria-hidden="true">
                <span>Name</span>
                <span>Version</span>
                <span>Runs</span>
                <span>Last run</span>
              </div>
              {list.data.items.map((w: RecordData) => (
                <button
                  key={w.name}
                  className="table-row"
                  onClick={() => view(w.name)}
                >
                  <span className="table-name">
                    <strong>{w.name}</strong>
                    <small>
                      {w.archived_at
                        ? "Archived"
                        : `Published ${when(w.latest_published_at)}`}
                    </small>
                  </span>
                  <span>v{w.latest_version}</span>
                  <span>{w.run_count}</span>
                  <span>{when(w.last_run_at)}</span>
                </button>
              ))}
            </div>
          )}
          {list.data && !list.data.items.length && (
            <Empty>
              No workflows yet. Choose New workflow and add your first step.
            </Empty>
          )}
        </section>
      )}
      {mode === "assistant" && (
        <section className="panel workspace">
          <button className="link back" onClick={() => setMode("list")}>
            <ChevronLeft size={16} aria-hidden="true" />
            All workflows
          </button>
          <h2>Describe what you want to automate</h2>
          <p className="muted">
            The assistant drafts and checks a workflow using the connectors and
            agents in this organization. You review it before publishing.
          </p>
          <ActionForm
            label="Generate draft"
            submit={async (data) => {
              let r: RecordData;
              try {
                r = await api("/assistant/workflows", "POST", {
                  requirements,
                  connection: data.get("connection"),
                  model: data.get("model"),
                });
              } catch (e) {
                if (e instanceof APIError && e.status === 422 && e.body.yaml)
                  r = e.body;
                else throw e;
              }
              setDraft(r);
              setSource(r.yaml);
              setPublished("");
              select("");
              setWorkflow(null);
              setMode("editor");
            }}
          >
            <Field label="Requirements">
              <textarea
                required
                rows={7}
                value={requirements}
                onChange={(e) => setRequirements(e.target.value)}
                placeholder="Classify incoming tickets with my triage agent, then email urgent issues to the support team."
              />
            </Field>
            <Field label="Provider">
              <select name="connection">
                <option value="">Platform assistant</option>
                {providers.data?.items.map((p: RecordData) => (
                  <option key={p.id}>{p.name}</option>
                ))}
              </select>
            </Field>
            <Field
              label="Model"
              hint="Leave blank to use the platform assistant model."
            >
              <input name="model" />
            </Field>
          </ActionForm>
        </section>
      )}
      {mode === "editor" && (
        <section className="workspace editing">
          <div className="editor-bar">
            <div className="editor-title">
              <button
                className="icon-button"
                aria-label="All workflows"
                title="All workflows"
                onClick={() => {
                  setMode("list");
                  select("");
                  setStep("");
                  setWorkflow(null);
                  setPublished("");
                }}
              >
                <ChevronLeft size={18} aria-hidden="true" />
              </button>
              <input
                className="title-input"
                aria-label="Workflow name"
                key={docName}
                defaultValue={docName}
                readOnly={!editable}
                placeholder="Name this workflow"
                onBlur={(e) => rename(e.target.value.trim())}
                onKeyDown={(e) => {
                  if (e.key === "Enter") e.currentTarget.blur();
                }}
              />
            </div>
            <div className="editor-actions">
              {workflow?.versions && (
                <select
                  aria-label="Version"
                  value={version?.version || workflow.latest?.version}
                  onChange={async (e) => {
                    try {
                      const r = await api(
                        `/workflows/${enc(selected)}/versions/${e.target.value}`,
                      );
                      setSource(r.yaml_source);
                      setSpec(r.spec);
                      setVersion(r);
                      setPublished(workflow.name);
                    } catch (e) {
                      setDiagnostics([{ message: (e as Error).message }]);
                    }
                  }}
                >
                  {workflow.versions.map((v: RecordData) => (
                    <option key={v.version} value={v.version}>
                      Version {v.version}
                    </option>
                  ))}
                </select>
              )}
              <div className="check-wrap" ref={checkWrap}>
                <div
                  className={`check-state ${validating || blank ? "" : spec ? "ok" : "bad"}`}
                  role="status"
                >
                  {validating ? (
                    "Checking…"
                  ) : blank ? (
                    "Add a step to begin"
                  ) : spec ? (
                    <>
                      <Check size={15} aria-hidden="true" />
                      Valid workflow
                    </>
                  ) : (
                    <button
                      className="link"
                      aria-expanded={showProblems}
                      onClick={() => setShowProblems(!showProblems)}
                    >
                      <CircleAlert size={15} aria-hidden="true" />
                      {blocking} {blocking === 1 ? "problem" : "problems"} to
                      fix
                      <ChevronDown size={14} aria-hidden="true" />
                    </button>
                  )}
                </div>
                {!spec && !validating && showProblems && (
                  <ul className="problems pop" aria-label="Problems">
                    {problems.map((p, i) => (
                      <li key={i} className={p.warning ? "warning" : "error"}>
                        {p.step ? (
                          <button
                            className="link"
                            onClick={() => {
                              setStep(p.step!);
                              setShowProblems(false);
                            }}
                          >
                            {p.step}
                          </button>
                        ) : null}
                        <span>{p.message}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
              {workflow && editable && (
                <AsyncButton
                  run={async () => {
                    await api(
                      `/workflows/${enc(workflow.name)}${workflow.archived_at ? "/restore" : ""}`,
                      workflow.archived_at ? "POST" : "DELETE",
                    );
                    list.reload();
                    detail.reload();
                  }}
                >
                  {workflow.archived_at ? "Restore" : "Archive"}
                </AsyncButton>
              )}
              {editable && (
                <AsyncButton
                  primary
                  disabled={!spec || validating || blank}
                  run={() => publish(source)}
                >
                  Publish version
                </AsyncButton>
              )}
            </div>
          </div>
          <ResourceState
            error={detail.error}
            loading={!!selected && !detail.data}
          />
          {draft && (
            <p className="muted">
              Assistant draft · {draft.attempts} attempts ·{" "}
              {draft.tokens_in + draft.tokens_out} tokens ·{" "}
              {draft.cost_known
                ? `$${draft.cost_usd.toFixed(4)}`
                : "Price unavailable"}
            </p>
          )}
          <Suspense fallback={<p>Opening editor…</p>}>
            <Builder
              key={`${selected}:${version?.version}`}
              organizationID={organizationID}
              source={source}
              editable={editable}
              selected={step}
              onSelect={setStep}
              problems={problems}
              starter={agentName && !selected ? `agent:${agentName}` : ""}
              onChange={(value) => {
                setSource(value);
                setPublished("");
              }}
            />
          </Suspense>
          {published &&
            workflow &&
            version &&
            source === version.yaml_source && (
              <section className="subsection" ref={runPanel}>
                <h3>Run {published}</h3>
                <ActionForm
                  label="Start run"
                  disabled={!editable || !!workflow.archived_at}
                  submit={async (data) => {
                    const r = await api("/runs", "POST", {
                      workflow: published,
                      version: version.version,
                      input: Object.keys(version.spec.inputs || {}).length
                        ? readInput(data, version.spec.inputs)
                        : JSON.parse(String(data.get("input") || "{}")),
                    });
                    onRun(r.id);
                  }}
                >
                  {Object.keys(version.spec.inputs || {}).length ? (
                    <InputFields schema={version.spec.inputs} />
                  ) : (
                    <Field label="Input (JSON)">
                      <textarea name="input" defaultValue="{}" />
                    </Field>
                  )}
                </ActionForm>
              </section>
            )}
        </section>
      )}
    </>
  );
}
