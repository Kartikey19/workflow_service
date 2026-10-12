import {
  useEffect,
  useState,
  useId,
  cloneElement,
  isValidElement,
  type ReactNode,
  type FormEvent,
} from "react";
import { api, pretty, type RecordData } from "./api";
export const PRODUCT_NAME = "Weft";
// A "W" made of two threads: each stroke is a V, crossing over the other.
export function Logo() {
  return (
    <svg className="mark" viewBox="0 0 32 32" aria-hidden="true">
      <rect width="32" height="32" rx="9" fill="#0d6b65" />
      <g
        fill="none"
        strokeWidth="2.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <path d="M15 9l5.5 14L26 9" stroke="#e0a026" />
        <path d="M6 9l5.5 14L17 9" stroke="#eaf5f3" />
      </g>
    </svg>
  );
}
// A small workflow, drawn as the product does: steps joined by a thread.
export function StoryGraph() {
  const node = (
    x: number,
    y: number,
    title: string,
    sub: string,
    live = false,
  ) => (
    <g transform={`translate(${x} ${y})`}>
      <rect
        width="130"
        height="58"
        rx="11"
        fill="#ffffff0d"
        stroke={live ? "#e0a026" : "#ffffff2e"}
        strokeWidth={live ? 1.5 : 1}
      />
      <circle cx="16" cy="20" r="3.5" fill={live ? "#e0a026" : "#5fc1a1"} />
      <text x="28" y="24" fill="#fff" fontSize="13" fontWeight="600">
        {title}
      </text>
      <text x="16" y="43" fill="#9fb4b2" fontSize="11">
        {sub}
      </text>
    </g>
  );
  return (
    <svg
      viewBox="0 0 520 190"
      role="img"
      aria-label="A workflow: webhook, agent, then approval and email"
    >
      <g fill="none" stroke="#e0a026" strokeWidth="1.6" strokeLinecap="round">
        <path d="M130 95H175" />
        <path d="M305 95C330 95 325 43 350 43" />
        <path d="M305 95C330 95 325 147 350 147" />
      </g>
      <g fill="#e0a026">
        <circle cx="175" cy="95" r="3" />
        <circle cx="350" cy="43" r="3" />
        <circle cx="350" cy="147" r="3" />
      </g>
      {node(0, 66, "Webhook", "new support ticket")}
      {node(175, 66, "Agent", "triage and draft reply", true)}
      {node(350, 14, "Approval", "a person reviews")}
      {node(350, 118, "Send email", "customer is notified")}
    </svg>
  );
}
export function useResource(path: string | null) {
  const [data, setData] = useState<RecordData | null>(null),
    [error, setError] = useState(""),
    [revision, revise] = useState(0);
  useEffect(() => {
    setData(null);
    setError("");
    if (!path) return;
    const abort = new AbortController();
    api(path, "GET", undefined, abort.signal)
      .then((result) => {
        if (!abort.signal.aborted) setData(result);
      })
      .catch((e) => {
        if (!abort.signal.aborted) setError(e.message);
      });
    return () => abort.abort();
  }, [path, revision]);
  return { data, error, reload: () => revise((n) => n + 1) };
}
export function Field({
  label,
  children,
  hint,
}: {
  label: string;
  children: ReactNode;
  hint?: string;
}) {
  const id = useId();
  const control = isValidElement<{
    "aria-labelledby"?: string;
    "aria-describedby"?: string;
  }>(children)
    ? cloneElement(children, {
        "aria-labelledby": id,
        "aria-describedby": hint ? `${id}-hint` : undefined,
      })
    : children;
  return (
    <label className="field">
      <span id={id}>{label}</span>
      {control}
      {hint && <small id={`${id}-hint`}>{hint}</small>}
    </label>
  );
}
export function JSONView({ value }: { value: unknown }) {
  return <pre className="json">{pretty(value) ?? "—"}</pre>;
}
export function Status({ value }: { value: string }) {
  return (
    <span className={`status ${value}`}>
      {value?.replaceAll("_", " ") || "unknown"}
    </span>
  );
}
export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}
export function ResourceState({
  error,
  loading,
}: {
  error: string;
  loading: boolean;
}) {
  return error ? (
    <p className="error" role="alert">
      {error}
    </p>
  ) : loading ? (
    <p className="muted" role="status">
      Loading…
    </p>
  ) : null;
}
export function ActionForm({
  children,
  submit,
  label = "Save",
  onDone,
  disabled = false,
  done = "Saved successfully.",
}: {
  children: ReactNode;
  submit: (data: FormData) => Promise<unknown>;
  label?: string;
  onDone?: () => void;
  disabled?: boolean;
  // What to say when submit succeeds; empty when the result speaks for itself.
  done?: string;
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [success, setSuccess] = useState("");
  async function handle(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (busy) return;
    const form = e.currentTarget;
    setBusy(true);
    setError("");
    setSuccess("");
    try {
      await submit(new FormData(form));
      setSuccess(done);
      onDone?.();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <form onSubmit={handle}>
      {children}
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {success && (
        <p className="success" role="status">
          {success}
        </p>
      )}
      <button disabled={disabled || busy} className="primary" type="submit">
        {busy ? "Working…" : label}
      </button>
    </form>
  );
}
export function AsyncButton({
  children,
  run,
  danger = false,
  primary = false,
  disabled = false,
}: {
  children: ReactNode;
  run: () => Promise<unknown>;
  danger?: boolean;
  primary?: boolean;
  disabled?: boolean;
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  return (
    <span className="action">
      <button
        className={danger ? "danger" : primary ? "primary" : ""}
        disabled={disabled || busy}
        onClick={async () => {
          setBusy(true);
          setError("");
          try {
            await run();
          } catch (e) {
            setError((e as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        {busy ? "Working…" : children}
      </button>
      {error && (
        <small className="error" role="alert">
          {error}
        </small>
      )}
    </span>
  );
}
export function OneTime({
  title,
  value,
  close,
}: {
  title: string;
  value: RecordData;
  close: () => void;
}) {
  return (
    <section className="notice" role="status">
      <div className="row">
        <h3>{title}</h3>
        <button onClick={close}>Dismiss</button>
      </div>
      <p>Copy these values now. They will not be shown again.</p>
      <JSONView value={value} />
      <AsyncButton run={() => navigator.clipboard.writeText(pretty(value))}>
        Copy values
      </AsyncButton>
    </section>
  );
}
// Field names become labels people read: max_rows is "Max rows", sql is "SQL".
const acronyms: Record<string, string> = {
  sql: "SQL",
  id: "ID",
  url: "URL",
  html: "HTML",
};
export const humanize = (name: string) =>
  name
    .split("_")
    .map(
      (w, i) =>
        acronyms[w] || (i === 0 ? w.charAt(0).toUpperCase() + w.slice(1) : w),
    )
    .join(" ");
// Long text gets room to breathe.
const multiline = [
  "sql",
  "text",
  "html",
  "content",
  "prompt",
  "body",
  "description",
];
export function InputFields({
  schema,
  expressions = false,
}: {
  schema: RecordData;
  expressions?: boolean;
}) {
  const fields = schema?.properties || schema || {};
  return (
    <>
      {Object.entries(fields).map(([name, raw]) => {
        const s = raw as RecordData;
        if (!s || typeof s !== "object") return null;
        return (
          <Field key={name} label={humanize(name)} hint={s.description}>
            {s.enum ? (
              <select
                name={name}
                required={
                  s.required === true || schema.required?.includes(name)
                }
                defaultValue={s.default ?? ""}
              >
                <option value="">Select…</option>
                {s.enum.map((v: unknown) => (
                  <option key={String(v)} value={String(v)}>
                    {String(v)}
                  </option>
                ))}
              </select>
            ) : ["object", "array"].includes(s.type) ||
              (s.type === "string" && multiline.includes(name)) ? (
              <textarea
                name={name}
                className={s.type === "string" ? "" : "code"}
                rows={name === "sql" ? 5 : undefined}
                spellCheck={s.type === "string" && name !== "sql"}
                placeholder={
                  s.type === "array" ? "[]" : s.type === "object" ? "{}" : ""
                }
                defaultValue={
                  typeof s.default === "string" &&
                  (s.type === "string" || s.default.includes("${"))
                    ? s.default
                    : s.default !== undefined
                      ? pretty(s.default)
                      : ""
                }
              />
            ) : s.type === "boolean" ? (
              <select
                name={name}
                defaultValue={s.default === undefined ? "" : String(s.default)}
              >
                <option value="">Default</option>
                <option>true</option>
                <option>false</option>
              </select>
            ) : (
              <input
                name={name}
                type={
                  !expressions && (s.type === "number" || s.type === "integer")
                    ? "number"
                    : "text"
                }
                step={s.type === "number" ? "any" : undefined}
                required={
                  s.required === true || schema.required?.includes(name)
                }
                defaultValue={s.default ?? ""}
              />
            )}
          </Field>
        );
      })}
    </>
  );
}
function isJSON(text: string) {
  try {
    JSON.parse(text);
    return true;
  } catch {
    return false;
  }
}
export function readInput(
  data: FormData,
  schema: RecordData,
  expressions = false,
) {
  const result: RecordData = {};
  for (const [name, raw] of Object.entries(
    schema?.properties || schema || {},
  )) {
    const s = raw as RecordData;
    const v = data.get(name);
    if (v === "" || v === null) continue;
    // A list or object may hold expressions inside it, ["${run.id}"]; it is
    // still a list. Only a value that is not JSON stays a whole expression.
    const json = ["object", "array"].includes(s.type);
    result[name] =
      expressions && String(v).includes("${") && !(json && isJSON(String(v)))
        ? v
        : json
          ? JSON.parse(String(v))
          : s.type === "boolean"
            ? v === "true"
            : ["number", "integer"].includes(s.type)
              ? Number(v)
              : v;
  }
  return result;
}

// Shows a freshly created invite: the one-time link to hand to the invitee.
export function InviteNotice({
  invite,
  close,
}: {
  invite: RecordData;
  close: () => void;
}) {
  const link =
    invite.accept_url ||
    `${location.origin}/ui/accept-invite?token=${invite.token}`;
  return (
    <section className="notice" role="status">
      <div className="row spread">
        <h3>Invite link</h3>
        <button onClick={close}>Dismiss</button>
      </div>
      <p>
        Send this link to {invite.email}. It works once
        {invite.expires_at
          ? ` and expires on ${new Date(invite.expires_at).toLocaleDateString()}`
          : ""}
        . It is shown only now, and invites are not emailed automatically.
      </p>
      <div className="invite-link">
        <input
          readOnly
          value={link}
          aria-label="Invite link"
          onFocus={(e) => e.currentTarget.select()}
        />
        <AsyncButton run={() => navigator.clipboard.writeText(link)}>
          Copy link
        </AsyncButton>
      </div>
    </section>
  );
}
