"use client";

import { useState } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { useOrchestratorSession } from "@/app/shared/hooks/useOrchestratorSession";
import { useDashboardData } from "@/app/shared/hooks/useDashboardData";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { ToastProvider } from "@/app/shared/ui/Toast";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";
import { NavToChat } from "@/app/shared/ui/CrossViewNav";
import { NavRail } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { Icon } from "@/app/shared/ui/Icon";
import { OrchestratorLogin } from "./OrchestratorLogin";
import { StatsGrid, type StatFilter } from "./StatsGrid";
import { TasksTable } from "./TasksTable";
import { TaskCreateModal } from "./TaskCreateModal";
import { LogViewer } from "./LogViewer";
import { SettingsModal } from "./SettingsModal";
import { SLAReportPanel } from "./SLAReportPanel";

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
            <p className="ds-app-header__eyebrow">Elcano Internal</p>
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

function OrchestratorInner({ elcanoLoginEnabled }: { elcanoLoginEnabled: boolean }) {
  const session = useOrchestratorSession();
  const dashboard = useDashboardData(session.signedIn);
  const { servers, reload: reloadServers } = useMcpServers(session.signedIn);
  const { branding } = useClientConfig();

  const [statFilter, setStatFilter] = useState<StatFilter | null>(null);
  const [taskModalOpen, setTaskModalOpen] = useState(false);
  const [logTask, setLogTask] = useState<Task | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // Top-level dashboard tab (#274): "tasks" is the legacy Recent Tasks view;
  // "sla" swaps in the SLA report panel. Defaults to tasks so the existing
  // dashboard shape is unchanged on load.
  const [tab, setTab] = useState<"tasks" | "sla">("tasks");

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

  // Signed out — slim top bar (theme + cross-link) above the login card; no rail.
  // #458 symptom 1: when the visitor IS signed in to chat but that identity
  // isn't provisioned here (/me → 403 not_a_member, session.noAccess), we still
  // render the login card — the username/password (moc) path can admit a
  // provisioned operator even when the cookie identity can't — but with a notice
  // explaining why a login prompt appeared, instead of a bare, confusing form or
  // a dead-end card that would strand a valid moc user. A genuinely signed-out
  // visitor (401) gets the plain card with no notice.
  if (!session.signedIn) {
    return (
      <div className="container">
        <OrchestratorSlimHeader />
        <OrchestratorLogin
          elcanoLoginEnabled={elcanoLoginEnabled}
          onLogin={session.login}
          error={session.error}
          notice={
            session.noAccess
              ? "You're signed in, but that identity isn't provisioned for the Operations Center. Sign in with Operations Center credentials below, or ask an administrator to provision your account."
              : undefined
          }
        />
      </div>
    );
  }

  // Signed in — the unified two-column shell: rail + main dashboard.
  return (
    <div
      className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden bg-[var(--gradient-bg-ops-console)] text-[var(--color-text-primary)] lg:grid-cols-[18rem_minmax(0,1fr)]"
    >
      <NavRail
        activeView="orchestrator"
        brandName={branding.app_name}
        opsCount={dashboard.stats?.running_tasks}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        account={{
          email: session.username ?? "",
          onSignOut: () => void session.logout(),
          onSettings: () => setSettingsOpen(true),
        }}
      >
        <button
          type="button"
          data-testid="new-task-btn"
          className="flex w-full items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]"
          onClick={() => setTaskModalOpen(true)}
        >
          <Icon name="plus" className="size-4" /> New task
        </button>
      </NavRail>

      <main className="flex min-h-0 flex-col overflow-hidden">
        <PageTopBar title="Operations Center" onMenu={() => setSidebarOpen(true)} />

        <div className="min-h-0 flex-1 overflow-y-auto">
          <div className="container">
            <div className="dashboard-content visible" data-testid="orchestrator-dashboard">
              <StatsGrid stats={dashboard.stats} activeFilter={statFilter} onFilter={applyStatFilter} />

              <div className="dashboard-tabs" role="tablist" aria-label="Operations Center view">
                <button
                  type="button"
                  role="tab"
                  aria-selected={tab === "tasks"}
                  className={`tab-btn${tab === "tasks" ? " tab-btn-active" : ""}`}
                  onClick={() => setTab("tasks")}
                >
                  Recent Tasks
                </button>
                {/* #458 symptom 2: only admins see the SLA tab. The render guard
                    below mirrors this — a non-admin holding a stale tab === "sla"
                    still falls back to the tasks view. */}
                {isAdmin ? (
                  <button
                    type="button"
                    role="tab"
                    aria-selected={tab === "sla"}
                    className={`tab-btn${tab === "sla" ? " tab-btn-active" : ""}`}
                    onClick={() => setTab("sla")}
                  >
                    SLA
                  </button>
                ) : null}
              </div>

              {tab === "sla" && isAdmin ? (
                <SLAReportPanel />
              ) : (
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
                />
              )}

              <p className="refresh-note">Auto-refresh every 30 seconds</p>
            </div>
          </div>
        </div>
      </main>

      <TaskCreateModal
        open={taskModalOpen}
        servers={servers}
        onClose={() => setTaskModalOpen(false)}
        onCreated={() => void dashboard.reload()}
      />
      <SettingsModal
        open={settingsOpen}
        servers={servers}
        onClose={() => setSettingsOpen(false)}
        onChanged={reloadServers}
      />
      <LogViewer task={logTask} onClose={() => setLogTask(null)} />
    </div>
  );
}

export function OrchestratorClient({ elcanoLoginEnabled }: { elcanoLoginEnabled: boolean }) {
  return (
    <ToastProvider>
      <OrchestratorInner elcanoLoginEnabled={elcanoLoginEnabled} />
    </ToastProvider>
  );
}

export default OrchestratorClient;
