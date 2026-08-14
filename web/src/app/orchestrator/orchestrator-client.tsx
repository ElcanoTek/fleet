"use client";

import { useRef, useState } from "react";
import { orchestratorApi, type Task } from "@/app/shared/lib/orchestratorApi";
import { useOrchestratorSession } from "@/app/shared/hooks/useOrchestratorSession";
import { useDashboardData } from "@/app/shared/hooks/useDashboardData";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { ToastProvider, useToast } from "@/app/shared/ui/Toast";
import { ConfirmDialog } from "@/app/shared/ui/ConfirmDialog";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";
import { NavToChat } from "@/app/shared/ui/CrossViewNav";
import { NavRail, useRailCollapse } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { Icon } from "@/app/shared/ui/Icon";
import { OrchestratorLogin } from "./OrchestratorLogin";
import { StatsGrid, type StatFilter } from "./StatsGrid";
import { ServerClock } from "./ServerClock";
import { TasksTable } from "./TasksTable";
import { SleepingTasks } from "./SleepingTasks";
import { TaskCreateModal } from "./TaskCreateModal";
import { LogViewer } from "./LogViewer";
import { SLAReportPanel } from "./SLAReportPanel";
import { UsagePanel } from "./UsagePanel";
import { AdoptionPanel } from "./AdoptionPanel";
import { DatasetsPanel } from "./DatasetsPanel";
import { UpcomingPanel } from "./UpcomingPanel";
import { taskRunLabel } from "./taskDisplay";

// OrchestratorClient — the top-level orchestrator (Operations Center) view. It
// now renders inside the shared unified rail (#169): when signed in, the
// dashboard sits in a two-column shell beside the NavRail, with New Task in the
// rail and Settings / Theme / Sign out relocated into the rail's account menu
// (the standalone header buttons are gone). The signed-out login keeps a slim
// top bar with the theme switch + a cross-link to Chat. Routing, data-fetching,
// SSE, and the dashboard body are unchanged — this is a shell change.

// OrchestratorSlimHeader — the railless top bar (theme switch + cross-link to
// Chat) shown above the pre-dashboard cards. Shared by the signed-out login
// state and the #458 no-access state so the two read identically.
function OrchestratorSlimHeader() {
  return (
    <header className="header page-header" role="banner">
      <div className="ds-app-header">
        <div className="ds-app-header__brand">
          <div className="ds-app-header__text">
            {/* Neutral, not a company name: this bar renders pre-auth on
                white-labeled deployments (BRANDING.md — nothing here may
                hardcode a brand). */}
            <p className="ds-app-header__eyebrow">Internal</p>
            <h1 className="ds-app-header__title">Operations Center</h1>
          </div>
        </div>
        <div className="ds-app-header__actions">
          <ThemeToggle />
          <NavToChat className="btn btn-ghost" />
        </div>
      </div>
    </header>
  );
}

// Top-level dashboard tabs. "tasks" is the legacy Recent Tasks view; the
// admin-only entries (sla/usage/adoption) are render-guarded below.
const DASH_TABS = ["tasks", "upcoming", "sla", "datasets", "usage", "adoption"] as const;
type DashTab = (typeof DASH_TABS)[number];

// initialDashboardTab honors a ?tab= deep link (e.g. /orchestrator?tab=adoption,
// linked from the Settings admin pages). Read lazily in the useState
// initializer, the readCallbackBanner pattern — not an effect, which trips
// react-hooks/set-state-in-effect. Safe pre-hydration: the tab row only
// renders after the async session probe, so the seeded value never differs
// from the server-rendered markup; and safe pre-role-probe: the render guard
// falls back to Recent Tasks while (or if) the caller isn't an admin.
function initialDashboardTab(): DashTab {
  if (typeof window === "undefined") return "tasks";
  const want = new URLSearchParams(window.location.search).get("tab");
  return want && (DASH_TABS as readonly string[]).includes(want) ? (want as DashTab) : "tasks";
}

