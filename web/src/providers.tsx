import { useState } from "react";
import { Bot, Cpu, Plug, Search } from "lucide-react";
import { api, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  AsyncButton,
  Empty,
  ResourceState,
} from "./components";

const kindIcon = (id: string) =>
  id === "ollama" ? Cpu : id === "openai_compatible" ? Plug : Bot;

// The language models agents run on. Anthropic, OpenAI and Gemini use their own
// APIs; every other provider speaks the OpenAI API at a known endpoint, and a
// local Ollama needs no key.
export function Providers({ admin }: { admin: boolean }) {
  const list = useResource("/providers"),
    kinds = useResource("/provider-kinds");
  const [adding, setAdding] = useState(false),
    [kind, setKind] = useState("anthropic"),
    [filter, setFilter] = useState(""),
    [models, setModels] = useState<{ name: string; models: string[] } | null>(
      null,
    );
  const catalog: RecordData[] = kinds.data?.items || [];
  const title = (id: string) => catalog.find((k) => k.id === id)?.title || id;
  const chosen = catalog.find((k) => k.id === kind);
  const shown = catalog.filter((k) =>
    k.title.toLowerCase().includes(filter.trim().toLowerCase()),
  );
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          The language models your agents run on. Add a provider once, then pick
          it on any agent.
        </p>
        {admin && (
          <button className="primary" onClick={() => setAdding(!adding)}>
            Add provider
          </button>
        )}
      </div>
      <ResourceState error={list.error || kinds.error} loading={!list.data} />
      {models && (
        <section className="notice" role="status">
          <div className="row spread">
            <h3>{models.name} is connected</h3>
            <button onClick={() => setModels(null)}>Dismiss</button>
          </div>
          <p>
            {models.models.length
              ? `${models.models.length} models available, including ${models.models.slice(0, 6).join(", ")}.`
              : "The provider answered but listed no models."}
          </p>
        </section>
      )}
      <div className="cards">
        {list.data?.items?.map((p: RecordData) => {
          const Icon = kindIcon(p.provider);
          return (
            <article className="panel card" key={p.id}>
              <div className="card-head">
                <span className="card-icon">
                  <Icon size={18} aria-hidden="true" />
                </span>
                <div>
                  <h2>{p.name}</h2>
                  <small>{title(p.provider)}</small>
                </div>
              </div>
              <small>{p.base_url || "Provider’s own endpoint"}</small>
              <small>
                {p.agents
                  ? `Used by ${p.agents} ${p.agents === 1 ? "agent" : "agents"}`
                  : "Not used by an agent yet"}
              </small>
              {admin && (
                <div className="row">
                  <AsyncButton
                    run={async () => {
                      const r = await api(`/providers/${p.id}/test`, "POST");
                      setModels({ name: p.name, models: r.models || [] });
                    }}
                  >
                    Test connection
                  </AsyncButton>
                  <AsyncButton
                    danger
                    run={async () => {
                      if (confirm(`Delete ${p.name}?`)) {
                        await api(`/providers/${p.id}`, "DELETE");
                        list.reload();
                      }
                    }}
                  >
                    Delete
                  </AsyncButton>
                </div>
              )}
            </article>
          );
        })}
      </div>
      {list.data && !list.data.items.length && (
        <Empty>
          No providers yet. Add Anthropic, OpenAI, a local Ollama or any other
          provider to give your agents a model.
        </Empty>
      )}
      {adding && admin && (
        <section className="panel">
          <div className="row spread">
            <h2>Add provider</h2>
            <button onClick={() => setAdding(false)}>Close</button>
          </div>
          <label className="creator-search provider-search">
            <Search size={15} aria-hidden="true" />
            <input
              aria-label="Search providers"
              placeholder="Search providers"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
          </label>
          <div className="app-grid" role="radiogroup" aria-label="Provider">
            {shown.map((k) => {
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
                  <Icon size={18} aria-hidden="true" />
                  {k.title}
                </button>
              );
            })}
          </div>
          {chosen && (
            <ActionForm
              key={kind}
              label="Test and save"
              done=""
              submit={async (data) => {
                const r = await api("/providers", "POST", {
                  name: data.get("name"),
                  provider: kind,
                  api_key: data.get("api_key") || "",
                  base_url: chosen.native ? "" : data.get("base_url"),
                });
                setModels({ name: r.name, models: r.models || [] });
                setAdding(false);
                list.reload();
              }}
            >
              <Field label="Connection name">
                <input
                  name="name"
                  required
                  defaultValue={kind.replaceAll("_", "-")}
                  pattern="[a-z0-9][a-z0-9_\-]*"
                />
              </Field>
              {!chosen.native && (
                <Field
                  label="Base URL"
                  hint={
                    kind === "openai_compatible"
                      ? "Any server that speaks the OpenAI chat API, such as vLLM or LM Studio."
                      : kind === "ollama"
                        ? "Where Ollama runs, as the server sees it."
                        : "Change only to go through a proxy."
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
        </section>
      )}
    </>
  );
}
