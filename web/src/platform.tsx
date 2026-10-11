import { useState } from "react";
import { api, type RecordData } from "./api";
import {
  useResource,
  Field,
  ActionForm,
  Empty,
  InviteNotice,
  ResourceState,
} from "./components";

// Platform administration: bring a new customer organization onto the platform
// and manage the plan each organization is on. Only platform admins see this.
export function Platform() {
  const orgs = useResource("/admin/organizations"),
    plans = useResource("/plans");
  const [created, setCreated] = useState<RecordData | null>(null),
    [planError, setPlanError] = useState("");
  return (
    <>
      {created && (
        <InviteNotice invite={created} close={() => setCreated(null)} />
      )}
      <section className="panel">
        <h2>Invite a customer organization</h2>
        <p className="muted">
          The person you invite becomes the owner of a new, empty organization
          of their own. They can then invite their own team. Use the Team page
          instead to add people to an organization you already belong to.
        </p>
        <ActionForm
          label="Create organization invite"
          done=""
          onDone={orgs.reload}
          submit={async (data) =>
            setCreated(
              await api("/admin/invites", "POST", {
                email: data.get("email"),
                organization_name: data.get("organization_name"),
              }),
            )
          }
        >
          <div className="split">
            <Field label="Owner email">
              <input type="email" name="email" required />
            </Field>
            <Field label="Organization name">
              <input name="organization_name" required />
            </Field>
          </div>
        </ActionForm>
      </section>
      <section className="panel">
        <h2>Organizations</h2>
        <ResourceState
          error={orgs.error || plans.error}
          loading={!orgs.data || !plans.data}
        />
        {planError && (
          <p className="error" role="alert">
            {planError}
          </p>
        )}
        {orgs.data?.items?.length === 0 && <Empty>No organizations yet.</Empty>}
        {!!orgs.data?.items?.length && (
          <div className="table-wrap">
            <table className="platform-orgs">
              <thead>
                <tr>
                  <th>Organization</th>
                  <th>Plan</th>
                  <th>Members</th>
                  <th>Runs this month</th>
                </tr>
              </thead>
              <tbody>
                {orgs.data.items.map((o: RecordData) => (
                  <tr key={o.id}>
                    <td>{o.name}</td>
                    <td>
                      <select
                        aria-label={`Plan for ${o.name}`}
                        value={o.plan}
                        onChange={async (e) => {
                          setPlanError("");
                          try {
                            await api(
                              `/admin/organizations/${o.id}/plan`,
                              "PUT",
                              { plan: e.target.value },
                            );
                          } catch (err) {
                            setPlanError((err as Error).message);
                          }
                          orgs.reload();
                        }}
                      >
                        {plans.data?.items?.map((p: RecordData) => (
                          <option key={p.id} value={p.id}>
                            {p.name}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td>{o.members}</td>
                    <td>{o.runs_this_month}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}
