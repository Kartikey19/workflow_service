import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Search, X } from "lucide-react";
import { api, enc, type RecordData } from "./api";
import { ActionForm, Field, ResourceState, useResource } from "./components";
import { kindIcon } from "./connectors";

// A closed setup form cannot apply a late response to the workflow draft.
function useSetupSave(onSaved: (value: RecordData) => void) {
  const controller = useRef<AbortController | null>(null);
  const callback = useRef(onSaved);
  callback.current = onSaved;
  useEffect(() => {
    const request = new AbortController();
    controller.current = request;
    return () => request.abort();
  }, []);
  return {
    controller,
    save: (value: RecordData) => {
      if (controller.current && !controller.current.signal.aborted)
        callback.current(value);
    },
  };
}

export function SetupModal({
  title,
  children,
  onClose,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const titleID = useId();
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    dialog.current?.showModal();
    return () => {
      dialog.current?.close();
      previous?.focus();
    };
  }, []);
  return createPortal(
    <dialog
      ref={dialog}
      className="setup-modal"
      aria-labelledby={titleID}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <header className="row spread">
        <h2 id={titleID}>{title}</h2>
        <button
          type="button"
          className="icon-button"
          aria-label="Close setup"
          onClick={onClose}
        >
          <X size={20} />
        </button>
      </header>
      {children}
    </dialog>,
    document.body,
  );
}

export function ProviderForm({
  initialKind = "anthropic",
  onSaved,
}: {
  initialKind?: string;
  onSaved: (provider: RecordData) => void;
}) {
  const kinds = useResource("/provider-kinds");
  const { controller, save } = useSetupSave(onSaved);
  const [kind, setKind] = useState(initialKind),
    [search, setSearch] = useState("");
  const catalog: RecordData[] = kinds.data?.items || [];
  const chosen = catalog.find((k) => k.id === kind);
  return (
    <>
      <p className="muted">
        Connect an account once, then choose its model in each workflow. Add
        separate accounts for different clients or API keys.
      </p>
      <ResourceState loading={!kinds.data} error={kinds.error} />
      <label className="creator-search provider-search">
        <Search size={15} aria-hidden="true" />
        <input
          aria-label="Search providers"
          placeholder="Search providers"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </label>
      <div className="app-grid" role="radiogroup" aria-label="Provider">
        {catalog
          .filter((k) =>
            k.title.toLowerCase().includes(search.toLowerCase().trim()),
          )
          .map((k) => {
            const Icon = kindIcon(k.id);
            return (
              <button
                key={k.id}
                type="button"
                role="radio"
                aria-checked={kind === k.id}
                className="app-tile"
                onClick={() => setKind(k.id)}
              >
                <Icon size={20} aria-hidden="true" />
                {k.title}
              </button>
            );
          })}
      </div>
      {chosen && <h3>{chosen.title} account</h3>}
      {chosen && (
        <ActionForm
          key={kind}
          label="Test and save"
          done=""
          submit={async (data) =>
            save(
              await api(
                "/providers",
                "POST",
                {
                  name: data.get("name"),
                  provider: kind,
                  api_key: data.get("api_key") || "",
                  base_url: chosen.native ? "" : data.get("base_url"),
                },
                controller.current?.signal,
              ),
            )
          }
        >
          <Field
            label="Connection name"
            hint="Use a distinct name for each client or account, such as acme-openai."
          >
            <input
              name="name"
              required
              defaultValue={kind.replaceAll("_", "-")}
              pattern="[a-z0-9][a-z0-9_\-]*"
              maxLength={63}
            />
          </Field>
          {!chosen.native && (
            <Field
              label="Base URL"
              hint={
                kind === "ollama"
                  ? "Where Ollama runs, as the server sees it."
                  : "Use the provider’s endpoint, or your own compatible server."
              }
            >
              <input
                name="base_url"
                type="url"
                required={!chosen.base_url}
                defaultValue={chosen.base_url}
                placeholder="https://api.example.com/v1"
              />
            </Field>
          )}
          <Field
            label="API key"
            hint={
              chosen.key_optional
                ? "Leave blank for a server that needs no key."
                : "Stored encrypted; it is never shown again."
            }
          >
            <input
              name="api_key"
              type="password"
              required={!chosen.key_optional}
              autoComplete="off"
            />
          </Field>
        </ActionForm>
      )}
    </>
  );
}

