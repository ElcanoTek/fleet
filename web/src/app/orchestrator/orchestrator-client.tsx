"use client";

import { useState } from "react";
import type { Task } from "@/app/shared/lib/orchestratorApi";
import { useOrchestratorSession } from "@/app/shared/hooks/useOrchestratorSession";
import { useDashboardData } from "@/app/shared/hooks/useDashboardData";
import { useMcpServers } from "@/app/shared/hooks/useMcpServers";
import { ToastProvider } from "@/app/shared/ui/Toast";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";
import { NavToChat } from "@/app/shared/ui/CrossViewNav";
import { OrchestratorLogin } from "./OrchestratorLogin";
import { StatsGrid, type StatFilter } from "./StatsGrid";
import { NodesTable } from "./NodesTable";
import { TasksTable } from "./TasksTable";
import { TaskCreateModal } from "./TaskCreateModal";
import { LogViewer } from "./LogViewer";
import { ConcurrencyCapSetting } from "./ConcurrencyCapSetting";
import { CredentialAccountAdmin } from "./CredentialAccountAdmin";
import { SLAReportPanel } from "./SLAReportPanel";

// OrchestratorClient — the top-level orchestrator view. Re-port of moc's app.js
// orchestration: gate on login state, render the dashboard, wire stat-card
// filters, the create-task modal (with the MCP picker + concurrency cap), the
// log viewer, and credential-account admin. A header link crosses to /chat
// without re-login (the SAME session cookie gates both views).

function OrchestratorInner({ elcanoLoginEnabled }: { elcanoLoginEnabled: boolean }) {
  const session = useOrchestratorSession();
  const dashboard = useDashboardData(session.signedIn);
  const { servers, reload: reloadServers } = useMcpServers(session.signedIn);

  const [statFilter, setStatFilter] = useState<StatFilter | null>(null);
  const [nodeActiveOnly, setNodeActiveOnly] = useState(false);
  const [taskModalOpen, setTaskModalOpen] = useState(false);
  const [logTask, setLogTask] = useState<Task | null>(null);
  const [adminOpen, setAdminOpen] = useState(false);
  // Top-level dashboard tab (#274): "tasks" is the legacy Recent Tasks view;
  // "sla" swaps in the SLA report panel. Defaults to tasks so the existing
  // dashboard shape is unchanged on load.
  const [tab, setTab] = useState<"tasks" | "sla">("tasks");

  const applyStatFilter = (filter: StatFilter) => {
    if (statFilter === filter) {
      setStatFilter(null);
      setNodeActiveOnly(false);
      dashboard.clearFilters();
      return;
    }
    setStatFilter(filter);
    if (filter.startsWith("nodes-")) {
      setNodeActiveOnly(filter === "nodes-active");
      return;
    }
    setNodeActiveOnly(false);
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

  const header = (
      <header className="header page-header" role="banner">
        <div className="ds-app-header">
          <div className="ds-app-header__brand">
            <div className="ds-app-header__text">
              <p className="ds-app-header__eyebrow">Elcano Internal</p>
              <h1 className="ds-app-header__title">Operations Center</h1>
            </div>
          </div>
          <div className="ds-app-header__actions">
            {/* Shared shell theme switch — same light/dark control the chat
                view and login card render, now available here too. */}
            <ThemeToggle />
            {/* Cross-view navigation — same session gates both, so no re-login. */}
            <NavToChat className="btn btn-ghost" />
            {session.signedIn ? (
              <>
                <button
                  type="button"
                  className="btn btn-primary"
                  data-testid="new-task-btn"
                  onClick={() => setTaskModalOpen(true)}
                >
                  New Task
                </button>
                <button
                  type="button"
                  className="btn btn-ghost"
                  data-testid="admin-toggle"
                  onClick={() => setAdminOpen((o) => !o)}
                >
                  Settings
                </button>
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="logout-btn"
                  onClick={() => void session.logout()}
                >
                  Logout
                </button>
              </>
            ) : null}
          </div>
        </div>
      </header>
  );

  return (
    <div className="container">
      {header}

      {!session.ready ? (
        <div className="loading" data-testid="orchestrator-loading">
          <p>Loading…</p>
        </div>
      ) : !session.signedIn ? (
        <OrchestratorLogin
          elcanoLoginEnabled={elcanoLoginEnabled}
          onLogin={session.login}
          error={session.error}
        />
      ) : (
        <div className="dashboard-content visible" role="main" data-testid="orchestrator-dashboard">
          <StatsGrid stats={dashboard.stats} activeFilter={statFilter} onFilter={applyStatFilter} />

          <div className="section" role="region" aria-labelledby="nodesHeading">
            <div className="section-header">
              <h2 id="nodesHeading">Registered Agents</h2>
            </div>
            <NodesTable nodes={dashboard.nodes} activeOnly={nodeActiveOnly} />
          </div>

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
            <button
              type="button"
              role="tab"
              aria-selected={tab === "sla"}
              className={`tab-btn${tab === "sla" ? " tab-btn-active" : ""}`}
              onClick={() => setTab("sla")}
            >
              SLA
            </button>
          </div>

          {tab === "tasks" ? (
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
          ) : (
            <SLAReportPanel />
          )}

          {adminOpen ? (
            <div className="section" role="region" aria-label="Settings" data-testid="settings-section">
              <div className="section-header">
                <h2>Settings</h2>
              </div>
              <ConcurrencyCapSetting />
              <CredentialAccountAdmin servers={servers} onChanged={reloadServers} />
            </div>
          ) : null}

          <p className="refresh-note">Auto-refresh every 30 seconds</p>
        </div>
      )}

      <TaskCreateModal
        open={taskModalOpen}
        servers={servers}
        onClose={() => setTaskModalOpen(false)}
        onCreated={() => void dashboard.reload()}
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
