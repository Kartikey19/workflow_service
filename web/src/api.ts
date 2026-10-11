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
  const result = await response
    .json()
    .catch(() => ({ detail: "The server returned an unreadable response." }));
  if (!response.ok) throw new APIError(response.status, result);
  return result;
}
export const enc = encodeURIComponent;
export const pretty = (value: unknown) => JSON.stringify(value, null, 2);
export const terminal = (status: string) =>
  ["succeeded", "failed", "cancelled", "timed_out"].includes(status);
