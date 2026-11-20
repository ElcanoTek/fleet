import { t as tr } from "@/app/lib/i18n";
// The app shell: the root layout (what app/layout.tsx was) and the route table
// (what the app/ directory structure was). URLs are unchanged; the Go server
// renders index.html for each of them and does the server-side work first
// (auth gate, redirects, the membership check, the shared snapshot).
import { Component, type ErrorInfo, type ReactNode, Suspense, lazy, useEffect } from "react";
import { usePathname } from "@/app/lib/navigation";
import { getBootstrap } from "@/app/lib/bootstrap";
import { PushKeyRenewal } from "@/app/shared/PushKeyRenewal";
import { UpdateNotice } from "@/app/shared/UpdateNotice";
import { ErrorScreen } from "@/app/shared/ui/ErrorScreen";
import { NotFound } from "@/app/shared/ui/NotFound";

const ChatPage = lazy(() => import("@/app/chat/page"));
const LoginPage = lazy(() => import("@/app/login/page"));
const NoAccessPage = lazy(() => import("@/app/no-access/page"));
const OrchestratorPage = lazy(() => import("@/app/orchestrator/page"));
const AnalyticsPage = lazy(() => import("@/app/analytics/page"));
const SharedPage = lazy(() => import("@/app/shared/[token]/page"));
const HelpLayout = lazy(() => import("@/app/help/layout"));
const HelpIndex = lazy(() => import("@/app/help/page"));
const GuidePage = lazy(() => import("@/app/help/[slug]/page"));
const SettingsLayout = lazy(() => import("@/app/settings/layout"));
const GeneralSettings = lazy(() => import("@/app/settings/page"));
const ConnectionsPage = lazy(() => import("@/app/settings/connections/page"));
const SharedFilesPage = lazy(() => import("@/app/settings/shared-files/page"));
const SkillsPage = lazy(() => import("@/app/settings/skills/page"));
const PersonasPage = lazy(() => import("@/app/settings/personas/page"));
const MemoriesPage = lazy(() => import("@/app/settings/memories/page"));
const PromptsPage = lazy(() => import("@/app/settings/prompts/page"));
const TeamPage = lazy(() => import("@/app/settings/team/page"));
const AdminOverview = lazy(() => import("@/app/settings/admin/page"));
const AdminDoctor = lazy(() => import("@/app/settings/admin/doctor/page"));
const AdminFeatures = lazy(() => import("@/app/settings/admin/features/page"));
const AdminProviders = lazy(() => import("@/app/settings/admin/providers/page"));
const AdminServer = lazy(() => import("@/app/settings/admin/server/page"));
const AdminUsers = lazy(() => import("@/app/settings/admin/users/page"));
const AdminChats = lazy(() => import("@/app/settings/admin/chats/page"));
const AdminSidebar = lazy(() => import("@/app/settings/admin/sidebar/page"));

const settingsPages: Record<string, () => ReactNode> = {
  "/settings": () => <GeneralSettings />,
  "/settings/connections": () => <ConnectionsPage />,
  "/settings/shared-files": () => <SharedFilesPage />,
  "/settings/skills": () => <SkillsPage />,
  "/settings/personas": () => <PersonasPage />,
  "/settings/memories": () => <MemoriesPage />,
  "/settings/prompts": () => <PromptsPage />,
  "/settings/team": () => <TeamPage />,
  "/settings/admin": () => <AdminOverview />,
  "/settings/admin/doctor": () => <AdminDoctor />,
  "/settings/admin/features": () => <AdminFeatures />,
  "/settings/admin/providers": () => <AdminProviders />,
  "/settings/admin/server": () => <AdminServer />,
  "/settings/admin/users": () => <AdminUsers />,
  "/settings/admin/chats": () => <AdminChats />,
  "/settings/admin/sidebar": () => <AdminSidebar />,
};

