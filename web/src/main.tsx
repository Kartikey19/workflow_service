import {
  Component,
  useEffect,
  useState,
  lazy,
  Suspense,
  type ReactNode,
} from "react";
import { createRoot } from "react-dom/client";
import {
  Workflow,
  Activity,
  Bot,
  Plug,
  LockKeyhole,
  Webhook,
  Users,
  KeyRound,
  ChartNoAxesCombined,
  ListTodo,
  LogOut,
  Menu,
  Building2,
  Sparkles,
} from "lucide-react";
import { api, APIError, type RecordData } from "./api";
import {
  ActionForm,
  Field,
  ResourceState,
  AsyncButton,
  Logo,
  StoryGraph,
  PRODUCT_NAME,
} from "./components";
const Workflows = lazy(() =>
  import("./workflows").then((m) => ({ default: m.Workflows })),
);
import { Runs } from "./runs";
import { Platform } from "./platform";
const Providers = lazy(() =>
  import("./providers").then((m) => ({ default: m.Providers })),
);
const Agents = lazy(() =>
  import("./agents").then((m) => ({ default: m.Agents })),
);
import {
  Connections,
  Vault,
  Webhooks,
  Team,
  APIKeys,
  Usage,
  Tasks,
} from "./settings";
import "./style.css";
const pages = [
  ["workflows", "Workflows", Workflow, "Build"],
  ["agents", "Agents", Bot, "Build"],
  ["providers", "AI providers", Sparkles, "Build"],
  ["runs", "Runs", Activity, "Operate"],
  ["tasks", "Human tasks", ListTodo, "Operate"],
  ["connectors", "Connectors", Plug, "Connect"],
  ["vault", "Vault", LockKeyhole, "Connect"],
  ["webhooks", "Webhooks", Webhook, "Connect"],
  ["team", "Team", Users, "Manage"],
  ["api-keys", "API keys", KeyRound, "Manage"],
  ["usage", "Usage & plans", ChartNoAxesCombined, "Manage"],
  ["platform", "Platform", Building2, "Platform"],
] as const;
const groups = ["Build", "Operate", "Connect", "Manage", "Platform"] as const;
function SignIn({ onSession }: { onSession: (session: RecordData) => void }) {
  const token =
    location.pathname === "/ui/accept-invite"
      ? new URLSearchParams(location.search).get("token")
      : null;
  const [invite, setInvite] = useState<RecordData | null>(null),
    [error, setError] = useState("");
  useEffect(() => {
    if (!token) return;
    const abort = new AbortController();
    api(
      `/auth/invite?token=${encodeURIComponent(token)}`,
      "GET",
      undefined,
      abort.signal,
    )
      .then(setInvite)
      .catch((e) => {
        if (!abort.signal.aborted) setError(e.message);
      });
    return () => abort.abort();
  }, [token]);
  return (
    <main className="auth-shell">
      <section className="auth-story">
        <a className="brand" href="/ui/">
          <Logo />
          <span>{PRODUCT_NAME}</span>
        </a>
        <div>
          <span className="eyebrow">Durable workflows for teams</span>
          <h1>
            Operations,
            <br />
            <em>woven together.</em>
          </h1>
          <p>
            Connect your tools, AI agents and people in workflows that keep
            running. Every step is visible and every run is accounted for.
          </p>
          <div className="story-graph">
            <StoryGraph />
          </div>
        </div>
        <small>
          {PRODUCT_NAME} runs each step once, retries safely, and waits for
          people as long as it takes.
        </small>
      </section>
      <section className="auth-form">
        <div className="auth-card">
          <span className="eyebrow">
            {token ? "You’re invited" : "Welcome back"}
          </span>
          <h2>{token ? "Join your team" : "Sign in to your workspace"}</h2>
          {token && (
            <>
              <ResourceState error={error} loading={!invite && !error} />
              {invite && (
                <div className="invite-card">
                  <strong>{invite.organization_name || "Your team"}</strong>
                  <small>
                    {invite.email} · <span className="cap">{invite.role}</span>
                  </small>
                </div>
              )}
            </>
          )}
          <ActionForm
            label={token ? "Accept invite" : "Sign in"}
            disabled={!!token && !invite}
            submit={async (data) => {
              const session = await api(
                token ? "/auth/accept-invite" : "/auth/login",
                "POST",
                token
                  ? {
                      token,
                      password: data.get("password"),
                      name: data.get("name"),
                    }
                  : {
                      email: data.get("email"),
                      password: data.get("password"),
                    },
              );
              history.replaceState({}, "", "/ui/workflows");
              onSession(session);
            }}
          >
            {token ? (
              <Field label="Your name">
                <input name="name" required autoComplete="name" />
              </Field>
            ) : (
              <Field label="Email">
                <input
                  name="email"
                  type="email"
                  required
                  autoComplete="username"
                />
              </Field>
            )}
            <Field
              label="Password"
              hint={
                token
                  ? invite?.has_account
                    ? "Enter your existing account password."
                    : "Choose a password with at least 12 characters."
                  : undefined
              }
            >
              <input
                name="password"
                type="password"
                required
                minLength={token && !invite?.has_account ? 12 : undefined}
                autoComplete={token ? "new-password" : "current-password"}
              />
            </Field>
          </ActionForm>
          <p className="muted">
            {token
              ? "Your invite connects you to this organization."
              : "Ask your organization administrator for an invite if you don’t have an account."}
          </p>
        </div>
      </section>
    </main>
  );
}
// One page failing must not blank the whole workspace.
class PageBoundary extends Component<
  { children: ReactNode },
  { error: string }
