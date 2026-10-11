import { useEffect, useState, lazy, Suspense } from "react";
const Builder = lazy(() =>
  import("./builder").then((m) => ({ default: m.Builder })),
);
const CodeMirror = lazy(() => import("@uiw/react-codemirror"));
const Graph = lazy(() => import("./graph").then((m) => ({ default: m.Graph })));
import { yaml as yamlLanguage } from "@codemirror/lang-yaml";
import { stringify } from "yaml";
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
  JSONView,
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
    agents = useResource("/agents"),
    providers = useResource(editable ? "/providers" : null);
  const [editorTab, setEditorTab] = useState("visual");
  const [selected, select] = useState(""),
    [source, setSource] = useState(initial),
    [mode, setMode] = useState(agentName ? "agent" : "list"),
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
  const [chosenAgent, chooseAgent] = useState(agentName);
  const agent = agents.data?.items?.find(
    (a: RecordData) => a.name === chosenAgent,
  );
  function view(name: string) {
    select(name);
    setWorkflow(null);
    setMode("editor");
    setPublished("");
  }
  async function publish(yaml: string) {
    const r = await api("/workflows", "POST", { yaml });
    select(r.workflow.name);
    setPublished(r.workflow.name);
    list.reload();
    detail.reload();
    return r;
  }
  return (
    <>
      <div className="toolbar">
        <p className="muted">Versioned workflows, ready to run.</p>
        {editable && (
          <div className="row">
            <button
              onClick={() => {
                setMode("agent");
                setPublished("");
              }}
            >
              Create from agent
            </button>
            <button
              className="primary"
              onClick={() => {
                select("");
                setPublished("");
                setWorkflow(null);
                setSource(initial);
                setMode("editor");
                setEditorTab("visual");
              }}
            >
              New visual workflow
            </button>
            <button
              onClick={() => {
                select("");
                setPublished("");
                setWorkflow(null);
                setSource(initial);
                setMode("editor");
                setEditorTab("yaml");
              }}
            >
              New YAML workflow
            </button>
            <button onClick={() => setMode("assistant")}>Ask assistant</button>
          </div>
        )}
      </div>
      <div className="workflow-layout">
        <aside className="panel workflow-list">
          <h3>Your workflows</h3>
          <ResourceState error={list.error} loading={!list.data} />
          {list.data?.items?.map((w: RecordData) => (
            <button
              key={w.name}
              onClick={() => view(w.name)}
              className={`list-item ${selected === w.name ? "active" : ""}`}
            >
              <strong>{w.name}</strong>
              <small>
                {w.archived_at ? "Archived" : `Version ${w.latest_version}`} ·{" "}
                {w.run_count} runs
              </small>
            </button>
          ))}
          {list.data && !list.data.items.length && (
            <Empty>
              Create your first workflow from a stored agent or a YAML
              definition.
            </Empty>
          )}
        </aside>
        <section className="panel workspace">
          {mode === "list" && (
            <div className="welcome">
              <span className="eyebrow">BUILD ONCE. RUN RELIABLY.</span>
              <h2>A home for your operations.</h2>
              <p>
                Connect your tools, give an agent a job, and follow every step
                as it runs.
              </p>
              {editable && (
                <button className="primary" onClick={() => setMode("agent")}>
                  Create from agent
                </button>
              )}
            </div>
          )}
          {mode === "agent" && (
            <>
              <h2>Create a workflow from an agent</h2>
              <p className="muted">
                Choose an agent. We’ll create the workflow and its input form
                for you.
              </p>
              <ActionForm
                label="Publish workflow"
                disabled={!agent || !editable}
                submit={async (data) => {
                  if (!agent) throw new Error("Choose an agent first.");
                  const properties = agent.input_schema?.properties || {
                    prompt: {
                      type: "string",
                      description: "What should the agent do?",
                    },
                  };
                  const required = agent.input_schema?.required || ["prompt"];
                  const inputs = Object.fromEntries(
                    Object.entries(properties).map(([name, raw]) => {
                      const s = raw as RecordData;
                      return [
                        name,
                        {
                          type: [
                            "string",
                            "number",
                            "integer",
                            "boolean",
                            "object",
                            "array",
                            "any",
                          ].includes(s.type)
                            ? s.type
                            : "any",

                          required: required.includes(name),
                          ...(s.default === undefined
                            ? {}
                            : { default: s.default }),
                        },
                      ];
                    }),
                  );
                  const input = Object.fromEntries(
                    Object.keys(properties).map((name) => [
                      name,
                      "${input[" + JSON.stringify(name) + "]}",
                    ]),
                  );
                  const config = agent.input_schema
                    ? { agent: agent.name, input }
                    : { agent: agent.name, prompt: "${input.prompt}" };
                  const yaml = stringify({
                    apiVersion: "workflow/v1",
                    name: String(data.get("workflow-name")),
                    inputs,
                    nodes: [{ id: "agent", type: "agent", config }],
                    outputs: { result: "${nodes.agent.output}" },
                  });
                  setSource(yaml);
                  await publish(yaml);
                  setMode("editor");
                }}
              >
                <Field label="Workflow name">
                  <input
                    name="workflow-name"
                    required
                    pattern="[a-zA-Z][a-zA-Z0-9_-]+"
                    placeholder="customer-triage"
                  />
                </Field>
                <Field label="Agent">
                  <select
                    required
                    value={agent?.name || chosenAgent}
                    onChange={(e) => chooseAgent(e.target.value)}
                  >
                    <option value="">Choose an agent…</option>
                    {agents.data?.items?.map((a: RecordData) => (
                      <option key={a.name}>{a.name}</option>
                    ))}
                  </select>
                </Field>
                {agent && <p>{agent.description || agent.system_prompt}</p>}
              </ActionForm>
              {agents.data && !agents.data.items.length && (
                <Empty>Create an agent in the Agents page first.</Empty>
              )}
            </>
          )}
          {mode === "assistant" && (
            <>
              <h2>Describe what you want to automate</h2>
              <p className="muted">
                The assistant drafts and validates a workflow using resources in
                this organization.
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
                    if (
                      e instanceof APIError &&
                      e.status === 422 &&
                      e.body.yaml
                    )
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
            </>
          )}
          {mode === "editor" && (
            <>
              <div className="row spread">
                <h2>{published || "Workflow editor"}</h2>
                {workflow?.versions && (
                  <Field label="Version">
                    <select
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
                          v{v.version}
                        </option>
                      ))}
                    </select>
                  </Field>
                )}
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
              <div
                className="row"
                role="tablist"
                aria-label="Workflow editor mode"
              >
                <button
                  role="tab"
                  aria-selected={editorTab === "visual"}
                  onClick={() => setEditorTab("visual")}
                >
                  Visual builder
                </button>
                <button
                  role="tab"
                  aria-selected={editorTab === "yaml"}
                  onClick={() => setEditorTab("yaml")}
                >
                  YAML
                </button>
              </div>
              <Suspense fallback={<p>Opening editor…</p>}>
                {editorTab === "visual" ? (
                  <Builder
                    organizationID={organizationID}
                    source={source}
                    editable={editable}
                    onChange={(value) => {
                      setSource(value);
                      setPublished("");
                    }}
                  />
                ) : (
                  <CodeMirror
                    value={source}
                    height="330px"
                    extensions={[yamlLanguage()]}
                    editable={editable}
                    onChange={(value) => {
                      setSource(value);
                      setPublished("");
                    }}
                    basicSetup={{ foldGutter: true }}
                    aria-label="Workflow YAML"
                  />
                )}
              </Suspense>
              <div className="validation" role="status">
                {validating
                  ? "Validating…"
                  : spec
                    ? "✓ Valid workflow"
                    : "Workflow needs attention"}
              </div>
              {diagnostics.map((d, i) => (
                <p
                  key={i}
                  className={d.severity === "warning" ? "muted" : "error"}
                >
                  {d.path || d.field} {d.message}
                </p>
              ))}
              <div className="row">
                {editable && (
                  <AsyncButton
                    disabled={!spec || validating}
                    run={() => publish(source)}
                  >
                    Publish version
                  </AsyncButton>
                )}
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
                    {workflow.archived_at
                      ? "Restore workflow"
                      : "Archive workflow"}
                  </AsyncButton>
                )}
              </div>
              <Suspense fallback={<p>Opening graph…</p>}>
                {editorTab === "yaml" && <Graph spec={spec} />}
              </Suspense>
              {published &&
                workflow &&
                version &&
                source === version.yaml_source && (
                  <section className="subsection">
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
              {spec && (
                <details>
                  <summary>Compiled specification</summary>
                  <JSONView value={spec} />
                </details>
              )}
            </>
          )}
        </section>
      </div>
    </>
  );
}
