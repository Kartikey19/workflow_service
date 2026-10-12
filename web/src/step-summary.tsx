import {
  Bot,
  Clock,
  GitBranch,
  Globe,
  Hourglass,
  Mail,
  Minus,
  Server,
  Shuffle,
  UserCheck,
  Workflow,
  Plug,
} from "lucide-react";
import type { RecordData } from "./api";
import { kindIcon } from "./connectors";
import { WorkflowInputs } from "./workflow-inputs";

export const stepIcons: Record<string, typeof Plug> = {
  http: Globe,
  transform: Shuffle,
  sleep: Clock,
  branch: GitBranch,
  human_task: UserCheck,
  wait_event: Hourglass,
  workflow: Workflow,
  noop: Minus,
  agent: Bot,
  "email.send": Mail,
  "mcp.call": Server,
};
export const stepLabel = (name: string) =>
  name.replaceAll(/[_-]/g, " ").replace(/^./, (c) => c.toUpperCase());

function Value({ value }: { value: unknown }) {
  if (value === null || value === undefined)
    return <span className="muted">Not set</span>;
  if (Array.isArray(value))
    return value.length ? (
      <ol className="step-values">
        {value.map((item, i) => (
          <li key={i}>
            <Value value={item} />
          </li>
        ))}
      </ol>
    ) : (
      <span className="muted">None</span>
    );
  if (typeof value === "object")
    return (
      <dl className="step-values">
        {Object.entries(value).map(([key, child]) => (
          <div key={key}>
            <dt>{stepLabel(key)}</dt>
            <dd>
              <Value value={child} />
            </dd>
          </div>
        ))}
      </dl>
    );
  if (typeof value === "boolean") return <span>{value ? "Yes" : "No"}</span>;
  const text = String(value);
  const input = /^\$\{\s*input\.([\w]+)\s*\}$/.exec(text);
  if (input)
    return (
      <span className="value-reference">Input · {stepLabel(input[1])}</span>
    );
  const output = /^\$\{\s*nodes\.([\w]+)\.output(?:\.([\w.]+))?\s*\}$/.exec(
    text,
  );
  if (output)
    return (
      <span className="value-reference">
        {stepLabel(output[1])} ·{" "}
        {output[2] ? stepLabel(output[2].replaceAll(".", " ")) : "Output"}
      </span>
    );
  const secret = /^\$\{\s*secrets\.([\w]+)\s*\}$/.exec(text);
  if (secret)
    return <span className="value-reference">Saved secret · {secret[1]}</span>;
  return <span className="step-value-text">{text || "Not set"}</span>;
}

// Readable proposal fields. The YAML document remains an internal transport format.
export function StepSummary({
  node,
  title,
  connectionKind,
  inputs,
}: {
  node: RecordData;
  title: string;
  connectionKind?: string;
  inputs?: RecordData;
}) {
  const Icon = connectionKind
    ? kindIcon(connectionKind)
    : stepIcons[node.type] || Plug;
  return (
    <>
      <div className="step-summary-kind">
        <Icon size={18} aria-hidden="true" />
        <span>{title}</span>
      </div>
      <p className="muted">
        {node.needs?.length
          ? `Runs after ${node.needs.map(stepLabel).join(", ")}`
          : "Runs at the start"}
      </p>
      {inputs && Object.keys(inputs).length > 0 && (
        <section className="step-summary-inputs" aria-label="Run inputs">
          <h4>Run inputs</h4>
          <WorkflowInputs
            inputs={inputs}
            editable={false}
            onChange={() => {}}
          />
        </section>
      )}
      {Object.keys(node.config || {}).length > 0 ? (
        <dl className="step-values" aria-label="Step settings">
          {Object.entries(node.config).map(([key, value]) => (
            <div key={key}>
              <dt>{stepLabel(key)}</dt>
              <dd>
                <Value value={value} />
              </dd>
            </div>
          ))}
        </dl>
      ) : (
        <p className="muted">No settings needed.</p>
      )}
      {Object.entries({
        Timeout: node.timeout,
        "If time runs out": node.on_timeout,
        "If this step fails": node.on_failure,
        "Repeat for each": node.foreach,
        "Retry policy": node.retry,
        "Dependency rule": node.join,
      })
        .filter(([, value]) => value !== undefined && value !== null)
        .map(([label, value]) => (
          <div className="step-condition" key={label}>
            <strong>{label}</strong>
            <Value value={value} />
          </div>
        ))}
      {node.if && (
        <div className="step-condition">
          <strong>Run when</strong>
          <Value value={node.if} />
        </div>
      )}
    </>
  );
}
