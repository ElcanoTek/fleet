import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { StatsGrid } from "./StatsGrid";

// Agent-pool cards (#TBD): display-only cards fed by GET /stats
// active_agents/agent_slots. Absent fields (older server) = no cards, so the
// original four-card layout is untouched.

afterEach(() => cleanup());

const BASE = {
  pending_tasks: 1,
  running_tasks: 2,
  completed_tasks_today: 3,
  failed_tasks_today: 0,
};

describe("StatsGrid agent-pool cards", () => {
  it("renders Active Agents and Agent Slots when the server reports them", () => {
    render(
      <StatsGrid
        stats={{ ...BASE, active_agents: 3, agent_slots: 8 }}
        activeFilter={null}
        onFilter={vi.fn()}
      />,
    );
    expect(screen.getByTestId("stat-agents")).toHaveTextContent("Active Agents");
    expect(screen.getByTestId("stat-agents")).toHaveTextContent("3");
    expect(screen.getByTestId("stat-slots")).toHaveTextContent("Agent Slots");
    expect(screen.getByTestId("stat-slots")).toHaveTextContent("8");
    // display-only: not buttons, no filter wiring
    expect(screen.getByTestId("stat-agents").tagName).toBe("DIV");
  });

  it("shows a zero (not a blank) and drops the pulse when no agents run", () => {
    render(
      <StatsGrid
        stats={{ ...BASE, active_agents: 0, agent_slots: 8 }}
        activeFilter={null}
        onFilter={vi.fn()}
      />,
    );
    const card = screen.getByTestId("stat-agents");
    expect(card).toHaveTextContent("0");
    expect(card.className).not.toContain("live");
  });

  it("renders no agent cards when the server omits the fields", () => {
    render(<StatsGrid stats={BASE} activeFilter={null} onFilter={vi.fn()} />);
    expect(screen.queryByTestId("stat-agents")).toBeNull();
    expect(screen.queryByTestId("stat-slots")).toBeNull();
    expect(document.querySelector(".stats-bar")?.className).not.toContain("stats-bar-agents");
  });
});
