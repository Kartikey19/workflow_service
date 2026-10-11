import type { RecordData } from "./api";

// The backend owns action schemas and read/write metadata for every connector.
export function connectorActions(
  kinds: RecordData[] = [],
  kind: string,
): RecordData[] {
  return kinds.find((k) => k.kind === kind)?.actions || [];
}
export function connectorReference(connection: string, action: string) {
  return `connector:${connection}:${action}`;
}
