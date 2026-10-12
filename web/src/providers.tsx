import { useState } from "react";
import { Bot, Cpu, Plug } from "lucide-react";
import { api, type RecordData } from "./api";
import { ProviderForm } from "./setup";
import { useResource, AsyncButton, Empty, ResourceState } from "./components";

const kindIcon = (id: string) =>
  id === "ollama" ? Cpu : id === "openai_compatible" ? Plug : Bot;

// The language models agents run on. Anthropic, OpenAI and Gemini use their own
// APIs; every other provider speaks the OpenAI API at a known endpoint, and a
// local Ollama needs no key.
export function Providers({ admin }: { admin: boolean }) {
  const list = useResource("/providers"),
    kinds = useResource("/provider-kinds");
  const [adding, setAdding] = useState(false),
    [models, setModels] = useState<{ name: string; models: string[] } | null>(
      null,
    );
  const catalog: RecordData[] = kinds.data?.items || [];
  const title = (id: string) => catalog.find((k) => k.id === id)?.title || id;
  return (
    <>
      <div className="toolbar">
        <p className="muted">
          Save a named provider account for each client or API key. Choose its
          model on an agent or in a workflow step.
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
          <ProviderForm
            onSaved={(r) => {
              setModels({ name: r.name, models: r.models || [] });
              setAdding(false);
              list.reload();
            }}
          />
        </section>
      )}
    </>
  );
}
