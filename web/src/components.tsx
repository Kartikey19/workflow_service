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
      .then(setData)
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
}: {
  children: ReactNode;
  submit: (data: FormData) => Promise<unknown>;
  label?: string;
  onDone?: () => void;
  disabled?: boolean;
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
      setSuccess("Saved successfully.");
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
  disabled = false,
}: {
  children: ReactNode;
  run: () => Promise<unknown>;
  danger?: boolean;
  disabled?: boolean;
}) {
  const [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  return (
    <span className="action">
      <button
        className={danger ? "danger" : ""}
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
          <Field key={name} label={name} hint={s.description}>
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
            ) : ["object", "array"].includes(s.type) ? (
              <textarea
                name={name}
                placeholder={s.type === "array" ? "[]" : "{}"}
                defaultValue={
                  typeof s.default === "string" && s.default.includes("${")
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
    result[name] =
      expressions && String(v).includes("${")
        ? v
        : ["object", "array"].includes(s.type)
          ? JSON.parse(String(v))
          : s.type === "boolean"
            ? v === "true"
            : ["number", "integer"].includes(s.type)
              ? Number(v)
              : v;
  }
  return result;
}
