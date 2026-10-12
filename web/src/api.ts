export type RecordData = Record<string, any>;
export class APIError extends Error {
  constructor(
    public status: number,
    public body: RecordData,
  ) {
    super(
      (body.detail ||
        body.message ||
        body.title ||
        `Request failed (${status})`) +
        (Array.isArray(body.errors)
          ? ": " +
            body.errors
              .map((d: RecordData) =>
                [d.path || d.field, d.message].filter(Boolean).join(" "),
              )
              .join("; ")
          : ""),
    );
  }
}
export async function api(
  path: string,
  method = "GET",
  body?: unknown,
  signal?: AbortSignal,
): Promise<RecordData> {
  const response = await fetch(`/be/v1${path}`, {
    method,
    credentials: "same-origin",
    signal,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 204) return {};
  const result = await response.json().catch((error) => {
    // Cancellation can happen after headers arrive, while JSON is reading.
    // Never turn an aborted successful response into an object with no items.
    if (signal?.aborted) throw error;
    const detail = "The server returned an unreadable response.";
    if (response.ok) throw new Error(detail);
    return { detail };
  });
  if (!response.ok) throw new APIError(response.status, result);
  return result;
}
export const enc = encodeURIComponent;
// A vault name with a dot is one key of a group: "openai.api_key".
export function secretParts(name: string): { group?: string; key: string } {
  const [group, key] = name.split(".");
  return key === undefined ? { key: group } : { group, key };
}
export const pretty = (value: unknown) => JSON.stringify(value, null, 2);
export const terminal = (status: string) =>
  ["succeeded", "failed", "cancelled", "timed_out"].includes(status);