export function ModelFields({
  providers,
  connection,
  model,
  onChange,
  onAdd,
  disabled = false,
  connectionDisabled = false,
  modelDisabled = false,
  modelRequired = true,
}: {
  providers: RecordData[];
  connection: string;
  model: string;
  onChange: (
    connection: string,
    model: string,
    changed: "connection" | "model",
  ) => void;
  onAdd?: (kind?: string) => void;
  disabled?: boolean;
  connectionDisabled?: boolean;
  modelDisabled?: boolean;
  modelRequired?: boolean;
}) {
  const kinds = useResource(!disabled && onAdd ? "/provider-kinds" : null);
  const catalog: RecordData[] =
    kinds.data?.items ||
    [...new Set(providers.map((p) => p.provider))].map((id) => ({
      id,
      title: id.replaceAll("_", " "),
    }));
  const provider = providers.find((p) => p.name === connection);
  const models = useResource(
    provider ? `/providers/${provider.id}/models` : null,
  );
  const id = useId();
  return (
    <div className="model-fields">
      <Field
        label="Provider account"
        hint="Each account uses its own saved API key."
      >
        <select
          name="connection"
          value={connection}
          disabled={disabled || connectionDisabled}
          onChange={(e) => {
            if (e.target.value.startsWith("add:")) {
              onAdd?.(e.target.value.slice(4));
              return;
            }
            const next = providers.find((p) => p.name === e.target.value);
            onChange(
              e.target.value,
              provider && next && provider.provider !== next.provider
                ? ""
                : model,
              "connection",
            );
          }}
        >
          <option value="">Platform provider</option>
          {connection && !provider && (
            <option value={connection}>
              {connection}
              {disabled ? "" : " (unavailable)"}
            </option>
          )}
          {catalog.map((kind) => (
            <optgroup key={kind.id} label={kind.title}>
              {providers
                .filter((p) => p.provider === kind.id)
                .map((p) => (
                  <option key={p.id} value={p.name}>
                    {p.name}
                    {p.key_hint ? ` · ${p.key_hint}` : ""}
                  </option>
                ))}
              {onAdd && (
                <option value={`add:${kind.id}`}>
                  Add {kind.title} account…
                </option>
              )}
            </optgroup>
          ))}
        </select>
      </Field>
      {onAdd && !disabled && (
        <button type="button" onClick={() => onAdd()}>
          Add provider account
        </button>
      )}
      <Field
        label="Model"
        hint={
          disabled
            ? "Saved model for this step."
            : !connection
              ? "Enter a model now, or connect an account to load its available models."
              : !provider
                ? "Enter a model ID, or choose an available account to load its models."
                : models.error
                  ? "Could not load models. You can still enter a model ID and retry."
                  : !models.data
                    ? "Loading available models…"
                    : models.data.models?.length
                      ? "Choose an available model or enter a custom model ID."
                      : "This account listed no models. Enter the model ID you want to use."
        }
      >
        <input
          name="model"
          required={modelRequired}
          value={model}
          disabled={disabled || modelDisabled}
          onChange={(e) => onChange(connection, e.target.value, "model")}
          placeholder="Search or enter a model"
          list={id}
          autoComplete="off"
        />
      </Field>
      <datalist id={id}>
        {(models.data?.models || []).map((m: string) => (
          <option key={m} value={m} />
        ))}
      </datalist>
      {connection && models.error && (
        <button type="button" onClick={models.reload}>
          Retry loading models
        </button>
      )}
    </div>
  );
}

