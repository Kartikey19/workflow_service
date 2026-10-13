import { api, type RecordData } from "./api";
import { ActionForm, AsyncButton, Field } from "./components";

export function Organizations({
  session,
  onCreated,
}: {
  session: RecordData;
  onCreated: () => Promise<void>;
}) {
  return (
    <div className="split">
      <section className="panel">
        <h2>Your organizations</h2>
        <p className="muted">
          Each organization has its own workflows, team, connections, secrets,
          AI providers and settings. Your account can belong to several
          organizations with a different role in each.
        </p>
        {session.organizations.map((org: RecordData) => (
          <div className="resource-row" key={org.id}>
            <div>
              <strong>{org.name}</strong>
              <small>{org.role}</small>
            </div>
            {org.id === session.organization.id ? (
              <span>Current organization</span>
            ) : (
              <AsyncButton
                run={async () => {
                  await api("/auth/switch-org", "POST", {
                    organization_id: org.id,
                  });
                  location.assign("/ui/workflows");
                }}
              >
                Switch to {org.name}
              </AsyncButton>
            )}
          </div>
        ))}
      </section>
      <section className="panel">
        <h2>Add organization</h2>
        <p className="muted">
          Create a separate workspace for another company or project. You become
          its owner and can invite teammates from the Team page after switching
          to it.
        </p>
        <ActionForm
          label="Create organization"
          done="Organization created. Switch to it to configure its settings and invite your team."
          submit={async (data) => {
            await api("/organizations", "POST", { name: data.get("name") });
            await onCreated();
          }}
        >
          <Field label="Organization name">
            <input
              name="name"
              required
              maxLength={100}
              placeholder="Acme Operations"
            />
          </Field>
        </ActionForm>
      </section>
    </div>
  );
}
