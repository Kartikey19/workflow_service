import { useEffect, useMemo, useState, useRef } from "react";
import {
  ReactFlow,
  Background,
  Controls,
  MarkerType,
  Position,
  type ReactFlowInstance,
  applyNodeChanges,
  type Node,
  type Connection,
  type NodeChange,
} from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import { parse, stringify } from "yaml";
import { pretty, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  InputFields,
  readInput,
  ResourceState,
} from "./components";
import { connectorActions } from "./connectors";
import "@xyflow/react/dist/style.css";

function parsed(source: string): RecordData | null {
  try {
    const doc = parse(source);
    return doc &&
      typeof doc === "object" &&
      Array.isArray(doc.nodes) &&
      doc.nodes.every(
        (n: RecordData) =>
          n &&
          typeof n === "object" &&
          typeof n.id === "string" &&
          typeof n.type === "string" &&
          (n.needs === undefined ||
            (Array.isArray(n.needs) &&
              n.needs.every((d: unknown) => typeof d === "string"))),
      ) &&
      new Set(doc.nodes.map((n: RecordData) => n.id)).size === doc.nodes.length
      ? doc
      : null;
  } catch {
    return null;
  }
}
function defaults(schema: RecordData, values: RecordData = {}) {
  return {
    ...schema,
    properties: Object.fromEntries(
      Object.entries(schema.properties || {}).map(([key, raw]) => [
        key,
        {
          ...(raw as RecordData),
          ...(values[key] === undefined ? {} : { default: values[key] }),
        },
      ]),
    ),
  };
}
function newArguments(schema: RecordData) {
  return Object.fromEntries(
    Object.entries(schema.properties || {})
      .filter(
        ([name, raw]) =>
          schema.required?.includes(name) ||
          (raw as RecordData).default !== undefined,
      )
      .map(([name, raw]) => {
        const s = raw as RecordData;
        return [
          name,
          s.default ??
            (s.type === "array"
              ? []
              : s.type === "object"
                ? {}
                : ["number", "integer"].includes(s.type)
                  ? 1
                  : ""),
        ];
      }),
  );
}
export function Builder({
  source,
  onChange,
  editable,
  organizationID,
}: {
  source: string;
  onChange: (source: string) => void;
  editable: boolean;
  organizationID: string;
}) {
  const doc = useMemo(() => parsed(source), [source]);
  const connections = useResource(editable ? "/connections" : null),
    kinds = useResource("/integrations"),
    agents = useResource("/agents"),
    executors = useResource("/executors");
  const storageKey = `workflow-layout:${organizationID}:${doc?.name || "draft"}`;
  const lastLayout = useRef("");
  const [flow, setFlow] = useState<ReactFlowInstance | null>(null);
  const [selected, setSelected] = useState(""),
    [nodes, setNodes] = useState<Node[]>([]),
    [addType, setAddType] = useState("connector"),
    [connector, setConnector] = useState(""),
    [actionName, setActionName] = useState(""),
    [agentName, setAgentName] = useState(""),
    [error, setError] = useState("");
  const rows: RecordData[] = doc?.nodes || [];
  const edges = rows.flatMap((n) =>
    (n.needs || []).map((dep: string) => ({
      id: `${dep}:${n.id}`,
      source: dep,
      target: n.id,
      markerEnd: { type: MarkerType.ArrowClosed },
    })),
  );
  useEffect(() => {
    const graph = new dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}));
    graph.setGraph({ rankdir: "LR", nodesep: 32, ranksep: 64 });
    rows.forEach((n) => graph.setNode(n.id, { width: 200, height: 84 }));
    edges.forEach((e) => graph.setEdge(e.source, e.target));
    dagre.layout(graph);
    let saved: RecordData = {};
    try {
      const value = JSON.parse(localStorage.getItem(storageKey) || "{}");
      if (value && typeof value === "object" && !Array.isArray(value))
        saved = value;
    } catch {
      /* Unavailable or old browser layout. */
    }
    const sameWorkflow = lastLayout.current === storageKey;
    lastLayout.current = storageKey;
    setNodes((old) =>
      rows.map((n) => ({
        id: n.id,
        position: (sameWorkflow
          ? old.find((v) => v.id === n.id)?.position
          : undefined) ||
          (Number.isFinite(saved[n.id]?.x) && Number.isFinite(saved[n.id]?.y)
            ? saved[n.id]
            : undefined) || {
            x: graph.node(n.id).x - 100,
            y: graph.node(n.id).y - 42,
          },
        sourcePosition: Position.Right,
        targetPosition: Position.Left,
        data: {
          label: (
            <>
              <strong>{n.id}</strong>
              <small>
                {n.config?.agent
                  ? `Agent: ${n.config.agent}`
                  : n.config?.connection
                    ? `${n.config.connection} · ${n.config.action || n.type}`
                    : n.type}
              </small>
            </>
          ),
        },
        style: { width: 200, minHeight: 84 },
        selected: n.id === selected,
      })),
    );
  }, [source, selected, storageKey]);
  const graphIdentity = `${storageKey}:${rows.map((n) => n.id).join(",")}:${edges.map((e) => e.id).join(",")}`;
  useEffect(() => {
    const timer = setTimeout(() => {
      flow?.fitView({ padding: 0.18, duration: 0 });
    }, 100);
    return () => clearTimeout(timer);
  }, [flow, graphIdentity]);
  function savePositions(node: Node) {
    const positions = Object.fromEntries(
      nodes.map((n) => [n.id, n.id === node.id ? node.position : n.position]),
    );
    try {
      localStorage.setItem(storageKey, JSON.stringify(positions));
    } catch {
      /* Layout storage is optional. */
    }
  }
  const node = rows.find((n) => n.id === selected);
  const connection = connections.data?.items.find(
    (c: RecordData) => c.name === connector,
  );
  const available = connectorActions(kinds.data?.items, connection?.kind);
  const action = available.find((a) => a.name === actionName) || available[0];
  const selectedAction = (kinds.data?.items || [])
    .flatMap((k: RecordData) => k.actions || [])
    .find((a: RecordData) => a.name === (node?.config?.action || node?.type));
  const selectedAgent = agents.data?.items.find(
    (a: RecordData) => a.name === node?.config?.agent,
  );
  function write(next: RecordData) {
    onChange(stringify(next));
    setError("");
  }
  function updateNode(next: RecordData) {
    if (doc)
      write({ ...doc, nodes: rows.map((n) => (n.id === next.id ? next : n)) });
  }
  function connect(c: Connection) {
    if (!doc || !c.source || !c.target || c.source === c.target) return;
    // Invalid YAML may contain cycles already; use a visited set during traversal.
    const visited = new Set<string>();
    const reaches = (id: string): boolean => {
      if (id === c.source) return true;
      if (visited.has(id)) return false;
      visited.add(id);
      return rows
        .filter((n) => n.needs?.includes(id))
        .some((n) => reaches(n.id));
    };
    if (reaches(c.target)) {
      setError("This connection would create a cycle.");
      return;
    }
    write({
      ...doc,
      nodes: rows.map((n) =>
        n.id === c.target
          ? { ...n, needs: [...new Set([...(n.needs || []), c.source])] }
          : n,
      ),
    });
  }
  function add() {
    if (!doc) return;
    let type = addType,
      config: RecordData = {};
    if (addType === "connector") {
      if (!connection || !action) return;
      type = "connector.call";
      config = {
        connection: connection.name,
        action: action.name,
        arguments: newArguments(action.input_schema),
      };
    }
    if (addType === "agent") {
      if (!agentName) return;
      const a = agents.data?.items.find(
        (a: RecordData) => a.name === agentName,
      );
      config = {
        agent: agentName,
        ...(a?.input_schema
          ? { input: newArguments(a.input_schema) }
          : { prompt: "" }),
      };
    }
    let index = rows.length + 1,
      id = `${type.replaceAll(".", "_")}_${index}`;
    while (rows.some((n) => n.id === id))
      id = `${type.replaceAll(".", "_")}_${++index}`;
    write({ ...doc, nodes: [...rows, { id, type, config }] });
    setSelected(id);
  }
  if (!doc)
    return (
      <p className="error">
        The visual builder needs valid YAML with a nodes list. Open YAML to
        correct the definition.
      </p>
    );
  return (
    <>
      <p className="muted">
        Add steps, drag them into place, and connect their handles. Select a
        step to configure it. YAML supports advanced controls and expressions.
      </p>
      {editable && (
        <>
          <Field label="Workflow name">
            <input
              key={doc.name}
              defaultValue={doc.name}
              onBlur={(e) => {
                if (e.target.value !== doc.name)
                  write({ ...doc, name: e.target.value });
              }}
            />
          </Field>
          <div className="builder-add">
            <Field label="Step type">
              <select
                value={addType}
                onChange={(e) => setAddType(e.target.value)}
              >
                <option value="connector">Saved connector</option>
                <option value="agent">Saved agent</option>
                {executors.data?.items
                  .filter(
                    (e: RecordData) =>
                      !["agent", "connector.call"].includes(e.type) &&
                      !(kinds.data?.items || []).some((k: RecordData) =>
                        connectorActions(kinds.data?.items, k.kind).some(
                          (a) => a.name === e.type,
                        ),
                      ),
                  )
                  .map((e: RecordData) => (
                    <option key={e.type} value={e.type}>
                      {e.title || e.type}
                    </option>
                  ))}
              </select>
            </Field>
            {addType === "connector" && (
              <>
                <Field label="Saved connector">
                  <select
                    value={connector}
                    onChange={(e) => {
                      setConnector(e.target.value);
                      setActionName("");
                    }}
                  >
                    <option value="">Choose a connector…</option>
                    {connections.data?.items
                      .filter(
                        (c: RecordData) =>
                          connectorActions(kinds.data?.items, c.kind).length >
                          0,
                      )
                      .map((c: RecordData) => (
                        <option key={c.name} value={c.name}>
                          {c.name} · {c.kind}
                        </option>
                      ))}
                  </select>
                </Field>
                <Field label="Connector action">
                  <select
                    value={action?.name || ""}
                    onChange={(e) => setActionName(e.target.value)}
                  >
                    <option value="">Choose an action…</option>
                    {available.map((a) => (
                      <option key={a.name} value={a.name}>
                        {a.title} · {a.read_only ? "Read only" : "Writes"}
                      </option>
                    ))}
                  </select>
                </Field>
              </>
            )}
            {addType === "agent" && (
              <Field label="Saved agent">
                <select
                  value={agentName}
                  onChange={(e) => setAgentName(e.target.value)}
                >
                  <option value="">Choose an agent…</option>
                  {agents.data?.items.map((a: RecordData) => (
                    <option key={a.name}>{a.name}</option>
                  ))}
                </select>
              </Field>
            )}
            <button
              onClick={add}
              disabled={
                addType === "connector"
                  ? !connection || !action
                  : addType === "agent"
                    ? !agentName
                    : false
              }
            >
              Add step
            </button>
          </div>
          <ResourceState
            error={
              connections.error ||
              kinds.error ||
              agents.error ||
              executors.error
            }
            loading={false}
          />
        </>
      )}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <div className="builder-layout">
        <div className="builder-canvas" aria-label="Workflow canvas">
          <ReactFlow
            onInit={setFlow}
            onNodeDragStop={(_, node) => savePositions(node)}
            nodes={nodes}
            edges={edges}
            onNodesChange={(changes: NodeChange[]) =>
              setNodes((old) => applyNodeChanges(changes, old))
            }
            onNodeClick={(_, n) => setSelected(n.id)}
            onConnect={connect}
            nodesDraggable={editable}
            nodesConnectable={editable}
            onEdgesDelete={
              editable
                ? (removed) =>
                    write({
                      ...doc,
                      nodes: rows.map((n) => ({
                        ...n,
                        needs: (n.needs || []).filter(
                          (dep: string) =>
                            !removed.some(
                              (e) => e.source === dep && e.target === n.id,
                            ),
                        ),
                      })),
                    })
                : undefined
            }
            onNodesDelete={
              editable
                ? (removed) =>
                    write({
                      ...doc,
                      nodes: rows
                        .filter((n) => !removed.some((r) => r.id === n.id))
                        .map((n) => ({
                          ...n,
                          needs: (n.needs || []).filter(
                            (dep: string) => !removed.some((r) => r.id === dep),
                          ),
                        })),
                    })
                : undefined
            }
            deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
            fitView
          >
            <Background />
            <Controls />
          </ReactFlow>
        </div>
        <aside className="builder-properties">
          {!node ? (
            <p className="muted">Select a step on the canvas.</p>
          ) : (
            <>
              <h3>{node.id}</h3>
              <small>
                {node.type}{" "}
                {node.config?.connection && `· ${node.config.connection}`}
              </small>
              {editable && (
                <Field label="After step">
                  <select
                    value=""
                    onChange={(e) =>
                      connect({
                        source: e.target.value,
                        target: node.id,
                        sourceHandle: null,
                        targetHandle: null,
                      })
                    }
                  >
                    <option value="">Add dependency…</option>
                    {rows
                      .filter(
                        (n) => n.id !== node.id && !node.needs?.includes(n.id),
                      )
                      .map((n) => (
                        <option key={n.id}>{n.id}</option>
                      ))}
                  </select>
                </Field>
              )}
              {(node.needs || []).map((dep: string) => (
                <div className="row" key={dep}>
                  <small>After {dep}</small>
                  {editable && (
                    <button
                      aria-label={`Remove dependency ${dep}`}
                      onClick={() =>
                        updateNode({
                          ...node,
                          needs: node.needs.filter((d: string) => d !== dep),
                        })
                      }
                    >
                      ×
                    </button>
                  )}
                </div>
              ))}
              <ActionForm
                key={`${node.id}:${source}`}
                label="Apply step settings"
                disabled={!editable}
                submit={async (data) => {
                  let config = node.config || {};
                  if (selectedAction) {
                    const args = readInput(
                      data,
                      selectedAction.input_schema,
                      true,
                    );
                    config =
                      node.type === "connector.call"
                        ? { ...config, arguments: args }
                        : { ...args, connection: config.connection };
                  } else if (selectedAgent) {
                    config = selectedAgent.input_schema
                      ? {
                          ...config,
                          input: readInput(
                            data,
                            selectedAgent.input_schema,
                            true,
                          ),
                        }
                      : { ...config, prompt: data.get("prompt") };
                  } else {
                    config = JSON.parse(String(data.get("config") || "{}"));
                  }
                  updateNode({ ...node, config });
                }}
              >
                <fieldset disabled={!editable}>
                  {selectedAction ? (
                    <InputFields
                      expressions
                      schema={defaults(
                        selectedAction.input_schema,
                        node.type === "connector.call"
                          ? node.config.arguments
                          : node.config,
                      )}
                    />
                  ) : selectedAgent ? (
                    selectedAgent.input_schema ? (
                      <InputFields
                        expressions
                        schema={defaults(
                          selectedAgent.input_schema,
                          node.config.input,
                        )}
                      />
                    ) : (
                      <Field label="Agent prompt">
                        <textarea
                          name="prompt"
                          defaultValue={node.config.prompt}
                          required
                        />
                      </Field>
                    )
                  ) : (
                    <Field label="Step config (JSON)">
                      <textarea
                        name="config"
                        rows={8}
                        defaultValue={pretty(node.config || {})}
                      />
                    </Field>
                  )}
                </fieldset>
              </ActionForm>
              <details>
                <summary>Step definition</summary>
                <pre>{stringify(node)}</pre>
              </details>
              {editable && (
                <button
                  className="danger"
                  onClick={() => {
                    write({
                      ...doc,
                      nodes: rows
                        .filter((n) => n.id !== node.id)
                        .map((n) => ({
                          ...n,
                          needs: (n.needs || []).filter(
                            (dep: string) => dep !== node.id,
                          ),
                        })),
                    });
                    setSelected("");
                  }}
                >
                  Delete step
                </button>
              )}
            </>
          )}
        </aside>
      </div>
    </>
  );
}