export function ConnectorForm({
  descriptor,
  editing,
  onSaved,
  popup = false,
}: {
  descriptor: RecordData;
  editing?: RecordData | null;
  onSaved: (connection: RecordData) => void;
  popup?: boolean;
}) {
  const [waiting, setWaiting] = useState(false),
    [message, setMessage] = useState("");
  const oauthWindow = useRef<Window | null>(null);
  const { controller, save } = useSetupSave(onSaved);
  const currentSaved = useRef(save);
  currentSaved.current = save;
  // An OAuth account signs in in a separate window. Only a saved account from
  // this organization completes setup; the workflow remains mounted throughout.
  const pendingName = useRef("");
  useEffect(() => {
    if (!waiting) return;
    const abort = new AbortController();
    let busy = false;
    const started = Date.now();
    const check = async () => {
      if (busy) return;
      if (!oauthWindow.current || oauthWindow.current.closed) {
        setWaiting(false);
        setMessage(
          "The sign-in window was closed. Connect the account again when you’re ready.",
        );
        return;
      }
      if (Date.now() - started > 10 * 60 * 1000) {
        setWaiting(false);
        setMessage("Sign-in timed out. Please connect the account again.");
        oauthWindow.current.close();
        return;
      }
      try {
        const returned = new URL(oauthWindow.current.location.href);
        if (returned.origin !== window.location.origin) return;
        if (returned.searchParams.has("error")) {
          setWaiting(false);
          setMessage(
            returned.searchParams.get("error") || "Account sign-in failed.",
          );
          oauthWindow.current.close();
          return;
        }
        if (returned.searchParams.get("connected") !== pendingName.current)
          return;
      } catch {
        return;
      } // OAuth is still on the external provider's origin.
      busy = true;
      try {
        const result = await api(
          `/connections/${enc(pendingName.current)}`,
          "GET",
          undefined,
          abort.signal,
        );
        if (!abort.signal.aborted && result.kind === descriptor.kind) {
          setWaiting(false);
          oauthWindow.current?.close();
          currentSaved.current(result);
        }
      } catch {
        /* Not saved yet. Keep the user's OAuth window available. */
      } finally {
        busy = false;
      }
    };
    const timer = window.setInterval(check, 1500);
    void check();
    return () => {
      abort.abort();
      window.clearInterval(timer);
    };
  }, [waiting, descriptor.kind]);
  useEffect(() => () => oauthWindow.current?.close(), []);
  return (
    <>
      <p className="muted">
        Save an account for this step. You can connect another account for each
        client.
      </p>
      <ActionForm
        key={`${descriptor.kind}-${editing?.name || ""}`}
        label={descriptor.oauth ? "Connect account" : "Test and save"}
        disabled={
          waiting || (descriptor.oauth && descriptor.oauth_configured === false)
        }
        done=""
        submit={async (data) => {
          const name = String(data.get("name"));
          if (descriptor.oauth) {
            const child = popup
              ? window.open(
                  "about:blank",
                  "_blank",
                  "popup,width=560,height=720",
                )
              : null;
            if (popup && !child)
              throw new Error(
                "Allow the sign-in popup, then try again. Your workflow stays here.",
              );
            if (child) {
              child.opener = null;
              oauthWindow.current = child;
            }
            try {
              const r = await api(
                "/connections/oauth/start",
                "POST",
                {
                  kind: descriptor.kind,
                  name,
                },
                controller.current?.signal,
              );
              if (controller.current?.signal.aborted || child?.closed) return;
              if (child) {
                oauthWindow.current = child;
                pendingName.current = name;
                child.location.href = r.authorize_url;
                setWaiting(true);
                setMessage(
                  "Finish signing in in the account window. Your workflow will stay here.",
                );
              } else window.location.assign(r.authorize_url);
            } catch (e) {
              child?.close();
              throw e;
            }
            return;
          }
          const config: RecordData = {},
            secret: Record<string, string> = {};
          descriptor.fields?.forEach((f: RecordData) => {
            const value = data.get(f.name);
            if (value === null || value === "") return;
            if (f.secret) secret[f.name] = String(value);
            else
              config[f.name] =
                f.type === "int"
                  ? Number(value)
                  : f.type === "bool"
                    ? value === "true"
                    : value;
          });
          save(
            await api(
              editing ? `/connections/${enc(editing.name)}` : "/connections",
              editing ? "PUT" : "POST",
              { name, kind: descriptor.kind, config, secret },
              controller.current?.signal,
            ),
          );
        }}
      >
        <Field
          label="Connection name"
          hint="Name the client or account so you can recognize it later."
        >
          <input
            name="name"
            required
            defaultValue={editing?.name}
            readOnly={!!editing}
            placeholder={`acme-${descriptor.kind}`}
            maxLength={63}
          />
        </Field>
        {descriptor.fields?.map((f: RecordData) => (
          <Field
            key={f.name}
            label={f.title}
            hint={
              f.secret && editing
                ? "Leave blank to retain the saved credential."
                : f.help
            }
          >
            {f.type === "bool" || f.type === "enum" ? (
              <select
                name={f.name}
                defaultValue={String(
                  editing?.config?.[f.name] ?? f.default ?? "",
                )}
              >
                <option value="">Default</option>
                {(f.type === "bool" ? ["true", "false"] : f.enum).map(
                  (v: string) => (
                    <option key={v}>{v}</option>
                  ),
                )}
              </select>
            ) : (
              <input
                name={f.name}
                type={
                  f.secret ? "password" : f.type === "int" ? "number" : "text"
                }
                required={f.required && !(f.secret && editing)}
                defaultValue={
                  f.secret ? "" : (editing?.config?.[f.name] ?? f.default ?? "")
                }
                autoComplete={f.secret ? "off" : undefined}
              />
            )}
          </Field>
        ))}
        {descriptor.oauth && descriptor.oauth_configured === false && (
          <p className="notice">
            Account sign-in for {descriptor.title} is not enabled in this
            workspace yet. Ask your administrator to enable it.
          </p>
        )}
      </ActionForm>
      {message && (
        <p role="status" className="notice">
          {message}
        </p>
      )}
      {waiting && (
        <button
          type="button"
          onClick={() => {
            setWaiting(false);
            setMessage("Sign-in cancelled. You can try connecting again.");
            oauthWindow.current?.close();
          }}
        >
          Cancel sign-in
        </button>
      )}
    </>
  );
}