> {
  state = { error: "" };
  static getDerivedStateFromError(e: Error) {
    return { error: e.message };
  }
  render() {
    return this.state.error ? (
      <section className="panel" role="alert">
        <h2>This page hit a problem</h2>
        <p className="muted">{this.state.error}</p>
        <button onClick={() => location.reload()}>Reload page</button>
      </section>
    ) : (
      this.props.children
    );
  }
}
function App() {
  const [session, setSession] = useState<RecordData | null>(null),
    [loading, setLoading] = useState(true),
    [error, setError] = useState(""),
    [page, setPage] = useState(
      (location.pathname.slice("/ui/".length) || "workflows").replace(
        /^connections$/,
        "connectors",
      ),
    ),
    [agentName, setAgentName] = useState(""),
    [runID, setRunID] = useState(""),
    [menu, setMenu] = useState(false);
  useEffect(() => {
    const abort = new AbortController();
    api("/auth/me", "GET", undefined, abort.signal)
      .then(setSession)
      .catch((e) => {
        if (
          !abort.signal.aborted &&
          (!(e instanceof APIError) || e.status !== 401)
        )
          setError(e.message);
      })
      .finally(() => {
        if (!abort.signal.aborted) setLoading(false);
      });
    const pop = () =>
      setPage(
        (location.pathname.slice("/ui/".length) || "workflows").replace(
          /^connections$/,
          "connectors",
        ),
      );
    addEventListener("popstate", pop);
    return () => {
      abort.abort();
      removeEventListener("popstate", pop);
    };
  }, []);
  function navigate(page: string) {
    setPage(page);
    history.pushState({}, "", `/ui/${page}`);
    setMenu(false);
  }
  if (loading)
    return (
      <div className="boot" role="status">
        Opening your workspace…
      </div>
    );
  if (!session)
    return (
      <>
        {error && (
          <p className="global-error" role="alert">
            {error}
          </p>
        )}
        <SignIn
          onSession={(s) => {
            setSession(s);
            setPage("workflows");
          }}
        />
      </>
    );
  const role = session.organization.role,
    admin = ["owner", "admin"].includes(role),
    editable = admin || role === "editor",
    currentPage = pages.find((p) => p[0] === page) || pages[0];
  const visible = pages
    .filter((p) => p[0] !== "platform" || session.user.platform_admin)
    .filter((p) => admin || p[0] !== "api-keys")
    .filter(
      (p) =>
        editable ||
        !["connectors", "providers", "vault", "webhooks"].includes(p[0]),
    );
  const allowed = visible.some((p) => p[0] === currentPage[0]);
  return (
    <div className={`app-shell ${menu ? "menu-open" : ""}`}>
      {menu && (
        <button
          className="menu-backdrop"
          aria-label="Close navigation"
          onClick={() => setMenu(false)}
        />
      )}
      <aside className="sidebar">
        <a
          className="brand"
          href="/ui/workflows"
          onClick={(e) => {
            e.preventDefault();
            navigate("workflows");
          }}
        >
          <Logo />
          <span>{PRODUCT_NAME}</span>
        </a>
        <Field label="Organization">
          <select
            value={session.organization.id}
            onChange={async (e) => {
              try {
                await api("/auth/switch-org", "POST", {
                  organization_id: e.target.value,
                });
                const next = await api("/auth/me");
                setAgentName("");
                setRunID("");
                setSession(next);
                setError("");
              } catch (e) {
                setError((e as Error).message);
              }
            }}
          >
            {session.organizations.map((o: RecordData) => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </select>
        </Field>
        <nav aria-label="Workspace">
          {groups.map((group) => {
            const items = visible.filter((p) => p[3] === group);
            return (
              items.length > 0 && (
                <div className="nav-group" key={group}>
                  <span className="nav-label">{group}</span>
                  {items.map(([id, label, Icon]) => (
                    <button
                      key={id}
                      className={currentPage[0] === id ? "active" : ""}
                      aria-current={currentPage[0] === id ? "page" : undefined}
                      onClick={() => {
                        if (id === "workflows") setAgentName("");
                        navigate(id);
                      }}
                    >
                      <Icon size={18} />
                      {label}
                    </button>
                  ))}
                </div>
              )
            );
          })}
        </nav>
        <div className="sidebar-footer">
          <div className="avatar">
            {(session.user.name || session.user.email)
              .slice(0, 1)
              .toUpperCase()}
          </div>
          <div>
            <strong>{session.user.name || session.user.email}</strong>
            <small>{role}</small>
          </div>
          <AsyncButton
            run={async () => {
              await api("/auth/logout", "POST");
              setSession(null);
            }}
          >
            <LogOut size={16} />
            <span className="sr-only">Sign out</span>
          </AsyncButton>
        </div>
      </aside>
      <main className="main">
        <header className="page-header">
          <div className="row">
            <button
              className="mobile-menu"
              aria-label="Toggle navigation"
              onClick={() => setMenu(!menu)}
            >
              <Menu />
            </button>
            <div>
              <span className="eyebrow">{session.organization.name}</span>
              <h1>{currentPage[1]}</h1>
            </div>
          </div>
          <span
            className="workspace-badge"
            title="Your role in this organization"
          >
            <span /> {role}
          </span>
        </header>
        {error && (
          <p className="error" role="alert">
            {error}
            <button onClick={() => setError("")}>Dismiss</button>
          </p>
        )}
        <div key={session.organization.id} className="page-content">
          <PageBoundary key={currentPage[0]}>
            <Suspense fallback={<p role="status">Opening page…</p>}>
              {!allowed ? (
                <p className="error">Your role does not allow this page.</p>
              ) : currentPage[0] === "workflows" ? (
                <Workflows
                  admin={admin}
                  key={agentName}
                  editable={editable}
                  agentName={agentName}
                  organizationID={session.organization.id}
                  onRun={(id) => {
                    setRunID(id);
                    navigate("runs");
                  }}
                />
              ) : currentPage[0] === "runs" ? (
                <Runs initialID={runID} editable={editable} />
              ) : currentPage[0] === "agents" ? (
                <Agents
                  editable={editable}
                  admin={admin}
                  useAgent={(name) => {
                    setAgentName(name);
                    navigate("workflows");
                  }}
                />
              ) : currentPage[0] === "providers" ? (
                <Providers admin={admin} />
              ) : currentPage[0] === "connectors" ? (
                <Connections admin={admin} />
              ) : currentPage[0] === "vault" ? (
                <Vault admin={admin} />
              ) : currentPage[0] === "webhooks" ? (
                <Webhooks admin={admin} />
              ) : currentPage[0] === "team" ? (
                <Team admin={admin} selfID={session.user.id} />
              ) : currentPage[0] === "api-keys" ? (
                <APIKeys />
              ) : currentPage[0] === "usage" ? (
                <Usage />
              ) : currentPage[0] === "platform" ? (
                <Platform />
              ) : (
                <Tasks editable={editable} />
              )}
            </Suspense>
          </PageBoundary>
        </div>
      </main>
    </div>
  );
}
createRoot(document.getElementById("root")!).render(<App />);
