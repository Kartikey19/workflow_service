import { parse } from "yaml";
import type { RecordData } from "./api";

// A validation message tied, when possible, to the step it is about. The API
// reports positions like `nodes[2].config`; people think in step names.
export type Problem = { step?: string; message: string; warning: boolean };

export function problemsFor(
  source: string,
  diagnostics: RecordData[],
): Problem[] {
  let ids: string[] = [];
  try {
    const doc = parse(source);
    if (Array.isArray(doc?.nodes))
      ids = doc.nodes.map((n: RecordData) => String(n?.id ?? ""));
  } catch {
    /* Invalid YAML: report the messages without step names. */
  }
  return diagnostics.map((d) => {
    const where = String(d.path || d.field || "");
    const match = /^nodes\[(\d+)\](.*)$/.exec(where);
    const step = match ? ids[Number(match[1])] || undefined : undefined;
    const rest = match ? match[2].replace(/^\./, "") : where;
    return {
      step,
      message: [rest, d.message].filter(Boolean).join(" "),
      warning: d.severity === "warning",
    };
  });
}

// A fresh workflow starts with one empty placeholder step; the first real step
// replaces it instead of sitting beside it.
export function isPlaceholder(rows: RecordData[]) {
  const n = rows[0];
  return (
    rows.length === 1 &&
    n.id === "done" &&
    n.type === "noop" &&
    !n.needs?.length &&
    !Object.keys(n.config || {}).length
  );
}
export function isBlankWorkflow(source: string) {
  try {
    const doc = parse(source);
    return Array.isArray(doc?.nodes) && isPlaceholder(doc.nodes);
  } catch {
    return false;
  }
}
