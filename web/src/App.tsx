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
const SharedPage = lazy(() => import("@/app/shared/[token]/page"));
const HelpLayout = lazy(() => import("@/app/help/layout"));
const HelpIndex = lazy(() => import("@/app/help/page"));
const GuidePage = lazy(() => import("@/app/help/[slug]/page"));
const SettingsLayout = lazy(() => import("@/app/settings/layout"));
const GeneralSettings = lazy(() => import("@/app/settings/page"));
const ConnectionsPage = lazy(() => import("@/app/settings/connections/page"));
const SharedFilesPage = lazy(() => import("@/app/settings/shared-files/page"));
const SkillsPage = lazy(() => import("@/app/settings/skills/page"));
const TeamPage = lazy(() => import("@/app/settings/team/page"));
const AdminOverview = lazy(() => import("@/app/settings/admin/page"));
const AdminDoctor = lazy(() => import("@/app/settings/admin/doctor/page"));
const AdminFeatures = lazy(() => import("@/app/settings/admin/features/page"));
const AdminNotifications = lazy(() => import("@/app/settings/admin/notifications/page"));
const AdminProviders = lazy(() => import("@/app/settings/admin/providers/page"));
const AdminServer = lazy(() => import("@/app/settings/admin/server/page"));
const AdminUsers = lazy(() => import("@/app/settings/admin/users/page"));

const settingsPages: Record<string, () => ReactNode> = {
  "/settings": () => <GeneralSettings />,
  "/settings/connections": () => <ConnectionsPage />,
  "/settings/shared-files": () => <SharedFilesPage />,
  "/settings/skills": () => <SkillsPage />,
  "/settings/team": () => <TeamPage />,
  "/settings/admin": () => <AdminOverview />,
  "/settings/admin/doctor": () => <AdminDoctor />,
  "/settings/admin/features": () => <AdminFeatures />,
  "/settings/admin/notifications": () => <AdminNotifications />,
  "/settings/admin/providers": () => <AdminProviders />,
  "/settings/admin/server": () => <AdminServer />,
  "/settings/admin/users": () => <AdminUsers />,
};

const GUIDE_TITLES: Record<string, string> = {
  chat: "Chat guide",
  "operations-center": "Operations Center guide",
};

/** route resolves a pathname to its page and (for /help) its title. */
export function route(pathname: string): { page: ReactNode; title?: string } {
  const path = pathname.length > 1 ? pathname.replace(/\/+$/, "") : pathname;
  if (getBootstrap().notFound && path === initialPath) return { page: <NotFound /> };
  if (path === "/chat") return { page: <ChatPage /> };
  if (path === "/login") return { page: <LoginPage /> };
  if (path === "/no-access") return { page: <NoAccessPage /> };
  if (path === "/orchestrator") return { page: <OrchestratorPage /> };
  if (path.startsWith("/shared/")) return { page: <SharedPage /> };
  if (path === "/help") return { page: <HelpLayout><HelpIndex /></HelpLayout>, title: "Guides" };
  const guide = /^\/help\/([^/]+)$/.exec(path);
  if (guide && GUIDE_TITLES[guide[1]]) {
    return { page: <HelpLayout><GuidePage slug={guide[1]} /></HelpLayout>, title: GUIDE_TITLES[guide[1]] };
  }
  const settings = settingsPages[path];
  if (settings) return { page: <SettingsLayout>{settings()}</SettingsLayout> };
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
  useEffect(() => {
    if (pathname === "/chat") return;
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
