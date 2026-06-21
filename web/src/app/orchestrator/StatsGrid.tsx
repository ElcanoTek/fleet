"use client";

import type { DashboardStats } from "@/app/shared/lib/orchestratorApi";

// StatsGrid — the six clickable stat cards. React port of moc dashboard.js's
// stat-card grid + applyStatCardFilter. Clicking a card applies the matching
// task/agent filter (handled by the parent via onFilter).

export type StatFilter =
  | "nodes-all"
  | "nodes-active"
  | "tasks-pending"
  | "tasks-running"
  | "tasks-completed-today"
  | "tasks-failed-today";

export type StatsGridProps = {
  stats: DashboardStats | null;
  activeFilter: StatFilter | null;
  onFilter: (filter: StatFilter) => void;
};

const CARDS: Array<{ filter: StatFilter; label: string; key: keyof DashboardStats; tone: string }> = [
  { filter: "nodes-all", label: "Total Agents", key: "total_nodes", tone: "" },
  { filter: "nodes-active", label: "Active Agents", key: "active_nodes", tone: "success" },
  { filter: "tasks-pending", label: "Pending Tasks", key: "pending_tasks", tone: "pending" },
  { filter: "tasks-running", label: "Running Tasks", key: "running_tasks", tone: "running" },
  { filter: "tasks-completed-today", label: "Completed Today", key: "completed_tasks_today", tone: "success" },
  { filter: "tasks-failed-today", label: "Failed Today", key: "failed_tasks_today", tone: "error" },
];

export function StatsGrid({ stats, activeFilter, onFilter }: StatsGridProps) {
  return (
    <div className="stats-grid" role="region" aria-label="Dashboard statistics">
      {CARDS.map((card) => {
        const value = stats ? stats[card.key] : undefined;
        const isActive = activeFilter === card.filter;
        return (
          <button
            key={card.filter}
            type="button"
            className={`stat-card stat-card-clickable ${card.tone}${isActive ? " active" : ""}`}
            data-filter={card.filter}
            aria-label={card.label}
            aria-pressed={isActive}
            onClick={() => onFilter(card.filter)}
          >
            <h3>{card.label}</h3>
            <div className="value" aria-live="polite">
              {value ?? "-"}
            </div>
          </button>
        );
      })}
    </div>
  );
}

export default StatsGrid;