function OrchestratorInner({ magicLinkLoginEnabled }: { magicLinkLoginEnabled: boolean }) {
  const session = useOrchestratorSession();
  const dashboard = useDashboardData(session.signedIn);
  const { servers, loading: serversLoading } = useMcpServers(session.signedIn);
  const { branding } = useClientConfig();

  const [statFilter, setStatFilter] = useState<StatFilter | null>(null);
  const [taskModalOpen, setTaskModalOpen] = useState(false);
  const [editTask, setEditTask] = useState<Task | null>(null);
  const [logTask, setLogTask] = useState<Task | null>(null);
  // "Run now" (#1019): the task awaiting the kick-off confirm, and the in-flight
  // guard that keeps a double-click from submitting two runs.
  const [runNowTask, setRunNowTask] = useState<Task | null>(null);
  const [runNowBusy, setRunNowBusy] = useState(false);
  const { showToast } = useToast();
  const [sidebarOpen, setSidebarOpen] = useState(false);

  // runNow resubmits the task as a fresh one-off run that starts immediately
  // (POST /tasks/{id}/rerun). The source is untouched — a recurring job keeps
  // its cron and still fires on its next tick — so this is "kick it off now",
  // not "reschedule it".
  const runNow = async (task: Task) => {
    if (runNowBusy) return;
    setRunNowBusy(true);
    try {
      const created = await orchestratorApi.rerunTask(task.id);
      showToast(`Started run ${created.id.slice(0, 8)}… from this task`, "success");
      setRunNowTask(null);
      void dashboard.reload();
    } catch (err) {
      showToast(
        `Run now failed: ${err instanceof Error ? err.message : "unknown error"}`,
        "error",
      );
    } finally {
      setRunNowBusy(false);
    }
  };
  // Desktop rail collapse + ≤900px auto-collapse/overlay (shared shell).
  const railCollapse = useRailCollapse();
  // Top-level dashboard tab (#274): defaults to Recent Tasks (the existing
  // dashboard shape) unless a ?tab= deep link picks another view.
  const [tab, setTab] = useState<DashTab>(initialDashboardTab);
  const tabsRef = useRef<HTMLDivElement | null>(null);
  // switchTab pins the tab row to the top of the scroll container when the
  // user had scrolled: the new tab then starts at its beginning instead of
  // the scroller clamping to the page top when the outgoing (taller) content
  // unmounts — the "jumps to the top" bug on phones.
  const switchTab = (next: typeof tab) => {
    setTab(next);
    // Pin after React commits the swapped panel (rAF): pinning against the
    // OLD layout would be undone when the taller outgoing content unmounts
    // and the scroller clamps.
    requestAnimationFrame(() => {
      const bar = tabsRef.current;
      const scroller = bar?.closest(".overflow-y-auto");
      if (!bar || !(scroller instanceof HTMLElement)) return;
      if (scroller.scrollTop <= 1) return; // already at the top — don't move
      const delta = bar.getBoundingClientRect().top - scroller.getBoundingClientRect().top;
      scroller.scrollTop = Math.max(0, scroller.scrollTop + delta - 8);
    });
  };

  // #458 symptom 2: the SLA tab + panel are admin-only. role may be absent (an
  // admin-API-key principal carries no role) — treat absent as non-admin for
  // gating, so a non-admin can never reach the SLA report even via stale state.
  const isAdmin = session.role === "admin";

  const applyStatFilter = (filter: StatFilter) => {
    if (statFilter === filter) {
      setStatFilter(null);
      dashboard.clearFilters();
      return;
    }
    setStatFilter(filter);
    switch (filter) {
      case "tasks-pending":
        dashboard.setFilters({ status: "pending", completedToday: false, completedStatus: "" });
        break;
      case "tasks-running":
        dashboard.setFilters({ status: "running", completedToday: false, completedStatus: "" });
        break;
      case "tasks-completed-today":
        dashboard.setFilters({ status: "", completedToday: true, completedStatus: "success" });
        break;
      case "tasks-failed-today":
        dashboard.setFilters({ status: "", completedToday: true, completedStatus: "error" });
        break;
    }
  };

  // Initial probe pending — keep the bare loading card (no rail yet).
  if (!session.ready) {
    return (
      <div className="container">
        <div className="loading" data-testid="orchestrator-loading">
          <p>Loading…</p>
        </div>
      </div>
    );
  }

  // Probe failed with a non-auth verdict (5xx/network): the backend — or the
  // chat DB behind its fail-closed session-epoch check — is down, and the
  // visitor's session may be perfectly valid. Rendering the login card here
  // would invite credentials mid-incident, so show a retry notice instead —
  // the orchestrator's analogue of chat's backendUnreachable card
  // (chat-experience.tsx / bootstrapFailure.ts).
  if (session.unreachable) {
    return (
      <div className="container">
        <OrchestratorSlimHeader />
        <div className="auth-section" role="region" aria-label="Server unreachable">
          <div className="auth-fields stack-form">
            <h2>Can&apos;t reach the server</h2>
            <p className="caption" data-testid="orchestrator-unreachable">
              The Operations Center backend didn&apos;t answer — it may be restarting. Your
              sign-in state is untouched; try again in a moment.
            </p>
            <button type="button" className="btn btn-primary" onClick={() => window.location.reload()}>
              Retry
            </button>
          </div>
        </div>
      </div>
    );
  }

  // Signed out — slim top bar (theme + cross-link) above the login card; no rail.
  // #458: a chat-signed-in visitor whose identity isn't provisioned here
  // (/me → 403 not_a_member, session.noAccess) gets a dead-end card. The moc
  // username/password form that used to sit under that notice is gone — cookie
  // / OIDC is the only operator path. A genuinely signed-out visitor (401)
  // gets the Elcano-email (or Chat) handoff.
  if (!session.signedIn) {
    return (
      <div className="container">
        <OrchestratorSlimHeader />
        {session.noAccess ? (
          <div className="auth-section" role="region" aria-label="No access">
            <div className="auth-fields stack-form">
              <h2>No access</h2>
              <p className="caption" data-testid="orchestrator-no-access">
                You&apos;re signed in, but that identity isn&apos;t provisioned for the
                Operations Center. Ask an administrator to provision your account.
              </p>
            </div>
          </div>
        ) : (
          <OrchestratorLogin magicLinkLoginEnabled={magicLinkLoginEnabled} />
        )}
      </div>
    );
  }

  // Signed in — the unified two-column shell: rail + main dashboard.
  return (
    <div
      // Transparent shell: the app-wide --gradient-bg on <body> (globals.css)
      // is the one page background shared with the chat view.
      className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]"
    >
      <NavRail
        activeView="orchestrator"
        brandName={branding.app_name}
        brandLogoSrc={branding.logo_url || undefined}
        opsCount={dashboard.stats?.running_tasks}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={railCollapse}
        account={{
          email: session.username ?? "",
          onSignOut: () => void session.logout(),
        }}
      >
        <div className={railCollapse.collapsed ? "sm:flex sm:justify-center" : ""}>
          <button
            type="button"
            data-testid="new-task-btn"
            className={[
              "flex w-full items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-status-success-border)] hover:bg-[var(--color-status-success-bg)]",
              railCollapse.collapsed ? "sm:size-10 sm:w-10 sm:gap-0 sm:p-0" : "",
            ].join(" ")}
            data-tip={railCollapse.collapsed ? "New task" : undefined}
            onClick={() => setTaskModalOpen(true)}
          >
            <Icon name="plus" className="size-4" />
            <span className={railCollapse.collapsed ? "sm:hidden" : ""}>New task</span>
          </button>
        </div>
      </NavRail>

      <main className="flex min-h-0 flex-col overflow-hidden">
        <PageTopBar
          title="Operations Center"
          onMenu={() => setSidebarOpen(true)}
          actions={
            /* Mobile-only New task: on phones the rail's button is inside the
               off-canvas drawer, so surface the primary action in the bar. */
            <button
              type="button"
              className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:hidden"
              data-testid="new-task-btn-topbar"
              onClick={() => setTaskModalOpen(true)}
            >
              <Icon name="plus" className="size-4" />
              New task
            </button>
          }
        />

        <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="container">
            <div className="dashboard-content visible" data-testid="orchestrator-dashboard">
              <div className="dashboard-status-row">
                <ServerClock />
              </div>
              <StatsGrid stats={dashboard.stats} activeFilter={statFilter} onFilter={applyStatFilter} />

              <div
                className="dashboard-tabs"
                role="tablist"
                aria-label="Operations Center view"
                ref={tabsRef}
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={tab === "tasks"}
                  className={`tab-btn${tab === "tasks" ? " tab-btn-active" : ""}`}
                  onClick={() => switchTab("tasks")}
                >
                  Recent Tasks
                </button>
                <button
                  type="button"
                  role="tab"
                  aria-selected={tab === "upcoming"}
                  className={`tab-btn${tab === "upcoming" ? " tab-btn-active" : ""}`}
                  onClick={() => switchTab("upcoming")}
                >
                  Upcoming
                </button>
                {/* #458 symptom 2: only admins see the SLA tab. The render guard
                    below mirrors this — a non-admin holding a stale tab === "sla"
                    still falls back to the tasks view. */}
                <button
                  type="button"
                  role="tab"
                  aria-selected={tab === "datasets"}
                  className={`tab-btn${tab === "datasets" ? " tab-btn-active" : ""}`}
                  onClick={() => switchTab("datasets")}
                >
                  Datasets
                </button>
                {isAdmin ? (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={tab === "sla"}
                    className={`tab-btn${tab === "sla" ? " tab-btn-active" : ""}`}
                    onClick={() => switchTab("sla")}
                  >
                    SLA
                  </button>
                ) : null}
                {/* Usage/spend analytics (#601) is admin-only like SLA: the
                    report is global across all principals. */}
                {isAdmin ? (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={tab === "usage"}
                    className={`tab-btn${tab === "usage" ? " tab-btn-active" : ""}`}
                    onClick={() => switchTab("usage")}
                  >
                    Usage
                  </button>
                ) : null}
                {/* The exec adoption audit is admin-only for the same reason
                    as Usage: it is global across all users' activity. */}
                {isAdmin ? (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={tab === "adoption"}
                    className={`tab-btn${tab === "adoption" ? " tab-btn-active" : ""}`}
                    onClick={() => switchTab("adoption")}
                  >
                    Adoption
                  </button>
                ) : null}
              </div>

              {/* The tab-panel wrapper keeps a floor under the content: when a
                  tab switches, the incoming panel briefly renders a tiny
                  loading state, and without the floor the page's height
                  collapses under the scroll position — the browser clamps to
                  the top, which reads as a jarring jump on phones. */}
              <div className="dashboard-tab-panel">
              {tab === "datasets" ? (
                <DatasetsPanel />
              ) : tab === "upcoming" ? (
                <UpcomingPanel />
              ) : tab === "sla" && isAdmin ? (
                <SLAReportPanel />
              ) : tab === "usage" && isAdmin ? (
                <UsagePanel />
              ) : tab === "adoption" && isAdmin ? (
                <AdoptionPanel />
              ) : (
                <>
                  <SleepingTasks onOpen={setLogTask} />
                  <TasksTable
                  tasks={dashboard.tasks}
                  total={dashboard.total}
                  page={dashboard.page}
                  pageSize={dashboard.pageSize}
                  filters={dashboard.filters}
                  onFilters={dashboard.setFilters}
                  onPage={dashboard.setPage}
                  onPageSize={dashboard.setPageSize}
                  onOpenLogs={setLogTask}
                  onEdit={setEditTask}
                  onRunNow={setRunNowTask}
                />
                </>
              )}
              </div>

              <p className="refresh-note">
                Auto-refresh every {dashboard.refreshSeconds} seconds
              </p>
            </div>
          </div>
        </div>
      </main>

      <TaskCreateModal
        key={editTask ? `edit-${editTask.id}` : "create"}
        open={taskModalOpen || !!editTask}
        servers={servers}
        serversLoading={serversLoading}
        onClose={() => {
          setTaskModalOpen(false);
          setEditTask(null);
        }}
        onCreated={() => void dashboard.reload()}
        editTask={editTask}
        onUpdated={() => void dashboard.reload()}
      />
      {/* Run-now confirm: the row action sits next to a click target that
          opens the log viewer, and a run costs real model spend — so the
          kick-off is confirmed, and the copy states plainly that the source's
          own schedule is left alone. */}
      <ConfirmDialog
        open={!!runNowTask}
        title="Run now"
        message={
          runNowTask
            ? `Start a run of "${taskRunLabel(runNowTask)}" right now?${
                runNowTask.recurrence
                  ? " This is a one-off copy — the task keeps its schedule and still runs at its next scheduled time."
                  : runNowTask.scheduled_for
                    ? " This is a one-off copy — the task stays scheduled and will still run at its scheduled time."
                    : ""
              }`
            : ""
        }
        confirmLabel={runNowBusy ? "Starting…" : "Run now"}
        onConfirm={() => {
          if (runNowTask) void runNow(runNowTask);
        }}
        onCancel={() => {
          if (!runNowBusy) setRunNowTask(null);
        }}
      />
      <LogViewer
        task={logTask}
        onClose={() => setLogTask(null)}
        canStop={isAdmin || (!!session.username && logTask?.created_by_username === session.username)}
        onResubmitted={() => void dashboard.reload()}
        onEdit={(t) => {
          setLogTask(null);
          setEditTask(t);
        }}
        onSelectTask={setLogTask}
      />
    </div>
  );
}

export function OrchestratorClient({ magicLinkLoginEnabled }: { magicLinkLoginEnabled: boolean }) {
  return (
    <ToastProvider>
      <OrchestratorInner magicLinkLoginEnabled={magicLinkLoginEnabled} />
    </ToastProvider>
  );
}

export default OrchestratorClient;