// The tab title of each settings page: its own name, then the section, so
// several Fleet tabs (and the browser's history) can be told apart.
const SETTINGS_TITLES: Record<string, [name: string, section: string]> = {
  "/settings": [tr("General"), tr("Settings")],
  "/settings/connections": [tr("Connections"), tr("Settings")],
  "/settings/shared-files": [tr("Shared files"), tr("Settings")],
  "/settings/skills": [tr("Skills"), tr("Settings")],
  "/settings/personas": [tr("Personas"), tr("Settings")],
  "/settings/memories": [tr("Memories"), tr("Settings")],
  "/settings/prompts": [tr("Prompts"), tr("Settings")],
  "/settings/team": [tr("Team"), tr("Settings")],
  "/settings/admin": [tr("Overview"), tr("Admin")],
  "/settings/admin/doctor": [tr("System checks"), tr("Admin")],
  "/settings/admin/features": [tr("Features"), tr("Admin")],
  "/settings/admin/providers": [tr("Providers"), tr("Admin")],
  "/settings/admin/server": [tr("Server"), tr("Admin")],
  "/settings/admin/users": [tr("Users"), tr("Admin")],
  "/settings/admin/chats": [tr("Chats"), tr("Admin")],
  "/settings/admin/sidebar": [tr("Sidebar"), tr("Admin")],
};

const GUIDE_TITLES: Record<string, string> = {
  chat: tr("Chat guide"),
  "operations-center": tr("Background tasks guide"),
};

/** route resolves a pathname to its page and its tab title (chat sets its own). */
export function route(pathname: string): { page: ReactNode; title?: string } {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  if (getBootstrap().notFound && path === initialPath) return { page: <NotFound /> };
  if (path === "/chat") return { page: <ChatPage /> };
  if (path === "/login") return { page: <LoginPage /> };
  if (path === "/no-access") return { page: <NoAccessPage /> };
  if (path === "/orchestrator") return { page: <OrchestratorPage />, title: tr("Background tasks") };
  if (path === "/analytics") return { page: <AnalyticsPage />, title: tr("Analytics") };
  if (path.startsWith("/shared/")) return { page: <SharedPage /> };
  if (path === "/help") return { page: <HelpLayout><HelpIndex /></HelpLayout>, title: tr("Guides") };
  const guide = /^\/help\/([^/]+)$/.exec(path);
  if (guide && GUIDE_TITLES[guide[1]]) {
    return { page: <HelpLayout><GuidePage slug={guide[1]} /></HelpLayout>, title: GUIDE_TITLES[guide[1]] };
  }
  const settings = settingsPages[path];
  if (settings) {
    const [name, section] = SETTINGS_TITLES[path] ?? [tr("Settings"), ""];
    return {
      page: <SettingsLayout>{settings()}</SettingsLayout>,
      title: section ? `${name} — ${section}` : name,
    };
  }
  return { page: <NotFound /> };
}

// The path the server rendered: its notFound verdict applies to this path only,
// not to wherever the user navigates next.
const initialPath = typeof window === "undefined" ? "" : window.location.pathname.replace(/\/+$/, "") || "/";

/** ErrorBoundary is what app/error.tsx was: a crash renders the recovery screen. */
// Navigating away from a crashed page clears the error (resetKey is the
// pathname) without remounting the layouts that did not crash.
type BoundaryState = { error: Error | null; resetKey: string };
class ErrorBoundary extends Component<{ children: ReactNode; resetKey: string }, BoundaryState> {
  state: BoundaryState = { error: null, resetKey: this.props.resetKey };
  static getDerivedStateFromError(error: Error) {
    return { error };
  }
  static getDerivedStateFromProps(props: { resetKey: string }, state: BoundaryState) {
    return props.resetKey !== state.resetKey ? { error: null, resetKey: props.resetKey } : null;
  }
  componentDidCatch(error: Error, info: ErrorInfo) {
    void error;
    void info;
  }
  render() {
    return this.state.error ? <ErrorScreen error={this.state.error} /> : this.props.children;
  }
}

export function App() {
  const pathname = usePathname();
  const { page, title } = route(pathname);
  const appName = getBootstrap().branding.appName;

  // The tab title (the Next metadata). The chat view sets its own
  // "{conversation} — {app}" title once a conversation is open.
  // A shared chat names its own tab too (SharedConversationView).
  useEffect(() => {
    if (pathname === "/chat" || pathname.startsWith("/shared/")) return;
    document.title = title ? (appName.trim() ? `${title} — ${appName}` : title) : appName;
  }, [pathname, title, appName]);

  return (
    <>
      <ErrorBoundary resetKey={pathname}>
        <Suspense fallback={null}>{page}</Suspense>
      </ErrorBoundary>
      <PushKeyRenewal />
      <UpdateNotice appName={appName} />
    </>
  );
}
