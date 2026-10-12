import { useEffect, useRef, useState, lazy, Suspense } from "react";
import {
  ChevronDown,
  ChevronLeft,
  Check,
  CircleAlert,
  Undo2,
  Redo2,
} from "lucide-react";
const Builder = lazy(() =>
  import("./builder").then((m) => ({ default: m.Builder })),
);
import { parse, stringify } from "yaml";
import { WorkflowCreator } from "./workflow-creator";
import { useWorkflowDraft } from "./use-workflow-draft";
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
  admin,
  agentName,
  onRun,
  organizationID,
  userID,
}: {
  editable: boolean;
  admin: boolean;
  agentName: string;
  onRun: (id: string) => void;
  organizationID: string;
  userID: string;
}) {
  const list = useResource("/workflows?include_archived=true");
  const [step, setStep] = useState(""),
    [justPublished, setJustPublished] = useState(false),
    [showProblems, setShowProblems] = useState(false);
  const runPanel = useRef<HTMLElement>(null),
    checkWrap = useRef<HTMLDivElement>(null);
  const { editor, state: editing } = useWorkflowDraft(
    organizationID,
    userID,
    initial,
    editable,
    !!agentName,
  );
  const selected = editing.open ? editing.slot : "";
  const source = editing.source;
  const [mode, setMode] = useState(editing.open ? "editor" : "list"),
    [workflow, setWorkflow] = useState<RecordData | null>(null),
    [editorMode, setEditorMode] = useState("canvas"),
    [starterAgent, setStarterAgent] = useState(agentName);
  const [spec, setSpec] = useState<RecordData | null>(null),
    [diagnostics, setDiagnostics] = useState<RecordData[]>([]),
    [validating, setValidating] = useState(false);
  const [creator, setCreator] = useState<{ source: string } | null>(null);
  const [version, setVersion] = useState<RecordData | null>(null);
  const detail = useResource(selected ? `/workflows/${enc(selected)}` : null);
  useEffect(() => {
    if (
      detail.data?.name === selected &&
      (detail.data.latest?.version || 0) >= editor.getSnapshot().baseVersion
    ) {
      setWorkflow(detail.data);
      setVersion(detail.data.latest);
      editor.load(
        selected,
        detail.data.latest?.yaml_source || initial,
        detail.data.latest?.version || 0,
      );
      setSpec(detail.data.latest?.spec || null);
    }
  }, [detail.data, selected, editor]);
  useEffect(() => {
    if (mode !== "editor" || !editable) return;
    // Keep the last result while checking an edit: blurring a pending field
    // must not disable Publish before its click fires. The server validates
    // the current draft again when publishing.
    setDiagnostics([]);
    setValidating(true);
    const abort = new AbortController();
    const timer = setTimeout(() => {
      api("/workflows?dry_run=true", "POST", { yaml: source }, abort.signal)
        .then((r) => {
          if (!abort.signal.aborted) {
            setSpec(r.spec);
            setDiagnostics(r.warnings || []);
          }
        })
        .catch((e) => {
          if (!abort.signal.aborted) {
            setSpec(null);
            setDiagnostics(
              e instanceof APIError
                ? e.body.errors || [{ message: e.message }]
                : [{ message: e.message }],
            );
          }
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
    editor.begin("");
    setStep("");

    setWorkflow(null);
    setVersion(null);
    setStarterAgent("");
    setMode("editor");
  }
  function view(name: string) {
    editor.begin(name);
    setStep("");
    setWorkflow(null);
    setVersion(null);
    setMode("editor");
  }
  async function publish(yaml: string) {
    const { session, slot } = editor.getSnapshot();
    const r = await api("/workflows", "POST", { yaml });
    const canonical = r.unchanged
      ? (await api(`/workflows/${enc(r.workflow.name)}/versions/${r.version}`))
          .yaml_source
      : yaml;
    const stillEditing =
      editor.getSnapshot().session === session && editor.getSnapshot().open;
    editor.published(
      session,
      slot,
      r.workflow.name,
      yaml,
      r.version,
      canonical,
    );
    if (stillEditing) {
      setJustPublished(true);
      detail.reload();
    }
    list.reload();
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
  }, [justPublished, version]);
  function rename(name: string) {
    try {
      const doc = parse(source);
      if (doc && typeof doc === "object" && name !== doc.name) {
        editor.change(stringify({ ...doc, name }), "name");
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
      {creator && (
        <WorkflowCreator
          source={creator.source}
          onClose={() => setCreator(null)}
          onAccept={(yaml, id) => {
            if (mode === "list") {
              editor.begin("");
              setWorkflow(null);
              setVersion(null);
              setStarterAgent("");
            }
            editor.change(yaml);
            setStep(id);
            setMode("editor");
          }}
        />
      )}
      {mode === "list" && (
        <section className="overview">
          <div className="toolbar">
            <p className="muted">
              Each workflow keeps every published version, ready to run.
            </p>
            {editable && (
              <div className="row">
                <button onClick={() => setCreator({ source: "" })}>
                  Create with AI
                </button>
                <button className="primary" onClick={startNew}>
                  {Object.hasOwn(editing.drafts, "")
                    ? "Resume draft"
                    : "New workflow"}
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
                    {editable && Object.hasOwn(editing.drafts, w.name) && (
                      <small>Unpublished draft</small>
                    )}
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
                  editor.close();
                  setStep("");
                  setWorkflow(null);
                }}
              >
                <ChevronLeft size={18} aria-hidden="true" />
              </button>
              <input
                className="title-input"
                aria-label="Workflow name"
                value={docName}
                onChange={(e) => rename(e.target.value)}
                readOnly={!editable}
                placeholder="Name this workflow"
                onBlur={(e) => rename(e.target.value.trim())}
                onKeyDown={(e) => {
                  if (e.key === "Enter") e.currentTarget.blur();
                }}
              />
            </div>
            <div className="editor-actions">
              {editable && (
                <button
                  onClick={() => setCreator({ source: blank ? "" : source })}
                >
                  Create with AI
                </button>
              )}
              {editable && (
                <div className="edit-history" aria-label="Edit history">
                  <button
                    className="icon-button"
                    aria-label="Undo"
                    title="Undo (Ctrl/Cmd+Z outside text fields)"
                    disabled={!editing.canUndo}
                    onClick={() => editor.move(-1)}
                  >
                    <Undo2 size={16} aria-hidden="true" />
                  </button>
                  <button
                    className="icon-button"
                    aria-label="Redo"
                    title="Redo (Ctrl/Cmd+Shift+Z outside text fields)"
                    disabled={!editing.canRedo}
                    onClick={() => editor.move(1)}
                  >
                    <Redo2 size={16} aria-hidden="true" />
                  </button>
                </div>
              )}
              <div className="editor-view" aria-label="Editor view">
                <button
                  aria-pressed={editorMode === "canvas"}
                  onClick={() => setEditorMode("canvas")}
                >
                  Canvas
                </button>
                <button
                  aria-pressed={editorMode === "yaml"}
                  onClick={() => setEditorMode("yaml")}
                >
                  YAML
                </button>
              </div>
              {workflow?.versions && (
                <select
                  aria-label="Version"
                  value={version?.version || workflow.latest?.version}
                  onChange={async (e) => {
                    const session = editing.session;
                    const source = editor.getSnapshot().source;
                    try {
                      const r = await api(
                        `/workflows/${enc(selected)}/versions/${e.target.value}`,
                      );
                      if (
                        editor.getSnapshot().session !== session ||
                        editor.getSnapshot().source !== source ||
                        !editor.getSnapshot().open
                      )
                        return;
                      editor.replace(r.yaml_source);
                      setSpec(r.spec);
                      setVersion(r);
                    } catch (e) {
                      if (editor.getSnapshot().session === session)
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
                  disabled={!spec || blank}
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

          {editable && (
            <div className="draft-state">
              <span role="status">
                {editing.dirty
                  ? editing.saveError
                    ? "Draft could not be saved on this browser"
                    : "Draft saved on this browser"
                  : "No unpublished changes"}
              </span>
              {editing.dirty && (
                <button className="link" onClick={() => editor.revert()}>
                  Revert changes
                </button>
              )}
            </div>
          )}
          {editing.restored && (
            <p className="draft-notice" role="status">
              Recovered your unpublished draft. Review it before publishing.
            </p>
          )}
          {editing.dirty && editing.conflict && (
            <p className="draft-notice" role="alert">
              A newer version was published since this draft started. Your
              changes are preserved; review the latest version before
              publishing.
            </p>
          )}
          {editing.dirty && editing.saveError && (
            <p className="error" role="alert">
              {editing.saveError === "changed"
                ? "Drafts changed in another tab. Copy your YAML, then reload to review the saved drafts."
                : "Browser storage is unavailable or full. Copy your YAML before leaving this page."}{" "}
              <button className="link" onClick={() => setEditorMode("yaml")}>
                Show YAML
              </button>
            </p>
          )}
          {editorMode === "yaml" ? (
            <Field
              label="Workflow YAML"
              hint="Unpublished drafts are stored on this browser and may include prompts or sample data. Account credentials are stored separately."
            >
              <textarea
                className="code workflow-yaml"
                spellCheck={false}
                readOnly={!editable}
                value={source}
                onChange={(e) => {
                  editor.change(e.target.value, "yaml");
                }}
              />
            </Field>
          ) : (
            <Suspense fallback={<p>Opening editor…</p>}>
              <Builder
                key={`${editing.session}:${editing.revision}`}
                organizationID={organizationID}
                source={source}
                editable={editable}
                admin={admin}
                selected={step}
                onSelect={setStep}
                problems={problems}
                starter={
                  starterAgent && !selected ? `agent:${starterAgent}` : ""
                }
                onChange={(value, group) => {
                  editor.change(value, group);
                  setStarterAgent("");
                }}
              />
            </Suspense>
          )}
          {workflow &&
            workflow.name === selected &&
            version &&
            source === version.yaml_source && (
              <section className="subsection" ref={runPanel}>
                <h3>Run {workflow.name}</h3>
                <ActionForm
                  label="Start run"
                  disabled={!editable || !!workflow.archived_at}
                  submit={async (data) => {
                    const r = await api("/runs", "POST", {
                      workflow: workflow.name,
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
