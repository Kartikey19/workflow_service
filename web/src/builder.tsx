import { useEffect, useMemo, useState, useRef } from "react";
import {
  ReactFlow,
  Background,
  BackgroundVariant,
  Controls,
  Handle,
  MarkerType,
  Position,
  type ReactFlowInstance,
  applyNodeChanges,
  type Node,
  type NodeProps,
  type Connection,
  type NodeChange,
} from "@xyflow/react";
import dagre from "@dagrejs/dagre";
import {
  Bot,
  CircleAlert,
  Clock,
  Eye,
  EyeOff,
  GitBranch,
  Globe,
  Hourglass,
  KeyRound,
  Mail,
  Minus,
  Plug,
  Play,
  Plus,
  Server,
  Shuffle,
  UserCheck,
  Workflow as WorkflowIcon,
} from "lucide-react";
import { parse, stringify } from "yaml";
import { secretParts, type RecordData } from "./api";
import {
  useResource,
  Field,
  InputFields,
  readInput,
  ResourceState,
} from "./components";
import { connectorActions, kindIcon } from "./connectors";
import { NodeCreator, type CreatorEntry } from "./creator";
import { ConnectorForm, ModelFields, ProviderForm, SetupModal } from "./setup";
import { StepPicker, type PickerItem } from "./picker";
import { isPlaceholder, type Problem } from "./problems";
import { WorkflowInputs } from "./workflow-inputs";
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
type Icon = typeof Plug;
const typeIcons: Record<string, Icon> = {
  http: Globe,
  transform: Shuffle,
  sleep: Clock,
  branch: GitBranch,
  human_task: UserCheck,
  wait_event: Hourglass,
  workflow: WorkflowIcon,
  noop: Minus,
  agent: Bot,
  "email.send": Mail,
  "mcp.call": Server,
};
const icon = (I: Icon, size = 16) => <I size={size} aria-hidden="true" />;
// The one setting worth showing under a step, so typing details is visible as
// it happens.
const cardKeys = ["url", "duration", "queue", "key", "name", "subject", "to"];
function cardDetail(config: RecordData = {}) {
  const shown = Object.entries(config).filter(
    ([, v]) => ["string", "number"].includes(typeof v) && String(v),
  );
  const hit =
    cardKeys.map((k) => shown.find(([name]) => name === k)).find(Boolean) ||
    shown[0];
  return hit ? String(hit[1]).slice(0, 48) : "";
}
// "decision: ${ok ? 'yes' : 'no'}" is not valid YAML. Quote every value that
// holds an expression, so nobody needs YAML's quoting rules.
export function quoteExpressions(text: string) {
  return text.replace(
    /^([ \t]*(?:-[ \t]+)?[\w.-]+:[ \t]+|[ \t]*-[ \t]+)(?![ \t"'[{|>])(.*\$\{.*)$/gm,
    (_, head: string, value: string) => `${head}'${value.replace(/'/g, "''")}'`,
  );
}
// Steps in the order they can run: a step comes after everything it needs.
function runOrder(rows: RecordData[]) {
  const done = new Set<string>(),
    out: RecordData[] = [];
  let left = [...rows];
  while (left.length) {
    const ready = left.filter((n) =>
      (n.needs || []).every(
        (d: string) => done.has(d) || !rows.some((r) => r.id === d),
      ),
    );
    const next = ready.length ? ready : [left[0]]; // a cycle: keep going
    next.forEach((n) => {
      done.add(n.id);
      out.push(n);
    });
    left = left.filter((n) => !next.includes(n));
  }
  return out;
}

type StepData = {
  title: string;
  subtitle: string;
  Icon: Icon;
  problem: boolean;
  onAdd?: () => void;
};
// A step as n8n draws one: a square tile with its icon, the name underneath,
// and a + after the last step to continue the flow.
function StepNode({ data, selected }: NodeProps<Node<StepData>>) {
  return (
    <div className={`step-node ${selected ? "selected" : ""}`}>
      <Handle type="target" position={Position.Left} />
      <div className={`step-tile ${data.problem ? "has-problem" : ""}`}>
        {icon(data.Icon, 26)}
        {data.problem && (
          <CircleAlert
            size={15}
            className="tile-alert"
            aria-label="This step has problems"
          />
        )}
      </div>
      <Handle type="source" position={Position.Right} />
      {data.onAdd && (
        <button
          className="tile-add nodrag"
          aria-label={`Add a step after ${data.title}`}
          onClick={(e) => {
            e.stopPropagation();
            data.onAdd?.();
          }}
        >
          <Plus size={14} aria-hidden="true" />
        </button>
      )}
      <div className="step-caption">
        <strong>{data.title}</strong>
        <small>{data.subtitle}</small>
      </div>
    </div>
  );
}
const nodeTypes = { step: StepNode };

export function Builder({
  source,
  onChange,
  editable,
  organizationID,
  selected,
  onSelect,
  problems,
  starter,
  admin,
}: {
  source: string;
  onChange: (source: string, group?: string) => void;
  editable: boolean;
  admin: boolean;
  organizationID: string;
  selected: string;
  onSelect: (id: string) => void;
  problems: Problem[];
  // A step to add as soon as the catalog loads, e.g. the agent a person chose
  // on the Agents page.
  starter: string;
}) {
  const doc = useMemo(() => parsed(source), [source]);
  const connections = useResource(editable ? "/connections" : null),
    kinds = useResource("/integrations"),
    agents = useResource("/agents"),
    providers = useResource(editable ? "/providers" : null),
    executors = useResource("/executors"),
    secrets = useResource(editable ? "/secrets" : null);
  const storageKey = `workflow-layout:${organizationID}:${doc?.name || "draft"}`;
  const lastLayout = useRef("");
  const [flow, setFlow] = useState<ReactFlowInstance<Node<StepData>> | null>(
    null,
  );
  const placed = useRef<Record<string, { x: number; y: number }>>({}),
    pending = useRef<ReturnType<typeof setTimeout>>(undefined),
    pendingForm = useRef<HTMLFormElement | null>(null),
    pendingGroup = useRef(""),
    panel = useRef<HTMLElement>(null),
    focusSettings = useRef(false),
    lastField = useRef<HTMLInputElement | HTMLTextAreaElement | null>(null),
    started = useRef(false);
  const [settingsError, setSettingsError] = useState("");
  const [nodes, setNodes] = useState<Node<StepData>[]>([]),
    [error, setError] = useState("");
  // Where a new step joins: null keeps the creator closed.
  const [adding, setAdding] = useState<{ after: string } | null>(null),
    [diagram, setDiagram] = useState(false);
  const startOpen = selected === "__start__";
  const [setup, setSetup] = useState<{
    kind: "provider" | "connector";
    nodeID: string;
    descriptor?: RecordData;
    providerKind?: string;
  } | null>(null);
  const rows: RecordData[] = doc?.nodes || [];
  const empty = isPlaceholder(rows);
  const ordered = empty ? [] : runOrder(rows);
  const edges = rows.flatMap((n) =>
    (n.needs || []).map((dep: string) => ({
      id: `${dep}:${n.id}`,
      source: dep,
      target: n.id,
      markerEnd: {
        type: MarkerType.ArrowClosed,
        color: "#c98a12",
        width: 16,
        height: 16,
      },
    })),
  );
  const stepProblems = (id: string) =>
    problems.filter((p) => p.step === id && !p.warning);
  const describe = (n: RecordData) => {
    const info = executors.data?.items.find(
      (e: RecordData) => e.type === n.type,
    );
    const conn = connections.data?.items.find(
      (c: RecordData) => c.name === n.config?.connection,
    );
    const action = (kinds.data?.items || [])
      .flatMap((k: RecordData) => k.actions || [])
      .find((a: RecordData) => a.name === (n.config?.action || n.type));
    const Icon: Icon =
      n.type === "agent"
        ? Bot
        : n.config?.connection
          ? kindIcon(conn?.kind)
          : typeIcons[n.type] || Shuffle;
    const subtitle =
      n.type === "agent"
        ? `${n.config?.agent || n.config?.model || "Choose a model"}${n.config?.connection ? ` · ${n.config.connection}` : ""}`
        : n.config?.connection
          ? `${action?.title || n.config.action || n.type} · ${n.config.connection}`
          : cardDetail(n.config) || info?.title || n.type;
    return { Icon, subtitle, title: info?.title || n.type, action };
  };
  const sinks = rows.filter((n) => !rows.some((m) => m.needs?.includes(n.id)));
  useEffect(() => {
    const graph = new dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}));
    graph.setGraph({ rankdir: "LR", nodesep: 48, ranksep: 96 });
    const shown = empty ? [] : rows;
    shown.forEach((n) => graph.setNode(n.id, { width: 150, height: 120 }));
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
      shown.map((n) => {
        const d = describe(n);
        return {
          id: n.id,
          type: "step",
          position: (sameWorkflow
            ? old.find((v) => v.id === n.id)?.position
            : undefined) ||
            (Number.isFinite(saved[n.id]?.x) && Number.isFinite(saved[n.id]?.y)
              ? saved[n.id]
              : undefined) ||
            placed.current[n.id] || {
              x: graph.node(n.id).x - 75,
              y: graph.node(n.id).y - 60,
            },
          data: {
            title: n.id,
            subtitle: d.subtitle,
            Icon: d.Icon,
            problem: stepProblems(n.id).length > 0,
            onAdd:
              editable && sinks.some((s) => s.id === n.id)
                ? () => setAdding({ after: n.id })
                : undefined,
          },
          selected: n.id === selected,
        };
      }),
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    source,
    selected,
    storageKey,
    problems,
    executors.data,
    connections.data,
    kinds.data,
  ]);
  const graphIdentity = `${storageKey}:${rows.map((n) => n.id).join(",")}:${edges.map((e) => e.id).join(",")}:${diagram}`;
  useEffect(() => {
    const timer = setTimeout(() => {
      flow?.fitView({ padding: 0.25, duration: 0, maxZoom: 1 });
    }, 220); // after a just-shown diagram has been measured
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
  const selectedAction = node ? describe(node).action : undefined;
  const info = executors.data?.items.find(
    (e: RecordData) => e.type === node?.type,
  );
  const selectedAgent = agents.data?.items.find(
    (a: RecordData) => a.name === node?.config?.agent,
  );
  function write(next: RecordData, group?: string) {
    onChange(stringify(next), group);
    setError("");
  }
  function updateNode(next: RecordData, group?: string) {
    if (doc)
      write(
        { ...doc, nodes: rows.map((n) => (n.id === next.id ? next : n)) },
        group,
      );
  }
  // Renaming keeps the flow intact: steps that run after it, and expressions
  // that read nodes.<old>, follow the new name.
  function renameStep(from: string, to: string) {
    if (!doc || to === from) return;
    if (!/^[a-z][a-z0-9_]{0,62}$/.test(to)) {
      setError(
        "A step name starts with a lowercase letter and uses a–z, 0–9 and _.",
      );
      return;
    }
    if (rows.some((n) => n.id === to)) {
      setError(`Another step is already called ${to}.`);
      return;
    }
    const reference = new RegExp(`\\bnodes\\.${from}\\b`, "g");
    const retarget = (value: unknown) =>
      JSON.parse(JSON.stringify(value ?? {}).replace(reference, `nodes.${to}`));
    write({
      ...doc,
      nodes: rows.map((n) => ({
        ...n,
        id: n.id === from ? to : n.id,
        ...(n.needs
          ? { needs: n.needs.map((d: string) => (d === from ? to : d)) }
          : {}),
        config: retarget(n.config),
      })),
      ...(doc.outputs ? { outputs: retarget(doc.outputs) } : {}),
    });
    onSelect(to);
  }
  function removeSteps(ids: string[]) {
    if (!doc) return;
    write({
      ...doc,
      nodes: rows
        .filter((n) => !ids.includes(n.id))
        .map((n) => ({
          ...n,
          needs: (n.needs || []).filter((dep: string) => !ids.includes(dep)),
        })),
    });
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
  // Everything a person can add, the way n8n's node creator groups it: apps
  // (every connector action), AI, flow control, data, and core steps.
  const catalog = useMemo(() => {
    const build = new Map<string, () => { type: string; config: RecordData }>();
    const saved: RecordData[] = connections.data?.items || [];
    const apps: CreatorEntry[] = (kinds.data?.items || [])
      .filter(
        (k: RecordData) => connectorActions(kinds.data?.items, k.kind).length,
      )
      .map((k: RecordData) => {
        const mine = saved.filter((c) => c.kind === k.kind);
        return {
          id: `app:${k.kind}`,
          title: k.title || k.kind,
          hint: mine.length
            ? mine.map((c) => c.name).join(", ")
            : "Not connected yet",
          icon: icon(kindIcon(k.kind)),
          connected: mine.length > 0,
          children: connectorActions(kinds.data?.items, k.kind).map((a) => {
            const id = `action:${k.kind}:${a.name}`;
            build.set(id, () => ({
              type: "connector.call",
              config: {
                connection: mine[0]?.name || "",
                action: a.name,
                arguments: newArguments(a.input_schema),
              },
            }));
            return {
              id,
              title: a.title,
              section: a.read_only ? "Read" : "Write",
              icon: icon(kindIcon(k.kind)),
              blocked: mine.length
                ? undefined
                : {
                    reason: `Connect ${k.title || k.kind} first.`,
                    action: admin ? "Connect account here" : "Configure step",
                  },
            };
          }),
        };
      })
      .sort(
        (a: RecordData, b: RecordData) =>
          Number(b.connected) - Number(a.connected),
      );
    const ai: CreatorEntry[] = (agents.data?.items || []).map(
      (a: RecordData) => {
        const id = `agent:${a.name}`;
        build.set(id, () => ({
          type: "agent",
          config: {
            agent: a.name,
            ...(a.input_schema
              ? { input: newArguments(a.input_schema) }
              : { prompt: "" }),
          },
        }));
        return {
          id,
          title: a.name,
          hint: a.description || a.model,
          icon: icon(Bot),
        };
      },
    );
    build.set("agent:inline", () => ({
      type: "agent",
      config: { model: "", prompt: "", max_iterations: 8 },
    }));
    ai.unshift({
      id: "agent:inline",
      title: "AI step",
      hint: "Choose a model and account here",
      icon: icon(Bot),
    });
    const viaConnector = new Set(
      (kinds.data?.items || []).flatMap((k: RecordData) =>
        connectorActions(kinds.data?.items, k.kind).map((a) => a.name),
      ),
    );
    const byCategory = (category: string) =>
      (executors.data?.items || [])
        .filter(
          (e: RecordData) =>
            e.category === category &&
            !["agent", "connector.call"].includes(e.type) &&
            !viaConnector.has(e.type),
        )
        .map((e: RecordData) => {
          const id = `type:${e.type}`;
          build.set(id, () => ({ type: e.type, config: {} }));
          return {
            id,
            title: e.title || e.type,
            hint: e.description?.split(". ")[0],
            icon: icon(typeIcons[e.type] || Shuffle),
          };
        });
    const entries: CreatorEntry[] = [
      {
        id: "apps",
        title: "Action in an app",
        hint: "Send email, run a query, post to Slack…",
        icon: icon(Plug),
        children: apps,
      },
      {
        id: "ai",
        title: "AI",
        hint: "Choose a model or use a saved agent",
        icon: icon(Bot),
        children: ai,
      },
      {
        id: "flow",
        title: "Flow",
        hint: "Branch, wait, ask a person, run another workflow",
        icon: icon(GitBranch),
        children: byCategory("control"),
      },
      {
        id: "data",
        title: "Data",
        hint: "Reshape or compute values",
        icon: icon(Shuffle),
        children: byCategory("data"),
      },
      {
        id: "core",
        title: "Core",
        hint: "HTTP request, email without a connector, MCP",
        icon: icon(Globe),
        children: byCategory("integration"),
      },
    ];
    return { entries, build };
  }, [connections.data, kinds.data, agents.data, executors.data, admin]);
  function add(id: string, after = adding?.after) {
    const entry = catalog.build.get(id);
    if (!doc || !entry) return;
    const { type, config } = entry();
    const replacing = isPlaceholder(rows),
      base = replacing ? [] : rows;
    // New steps continue the flow: after the chosen step, else after the
    // selected one, else after the only step nothing depends on yet.
    const ends = base.filter((n) => !base.some((m) => m.needs?.includes(n.id)));
    const parent = replacing
      ? ""
      : after && base.some((n) => n.id === after)
        ? after
        : base.some((n) => n.id === selected)
          ? selected
          : ends.length === 1
            ? ends[0].id
            : "";
    // Name the step after what it does: db_query_2, run_analyst_3, sleep_1.
    const stem =
      String(config.action || config.agent || type)
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "_")
        .replace(/^[^a-z]+|_+$/g, "") || "step";
    let index = base.length + 1,
      nodeID = `${stem}_${index}`;
    while (base.some((n) => n.id === nodeID)) nodeID = `${stem}_${++index}`;
    const from = nodes.find((n) => n.id === (parent || rows[0]?.id));
    if (from) {
      const siblings = base.filter((n) => n.needs?.includes(parent)).length;
      placed.current[nodeID] = replacing
        ? from.position
        : { x: from.position.x + 246, y: from.position.y + siblings * 150 };
    }
    write({
      ...doc,
      nodes: [
        ...base,
        { id: nodeID, type, ...(parent ? { needs: [parent] } : {}), config },
      ],
    });
    setAdding(null);
    focusSettings.current = true;
    onSelect(nodeID);
    return nodeID;
  }
  useEffect(() => {
    if (starter && editable && !started.current && catalog.build.has(starter)) {
      started.current = true;
      add(starter, "");
    }
  });
  // After adding a step, put the cursor in its first field.
  useEffect(() => {
    if (!focusSettings.current || !node) return;
    focusSettings.current = false;
    panel.current
      ?.querySelector<HTMLElement>(
        ".step-detail form input, .step-detail form textarea, .step-detail form select",
      )
      ?.focus();
  }, [node?.id]);
  // Settings apply as you type; the canvas and checks follow. Leaving the form
  // applies anything still waiting.
  function applySettings(form: HTMLFormElement, group: string) {
    // A form left over from another step must not write into this one.
    if (!node || form.dataset.node !== node.id) return;
    const data = new FormData(form);
    try {
      let config = node.config || {};
      if (selectedAction) {
        const args = readInput(data, selectedAction.input_schema, true);
        config =
          node.type === "connector.call"
            ? { ...config, arguments: args }
            : { ...args, connection: config.connection };
      } else if (selectedAgent) {
        config = selectedAgent.input_schema
          ? {
              ...config,
              input: readInput(data, selectedAgent.input_schema, true),
            }
          : { ...config, prompt: data.get("prompt") };
      } else if (node.type === "agent" && !node.config?.agent) {
        config = {
          ...config,
          prompt:
            typeof config.prompt !== "string" &&
            data.get("prompt") === JSON.stringify(config.prompt || "")
              ? config.prompt
              : data.get("prompt"),
          system: data.get("system") || "",
          max_iterations: Number(data.get("max_iterations")),
        };
      } else {
        const next =
          parse(quoteExpressions(String(data.get("config") || ""))) ?? {};
        if (typeof next !== "object" || Array.isArray(next))
          throw new Error("Write one setting per line, as name: value.");
        config = next;
      }
      setSettingsError("");
      updateNode({ ...node, config }, group);
    } catch (e) {
      setSettingsError((e as Error).message);
    }
  }
  // Timers fire later: call the newest applySettings, never the copy from the
  // render that queued them, which may predate a rename or another edit.
  const latestApply = useRef(applySettings);
  latestApply.current = applySettings;
  function flushSettings(form: HTMLFormElement) {
    if (!pending.current) return;
    clearTimeout(pending.current);
    pending.current = undefined;
    latestApply.current(form, pendingGroup.current);
  }
  useEffect(() => {
    const flush = () => {
      if (pendingForm.current) flushSettings(pendingForm.current);
    };
    const hidden = () => {
      if (document.visibilityState === "hidden") flush();
    };
    window.addEventListener("beforeunload", flush, true);
    window.addEventListener("pagehide", flush);
    document.addEventListener("visibilitychange", hidden);
    return () => {
      clearTimeout(pending.current);
      window.removeEventListener("beforeunload", flush, true);
      window.removeEventListener("pagehide", flush);
      document.removeEventListener("visibilitychange", hidden);
    };
  }, []);
  // Vault secrets, a group's keys together, ready to drop into a setting.
  const secretItems: PickerItem[] = (secrets.data?.items || []).map(
    (s: RecordData) => {
      const { group = "Secrets", key } = secretParts(s.name);
      return { id: s.reference, group, title: key, hint: s.reference };
    },
  );
  // Insert at the caret of the setting being edited, or the first text field.
  function insertSecret(reference: string) {
    const field =
      lastField.current?.isConnected && lastField.current.form
        ? lastField.current
        : panel.current?.querySelector<HTMLInputElement | HTMLTextAreaElement>(
            ".step-detail form textarea, .step-detail form input:not([type])",
          );
    if (!field) return;
    field.focus();
    const at = field.selectionEnd ?? field.value.length;
    field.setRangeText(reference, field.selectionStart ?? at, at, "end");
    field.dispatchEvent(new Event("input", { bubbles: true }));
  }
  if (!doc)
    return (
      <p className="error">
        This workflow definition has no readable list of steps. Start a new
        workflow or ask the assistant again.
      </p>
    );
  // Which form the step's details use. Agent and connector steps wait for
  // their schemas, so a half-loaded form never shows the raw settings.
  const mode = !node
    ? ""
    : selectedAction
      ? "action"
      : selectedAgent
        ? "agent"
        : node.type === "agent" && !node.config?.agent
          ? "inline-agent"
          : (node.type === "agent" && !agents.data) ||
              (node.type === "connector.call" && !kinds.data)
            ? "loading"
            : "settings";
  // Saved connectors that can run the selected connector step's action.
  const accounts = (connections.data?.items || []).filter((c: RecordData) =>
    selectedAction?.kinds?.includes(c.kind),
  );
  const detail = node && (
    <div className="step-detail">
      {editable && (
        <Field
          label="Step name"
          hint="Other steps read its output as nodes.<name>."
        >
          <input
            key={node.id}
            defaultValue={node.id}
            spellCheck={false}
            onBlur={(e) => renameStep(node.id, e.target.value.trim())}
            onKeyDown={(e) => {
              if (e.key === "Enter") e.currentTarget.blur();
            }}
          />
        </Field>
      )}
      {info?.description && !node.config?.connection && !selectedAgent && (
        <p className="muted step-doc">{info.description}</p>
      )}
      {stepProblems(node.id).length > 0 && (
        <ul className="problems compact" aria-label="Problems">
          {stepProblems(node.id).map((p, i) => (
            <li key={i} className="error">
              <span>{p.message}</span>
            </li>
          ))}
        </ul>
      )}
      {node.type === "connector.call" && (
        <div className="account-controls">
          <Field
            label="Connector account"
            hint="Choose the saved account this step should use."
          >
            <select
              value={node.config?.connection || ""}
              disabled={!editable}
              onChange={(e) =>
                updateNode({
                  ...node,
                  config: { ...node.config, connection: e.target.value },
                })
              }
            >
              <option value="">Choose an account</option>
              {node.config?.connection &&
                !accounts.some(
                  (c: RecordData) => c.name === node.config.connection,
                ) && (
                  <option value={node.config.connection}>
                    {node.config.connection} (unavailable)
                  </option>
                )}
              {accounts.map((c: RecordData) => (
                <option key={c.name} value={c.name}>
                  {c.name}
                </option>
              ))}
            </select>
          </Field>
          {editable && admin && (
            <button
              type="button"
              onClick={() =>
                setSetup({
                  kind: "connector",
                  nodeID: node.id,
                  descriptor: kinds.data?.items.find((k: RecordData) =>
                    selectedAction?.kinds?.includes(k.kind),
                  ),
                })
              }
            >
              Add connector account
            </button>
          )}
          {!accounts.length && !admin && (
            <p className="notice">
              An administrator needs to connect an account. You can keep
              configuring this step here.
            </p>
          )}
        </div>
      )}
      {node.type === "agent" && (
        <div className="agent-model-controls">
          {selectedAgent && (
            <>
              <p className="notice">
                Agent defaults:{" "}
                {selectedAgent.connection || "Platform provider"} ·{" "}
                {selectedAgent.model}. Overrides apply only to this step.
              </p>
              <div className="row wrap">
                <label>
                  <input
                    type="checkbox"
                    checked={Object.hasOwn(node.config || {}, "connection")}
                    disabled={!editable}
                    onChange={(e) => {
                      const config = { ...node.config };
                      if (e.target.checked)
                        config.connection = selectedAgent.connection || "";
                      else delete config.connection;
                      updateNode({ ...node, config });
                    }}
                  />
                  Override provider account
                </label>
                <label>
                  <input
                    type="checkbox"
                    checked={Object.hasOwn(node.config || {}, "model")}
                    disabled={!editable}
                    onChange={(e) => {
                      const config = { ...node.config };
                      if (e.target.checked) config.model = selectedAgent.model;
                      else delete config.model;
                      updateNode({ ...node, config });
                    }}
                  />
                  Override model
                </label>
              </div>
            </>
          )}
          <ModelFields
            providers={providers.data?.items || []}
            connection={
              node.config?.connection ?? selectedAgent?.connection ?? ""
            }
            model={node.config?.model ?? selectedAgent?.model ?? ""}
            disabled={!editable}
            connectionDisabled={
              !!selectedAgent && !Object.hasOwn(node.config || {}, "connection")
            }
            modelDisabled={
              !!selectedAgent && !Object.hasOwn(node.config || {}, "model")
            }
            onChange={(connection, model, changed) => {
              const config = { ...node.config };
              if (changed === "connection") {
                config.connection = connection;
                if (!selectedAgent || Object.hasOwn(config, "model"))
                  config.model = model;
              } else config.model = model;
              updateNode(
                { ...node, config },
                changed === "model" ? `model:${node.id}` : undefined,
              );
            }}
            onAdd={
              admin && editable
                ? (providerKind) =>
                    setSetup({
                      kind: "provider",
                      nodeID: node.id,
                      providerKind,
                    })
                : undefined
            }
          />
        </div>
      )}
      {editable && secretItems.length > 0 && (
        <div className="insert-secret">
          <StepPicker
            plain
            icon={<KeyRound size={14} aria-hidden="true" />}
            label="Insert secret"
            placeholder="Search secrets"
            items={secretItems}
            onPick={insertSecret}
          />
        </div>
      )}
      <form
        key={`${node.id}:${mode}`}
        data-node={node.id}
        onSubmit={(e) => e.preventDefault()}
        onInput={(e) => {
          const form = e.currentTarget;
          clearTimeout(pending.current);
          pendingForm.current = form;
          pendingGroup.current = `settings:${node.id}:${(e.target as HTMLInputElement).name}`;
          pending.current = setTimeout(() => {
            pending.current = undefined;
            latestApply.current(form, pendingGroup.current);
          }, 300);
        }}
        onFocus={(e) => {
          const t = e.target;
          if (
            t instanceof HTMLTextAreaElement ||
            (t instanceof HTMLInputElement &&
              ["text", "url", "email", "search", "password"].includes(t.type))
          )
            lastField.current = t;
        }}
        onBlur={(e) => flushSettings(e.currentTarget)}
      >
        <fieldset disabled={!editable}>
          {mode === "loading" ? (
            <p className="muted" role="status">
              Loading this step’s fields…
            </p>
          ) : selectedAction ? (
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
                schema={defaults(selectedAgent.input_schema, node.config.input)}
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
          ) : mode === "inline-agent" ? (
            <>
              <Field
                label="Instructions"
                hint="Optional guidance for this AI step."
              >
                <textarea
                  name="system"
                  defaultValue={node.config?.system || ""}
                />
              </Field>
              <Field
                label="Agent prompt"
                hint="Describe the task. You can reference workflow inputs and previous steps."
              >
                <textarea
                  name="prompt"
                  defaultValue={
                    typeof node.config?.prompt === "string"
                      ? node.config.prompt
                      : JSON.stringify(node.config?.prompt || "")
                  }
                  required
                />
              </Field>
              <Field
                label="Maximum iterations"
                hint="A hard limit on model calls for this step."
              >
                <input
                  name="max_iterations"
                  type="number"
                  min={1}
                  max={50}
                  defaultValue={node.config?.max_iterations || 8}
                  required
                />
              </Field>
            </>
          ) : (
            <Field
              label="Settings"
              hint="One setting per line, as name: value."
            >
              <textarea
                name="config"
                className="code"
                rows={6}
                spellCheck={false}
                defaultValue={
                  Object.keys(node.config || {}).length
                    ? stringify(node.config).trim()
                    : ""
                }
              />
            </Field>
          )}
        </fieldset>
        {settingsError && (
          <p className="error" role="alert">
            {settingsError}
          </p>
        )}
      </form>
      {editable && (
        <Field label="Runs after">
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
            <option value="">
              {node.needs?.length ? "Add another step…" : "Start of workflow"}
            </option>
            {rows
              .filter((n) => n.id !== node.id && !node.needs?.includes(n.id))
              .map((n) => (
                <option key={n.id}>{n.id}</option>
              ))}
          </select>
        </Field>
      )}
      {!!node.needs?.length && (
        <ul className="chips" aria-label="Runs after">
          {node.needs.map((dep: string) => (
            <li key={dep}>
              <span>After {dep}</span>
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
            </li>
          ))}
        </ul>
      )}
      {info?.example && !selectedAction && !selectedAgent && (
        <details>
          <summary>Example and output</summary>
          <pre>{info.example}</pre>
          {info.output && (
            <p className="muted">
              Output: <code>{info.output}</code>
            </p>
          )}
        </details>
      )}
      <details>
        <summary>Step definition</summary>
        <pre>{stringify(node)}</pre>
      </details>
      {editable && (
        <button
          className="danger"
          onClick={() => {
            removeSteps([node.id]);
            onSelect("");
          }}
        >
          Delete step
        </button>
      )}
    </div>
  );
  return (
    <div className={`editor-split ${diagram ? "show-diagram" : ""}`}>
      <section className="steps-pane" aria-label="Steps" ref={panel}>
        <div className="steps-head">
          <h3>Steps</h3>
          <span className="muted">{ordered.length || ""}</span>
          <button
            className="diagram-toggle"
            aria-pressed={diagram}
            onClick={() => setDiagram(!diagram)}
          >
            {diagram ? icon(EyeOff, 15) : icon(Eye, 15)}
            {diagram ? "Hide diagram" : "Show diagram"}
          </button>
        </div>
        <ResourceState
          error={
            connections.error || kinds.error || agents.error || executors.error
          }
          loading={false}
        />
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <ol className="step-list">
          <li className={startOpen ? "open" : ""}>
            <button
              className="step-row"
              aria-expanded={startOpen}
              onClick={() => onSelect(startOpen ? "" : "__start__")}
            >
              <span className="step-number" />
              <span className="step-icon">{icon(Play)}</span>
              <span className="step-text">
                <strong>Start</strong>
                <small>
                  {Object.keys(doc.inputs || {}).length
                    ? `Inputs: ${Object.keys(doc.inputs).join(", ")}`
                    : "By hand, from a webhook or another workflow"}
                </small>
              </span>
            </button>
            {startOpen && (
              <div className="step-detail">
                <WorkflowInputs
                  inputs={doc.inputs}
                  editable={editable}
                  onChange={(inputs, group) => {
                    const { inputs: _old, ...rest } = doc;
                    write(
                      Object.keys(inputs).length ? { ...rest, inputs } : rest,
                      group,
                    );
                  }}
                />
              </div>
            )}
          </li>
          {ordered.map((n, i) => {
            const d = describe(n),
              open = n.id === selected;
            return (
              <li key={n.id} className={open ? "open" : ""}>
                <button
                  className="step-row"
                  aria-expanded={open}
                  onClick={() => onSelect(open ? "" : n.id)}
                >
                  <span className="step-number">{i + 1}</span>
                  <span className="step-icon">{icon(d.Icon)}</span>
                  <span className="step-text">
                    <strong>{n.id}</strong>
                    <small>{d.subtitle}</small>
                  </span>
                  {stepProblems(n.id).length > 0 && (
                    <CircleAlert
                      size={15}
                      className="step-alert"
                      aria-label="Has problems"
                    />
                  )}
                </button>
                {open && detail}
              </li>
            );
          })}
        </ol>
        {empty && (
          <p className="muted steps-empty">
            A workflow is a list of steps that run in order. Add the first one.
          </p>
        )}
        {editable && (
          <button
            className="add-step"
            aria-expanded={!!adding}
            onClick={() =>
              setAdding(
                adding
                  ? null
                  : { after: selected === "__start__" ? "" : selected },
              )
            }
          >
            {icon(Plus)}
            Add step
          </button>
        )}
      </section>
      <section className="canvas-pane" aria-label="Workflow diagram">
        {empty && editable && (
          <button
            className="canvas-first"
            onClick={() => setAdding({ after: "" })}
          >
            <span>{icon(Plus, 28)}</span>
            Add first step
          </button>
        )}
        {editable && !empty && (
          <button
            className="canvas-add icon-button"
            aria-label="Add step from the diagram"
            title="Add step"
            onClick={() =>
              setAdding({ after: selected === "__start__" ? "" : selected })
            }
          >
            {icon(Plus, 18)}
          </button>
        )}
        <ReactFlow
          onInit={setFlow}
          nodeTypes={nodeTypes}
          onNodeDragStop={(_, node) => savePositions(node)}
          nodes={nodes}
          edges={edges}
          defaultEdgeOptions={{ style: { stroke: "#c98a12", strokeWidth: 2 } }}
          onNodesChange={(changes: NodeChange<Node<StepData>>[]) =>
            setNodes((old) => applyNodeChanges(changes, old))
          }
          onNodeClick={(_, n) => onSelect(n.id)}
          onPaneClick={() => onSelect("")}
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
              ? (removed) => removeSteps(removed.map((r) => r.id))
              : undefined
          }
          deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
          fitView
          fitViewOptions={{ maxZoom: 1 }}
          proOptions={{ hideAttribution: true }}
        >
          <Background
            variant={BackgroundVariant.Dots}
            gap={20}
            size={1.4}
            color="#c9d0ce"
          />
          <Controls showInteractive={false} position="bottom-left" />
        </ReactFlow>
      </section>
      {setup && (
        <SetupModal
          title={
            setup.kind === "provider"
              ? "Add provider account"
              : `Connect ${setup.descriptor?.title || "account"}`
          }
          onClose={() => setSetup(null)}
        >
          {setup.kind === "provider" ? (
            <ProviderForm
              initialKind={setup.providerKind}
              onSaved={(provider) => {
                providers.reload();
                const target = rows.find((n) => n.id === setup.nodeID);
                if (target)
                  updateNode({
                    ...target,
                    config: { ...target.config, connection: provider.name },
                  });
                setSetup(null);
              }}
            />
          ) : setup.descriptor ? (
            <ConnectorForm
              descriptor={setup.descriptor}
              popup
              onSaved={(connection) => {
                connections.reload();
                const target = rows.find((n) => n.id === setup.nodeID);
                if (target)
                  updateNode({
                    ...target,
                    config: { ...target.config, connection: connection.name },
                  });
                setSetup(null);
              }}
            />
          ) : (
            <p role="alert">
              This connector’s setup is unavailable. Close this dialog and retry
              when the catalog has loaded.
            </p>
          )}
        </SetupModal>
      )}
      {adding && (
        <NodeCreator
          entries={catalog.entries}
          onPick={(id) => add(id)}
          onSetup={(id) => {
            const nodeID = add(id);
            if (admin && nodeID)
              setSetup({
                kind: "connector",
                nodeID,
                descriptor: kinds.data?.items.find(
                  (k: RecordData) => k.kind === id.split(":")[1],
                ),
              });
          }}
          onClose={() => setAdding(null)}
          title={
            adding.after ? `Add a step after ${adding.after}` : "Add a step"
          }
        />
      )}
    </div>
  );
}
