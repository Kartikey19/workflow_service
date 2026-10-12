import { useState } from "react";
import { Bot } from "lucide-react";
import { ModelFields, ProviderForm, SetupModal } from "./setup";
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
  admin,
  useAgent,
}: {
  editable: boolean;
  admin: boolean;
  useAgent: (name: string) => void;
}) {
  const list = useResource("/agents"),
    tools = useResource("/agent-tools"),
    providers = useResource(editable ? "/providers" : null),
    connections = useResource(editable ? "/connections" : null);
  const [edit, setEdit] = useState<RecordData | null>(null),
    [test, setTest] = useState<RecordData | null>(null),
    [result, setResult] = useState<RecordData | null>(null),
    [chosenTools, setTools] = useState<string[]>([]),
    [addingProvider, setAddingProvider] = useState(false);
  function editing(agent: RecordData) {
    setEdit(agent);
    setTools(agent.tools || []);
    setTest(null);
  }
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Reusable AI steps that search your connectors and answer.
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
            <div className="card-head">
              <span className="card-icon">
                <Bot size={18} aria-hidden="true" />
              </span>
              <div>
                <h2>{a.name}</h2>
                <small>Revision {a.revision}</small>
              </div>
            </div>
            <p>{a.description || a.system_prompt}</p>
            <small>
              {a.model} on {a.connection || "the platform provider"}
              {a.tools?.length ? `, searches ${a.tools.join(", ")}` : ""}
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
              <ModelFields
                providers={providers.data?.items || []}
                connection={edit.connection || ""}
                model={edit.model || ""}
                onChange={(connection, model) =>
                  setEdit({ ...edit, connection, model })
                }
                onAdd={admin ? () => setAddingProvider(true) : undefined}
              />
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
              </div>
            </details>
            <fieldset>
              <legend>Connectors it can search</legend>
              <p className="muted">
                The agent searches what you attach with three read-only tools:{" "}
                {tools.data?.items?.map((t: RecordData) => t.name).join(", ") ||
                  "glob, grep, read"}
                . To send or save something, add a workflow step after it.
              </p>
              <ResourceState
                error={connections.error || tools.error}
                loading={!connections.data || !tools.data}
              />
              {connections.data?.items
                .filter(
                  (c: RecordData) =>
                    c.kind === "mcp" ||
                    tools.data?.searchable_kinds?.includes(c.kind),
                )
                .map((c: RecordData) => {
                  const ref = c.kind === "mcp" ? `mcp:${c.name}` : c.name;
                  return (
                    <label className="check" key={ref}>
                      <input
                        type="checkbox"
                        checked={chosenTools.includes(ref)}
                        onChange={(e) =>
                          setTools((old) =>
                            e.target.checked
                              ? [...old, ref]
                              : old.filter((t) => t !== ref),
                          )
                        }
                      />
                      <span>
                        <strong>{c.name}</strong>
                        <small>
                          {c.kind === "mcp" ? "MCP server tools" : c.kind}
                        </small>
                      </span>
                    </label>
                  );
                })}
              {connections.data &&
                tools.data &&
                !connections.data.items.some(
                  (c: RecordData) =>
                    c.kind === "mcp" ||
                    tools.data?.searchable_kinds?.includes(c.kind),
                ) && (
                  <p className="muted">
                    No searchable connectors yet. Add a database, Gmail, Notion
                    or Slack connector under Settings.
                  </p>
                )}
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
            done=""
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
            <div className="test-result">
              <h3>{result.ok ? "Agent completed" : "Agent failed"}</h3>
              {result.ok && typeof result.output?.text === "string" ? (
                <p className="answer">{result.output.text}</p>
              ) : result.ok ? (
                <JSONView value={result.output} />
              ) : (
                <p className="error">{result.error?.message}</p>
              )}
              {result.metrics && (
                <small>
                  {result.metrics.iterations} model{" "}
                  {result.metrics.iterations === 1 ? "call" : "calls"},{" "}
                  {result.metrics.tool_calls} tool{" "}
                  {result.metrics.tool_calls === 1 ? "call" : "calls"},{" "}
                  {result.metrics.tokens_in + result.metrics.tokens_out} tokens
                  {result.metrics.cost_known
                    ? `, $${result.metrics.cost_usd.toFixed(4)}`
                    : ""}
                </small>
              )}
            </div>
          )}
        </section>
      )}
      {addingProvider && (
        <SetupModal
          title="Add provider account"
          onClose={() => setAddingProvider(false)}
        >
          <ProviderForm
            onSaved={(provider) => {
              providers.reload();
              setEdit(
                (current) =>
                  current && { ...current, connection: provider.name },
              );
              setAddingProvider(false);
            }}
          />
        </SetupModal>
      )}
    </>
  );
}
