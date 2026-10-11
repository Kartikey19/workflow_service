import {
  Bot,
  CalendarDays,
  Database,
  FileSpreadsheet,
  FileText,
  HardDrive,
  Mail,
  MessageSquare,
  Plug,
  Server,
} from "lucide-react";
import type { RecordData } from "./api";

// The backend owns action schemas and read/write metadata for every connector.
export function connectorActions(
  kinds: RecordData[] = [],
  kind: string,
): RecordData[] {
  return kinds.find((k) => k.kind === kind)?.actions || [];
}

// One icon per kind of app, shared by the step catalog and the connectors page.
export const kindIcons: Record<string, typeof Plug> = {
  gmail: Mail,
  smtp: Mail,
  resend: Mail,
  postgres: Database,
  mysql: Database,
  mongo: Database,
  slack: MessageSquare,
  notion: FileText,
  gdocs: FileText,
  gsheets: FileSpreadsheet,
  gcalendar: CalendarDays,
  gdrive: HardDrive,
  dropbox: HardDrive,
  onedrive: HardDrive,
  mcp: Server,
  llm: Bot,
};
export const kindIcon = (kind: string) => kindIcons[kind] || Plug;
