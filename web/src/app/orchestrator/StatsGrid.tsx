"use client";

import type { DashboardStats } from "@/app/shared/lib/orchestratorApi";

// StatsGrid — the task-metric cards. Renders the design handoff's segmented
// stats bar (#169): four cards — Pending / Running / Completed Today / Failed
// Today — each with a colored dot, an uppercase title, and a large value. The
// worker-node "agents" counters were removed in #459, so these four task
// counters are the whole live dashboard signal. Each card stays click-to-filter
// (applies the matching Recent Tasks filter via the parent's onFilter), which is
// live functionality the static design mock doesn't show.

export type StatFilter =
  | "tasks-pending"
  | "tasks-running"
  | "tasks-completed-today"
  | "tasks-failed-today";

export type StatsGridProps = {
  stats: DashboardStats | null;
  activeFilter: StatFilter | null;
  onFilter: (filter: StatFilter) => void;
};

const CARDS: Array<{
  filter: StatFilter;
  label: string;
  key: keyof DashboardStats;
  // tone drives the .stat-dot color and the card's stat-tone-* accent bar +
  // value color (mapped to semantic tokens in CSS).
  tone: "pending" | "running" | "success" | "error";
  // live = the in-flight "Running" metric; its dot pulses (reduced-motion safe).
  live?: boolean;
}> = [
  { filter: "tasks-pending", label: "Pending Tasks", key: "pending_tasks", tone: "pending" },
  { filter: "tasks-running", label: "Running Tasks", key: "running_tasks", tone: "running", live: true },
  { filter: "tasks-completed-today", label: "Completed Today", key: "completed_tasks_today", tone: "success" },
  { filter: "tasks-failed-today", label: "Failed Today", key: "failed_tasks_today", tone: "error" },
];

export function StatsGrid({ stats, activeFilter, onFilter }: StatsGridProps) {
  // Agent-pool cards lead the row (slots, then active), display-only (no task
  // filter maps to them), and appear only when the server reports the pool
  // (older servers omit the fields).
  const agentCards: Array<{ label: string; value: number; tone: string; live?: boolean }> = [];
  if (typeof stats?.agent_slots === "number") {
    agentCards.push({ label: "Agent Slots", value: stats.agent_slots, tone: "slots" });
  }
  if (typeof stats?.active_agents === "number") {
    agentCards.push({
      label: "Active Agents",
      value: stats.active_agents,
      tone: "agents",
      live: stats.active_agents > 0,
    });
  }
  return (
    <div
      className={`stats-bar${agentCards.length > 0 ? " stats-bar-agents" : ""}`}
      role="region"
      aria-label="Dashboard statistics"
    >
      {agentCards.map((card) => (
        <div
          key={card.label}
          className={`stat-card stat-card-static stat-tone-${card.tone}${card.live ? " live" : ""}`}
          data-testid={`stat-${card.tone}`}
          aria-label={card.label}
        >
          <span className="stat-card-head">
            <span className={`stat-dot ${card.tone}`} aria-hidden="true" />
            <span className="stat-card-title">{card.label}</span>
          </span>
          <span className="stat-card-val" aria-live="polite">
            {card.value}
          </span>
        </div>
      ))}
      {CARDS.map((card) => {
        const value = stats ? stats[card.key] : undefined;
        const isActive = activeFilter === card.filter;
        return (
          <button
            key={card.filter}
            type="button"
            className={`stat-card stat-tone-${card.tone}${card.live ? " live" : ""}${isActive ? " active" : ""}`}
            data-filter={card.filter}
            aria-label={card.label}
            aria-pressed={isActive}
            onClick={() => onFilter(card.filter)}
          >
            <span className="stat-card-head">
              <span className={`stat-dot ${card.tone}`} aria-hidden="true" />
              <span className="stat-card-title">{card.label}</span>
            </span>
            <span className="stat-card-val" aria-live="polite">
              {value ?? "-"}
            </span>
          </button>
        );
      })}
    </div>
  );
}

export default StatsGrid;
