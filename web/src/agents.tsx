import { useState } from "react";
import { api, enc, pretty, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  AsyncButton,
  Empty,
  ResourceState,
  InputFields,
  readInput,
  JSONView,
} from "./components";
const fresh = {
  name: "",
  description: "",
  connection: "",
  model: "",
  system_prompt: "",
  tools: [],
  input_schema: null,
  output_schema: null,
  max_iterations: 8,
};
export function Agents({
  editable,
  useAgent,
}: {
  editable: boolean;
  useAgent: (name: string) => void;
}) {
  const list = useResource("/agents"),
    tools = useResource("/agent-tools"),
    providers = useResource(editable ? "/providers" : null),
    connections = useResource(editable ? "/connections" : null);
  const [edit, setEdit] = useState<RecordData | null>(null),
    [test, setTest] = useState<RecordData | null>(null),
    [result, setResult] = useState<RecordData | null>(null),
    [chosenTools, setTools] = useState<string[]>([]);
  function editing(agent: RecordData) {
    setEdit(agent);
    setTools(agent.tools || []);
    setTest(null);
  }
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Reusable agents with explicit tools and bounded execution.
        </p>
        {editable && (
          <button className="primary" onClick={() => editing(fresh)}>
            New agent
          </button>
        )}
      </div>
      <ResourceState error={list.error} loading={!list.data} />
      <div className="cards">
        {list.data?.items?.map((a: RecordData) => (
          <article className="panel card" key={a.name}>
            <span className="eyebrow">AGENT · REVISION {a.revision}</span>
            <h2>{a.name}</h2>
            <p>{a.description || a.system_prompt}</p>
            <small>
              {a.model} · {a.connection || "Platform provider"}
            </small>
            <div className="row">
              {editable && (
                <>
                  <button onClick={() => editing(a)}>Edit</button>
                  <button
                    onClick={() => {
                      setTest(a);
                      setResult(null);
                      setEdit(null);
                    }}
                  >
                    Test agent
                  </button>
                  <button className="primary" onClick={() => useAgent(a.name)}>
                    Use in workflow
                  </button>
                  <AsyncButton
                    danger
                    run={async () => {
                      if (
                        confirm(
                          `Delete agent ${a.name}? Workflows using it will fail.`,
                        )
                      ) {
                        await api(`/agents/${enc(a.name)}`, "DELETE");
                        list.reload();
                      }
                    }}
                  >
                    Delete
                  </AsyncButton>
                </>
              )}
            </div>
          </article>
        ))}
      </div>
      {list.data && !list.data.items.length && (
        <Empty>Create an agent to give your workflow a reusable AI step.</Empty>
      )}
      {edit && (
        <section className="panel">
          <div className="row spread">
            <h2>{edit.name ? `Edit ${edit.name}` : "Create an agent"}</h2>
            <button onClick={() => setEdit(null)}>Close</button>
          </div>
          <ActionForm
            key={edit.name}
            label="Save agent"
            submit={async (data) => {
              const input = {
                name: data.get("name"),
                description: data.get("description"),
                connection: data.get("connection"),
                model: data.get("model"),
                system_prompt: data.get("system_prompt"),
                tools: chosenTools,
                allowed_hosts: String(data.get("allowed_hosts") || "")
                  .split(",")
                  .map((s) => s.trim())
                  .filter(Boolean),
                database_connection: data.get("database_connection"),
                max_iterations: Number(data.get("max_iterations")),
                max_tokens: data.get("max_tokens")
                  ? Number(data.get("max_tokens"))
                  : undefined,
                temperature: data.get("temperature")
                  ? Number(data.get("temperature"))
                  : undefined,
                input_schema: JSON.parse(
                  String(data.get("input_schema") || "null"),
                ),
                output_schema: JSON.parse(
                  String(data.get("output_schema") || "null"),
                ),
              };
              await api(
                edit.name ? `/agents/${enc(edit.name)}` : "/agents",
                edit.name ? "PUT" : "POST",
                input,
              );
              setEdit(null);
              list.reload();
            }}
          >
            <div className="split">
              <Field label="Agent name">
                <input
                  name="name"
                  required
                  defaultValue={edit.name}
                  readOnly={!!edit.name}
                  placeholder="support-agent"
                />
              </Field>
              <Field label="Description">
                <input name="description" defaultValue={edit.description} />
              </Field>
              <Field label="Provider connection">
                <select name="connection" defaultValue={edit.connection || ""}>
                  <option value="">Platform provider</option>
                  {providers.data?.items?.map((p: RecordData) => (
                    <option key={p.id}>{p.name}</option>
                  ))}
                </select>
              </Field>
              <Field label="Model">
                <input
                  name="model"
                  required
                  defaultValue={edit.model}
                  placeholder="Provider model ID"
                />
              </Field>
            </div>
            <Field label="Instructions">
              <textarea
                name="system_prompt"
                required
                rows={5}
                defaultValue={edit.system_prompt}
                placeholder="Describe the task and how the agent should respond."
              />
            </Field>
            <details>
              <summary>Schemas and execution limits</summary>
              <div className="split">
                <Field label="Input schema (JSON)">
                  <textarea
                    name="input_schema"
                    defaultValue={pretty(edit.input_schema) || "null"}
                  />
                </Field>
                <Field label="Output schema (JSON)">
                  <textarea
                    name="output_schema"
                    defaultValue={pretty(edit.output_schema) || "null"}
                  />
                </Field>
                <Field label="Max iterations">
                  <input
                    name="max_iterations"
                    type="number"
                    min="1"
                    max="50"
                    defaultValue={edit.max_iterations}
                  />
                </Field>
                <Field label="Max tokens">
                  <input
                    name="max_tokens"
                    type="number"
                    min="1"
                    max="64000"
                    defaultValue={edit.max_tokens ?? ""}
                  />
                </Field>
                <Field label="Temperature">
                  <input
                    name="temperature"
                    type="number"
                    min="0"
                    max="1"
                    step="0.1"
                    defaultValue={edit.temperature ?? ""}
                  />
                </Field>
                <Field
                  label="Allowed HTTP hosts"
                  hint="Comma-separated host names for the HTTP tool."
                >
                  <input
                    name="allowed_hosts"
                    defaultValue={edit.allowed_hosts?.join(", ") || ""}
                  />
                </Field>
              </div>
            </details>
            <fieldset>
              <legend>Tools</legend>
              {tools.data?.items.map((tool: RecordData) => (
                <label className="check" key={tool.name}>
                  <input
                    type="checkbox"
                    checked={chosenTools.includes(tool.name)}
                    onChange={(e) =>
                      setTools((old) =>
                        e.target.checked
                          ? [...old, tool.name]
                          : old.filter((t) => t !== tool.name),
                      )
                    }
                  />
                  <span>
                    <strong>{tool.name}</strong>
                    <small>{tool.description}</small>
                  </span>
                </label>
              ))}
              <Field
                label="Database connection"
                hint="Read-only tools use this database connection."
              >
                <select
                  name="database_connection"
                  defaultValue={edit.database_connection || ""}
                >
                  <option value="">None</option>
                  {connections.data?.items
                    .filter((c: RecordData) =>
                      ["postgres", "mysql"].includes(c.kind),
                    )
                    .map((c: RecordData) => (
                      <option key={c.name}>{c.name}</option>
                    ))}
                </select>
              </Field>
              {connections.data?.items
                .filter((c: RecordData) => c.kind === "mcp")
                .map((c: RecordData) => {
                  const name = `mcp:${c.name}`;
                  return (
                    <label className="check" key={name}>
                      <input
                        type="checkbox"
                        checked={chosenTools.includes(name)}
                        onChange={(e) =>
                          setTools((old) =>
                            e.target.checked
                              ? [...old, name]
                              : old.filter((t) => t !== name),
                          )
                        }
                      />
                      <span>MCP tools from {c.name}</span>
                    </label>
                  );
                })}
            </fieldset>
          </ActionForm>
        </section>
      )}
      {test && (
        <section className="panel">
          <div className="row spread">
            <h2>Test {test.name}</h2>
            <button onClick={() => setTest(null)}>Close</button>
          </div>
          <p className="muted">This calls the provider and uses tokens.</p>
          <ActionForm
            label="Run agent test"
            submit={async (data) => {
              setResult(
                await api(
                  `/agents/${enc(test.name)}/test`,
                  "POST",
                  test.input_schema
                    ? { input: readInput(data, test.input_schema) }
                    : { prompt: data.get("prompt") },
                ),
              );
            }}
          >
            {test.input_schema ? (
              <InputFields schema={test.input_schema} />
            ) : (
              <Field label="Prompt">
                <textarea name="prompt" required />
              </Field>
            )}
          </ActionForm>
          {result && (
            <>
              <h3>{result.ok ? "Agent completed" : "Agent failed"}</h3>
              <JSONView value={result} />
            </>
          )}
        </section>
      )}
    </>
  );
}
