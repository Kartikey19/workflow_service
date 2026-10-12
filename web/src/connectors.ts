import { HardDrive, Mail, Plug, Server } from "lucide-react";
import type { RecordData } from "./api";
import { brandIcons } from "./brand-icons";

// The backend owns action schemas and read/write metadata for every connector.
export function connectorActions(
  kinds: RecordData[] = [],
  kind: string,
): RecordData[] {
  return kinds.find((k) => k.kind === kind)?.actions || [];
}

// One icon per kind of app, shared by the step catalog and the connectors page.
export const kindIcons: Record<string, typeof Plug> = {
  smtp: Mail,
  resend: Mail,
  onedrive: HardDrive,
  mcp: Server,
  ...brandIcons,
};
export const kindIcon = (kind: string) => kindIcons[kind] || Plug;
