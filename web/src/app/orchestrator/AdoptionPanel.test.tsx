import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
import { AdoptionPanel } from "./AdoptionPanel";
import type { AdoptionReport, AdoptionUser } from "@/app/shared/lib/orchestratorApi";

// AdoptionPanel renders the GET /admin/usage/adoption read model: KPI tiles
// with previous-window deltas, the two daily small multiples, the token-first
// leaderboard with sparklines + engagement tiers, and the inactive-seat
// roster. Honest-scope notes render verbatim.

const adoption = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    adoption: (...args: unknown[]) => adoption(...args),
  },
}));

afterEach(() => cleanup());

const DAYS = ["2026-06-01", "2026-06-02", "2026-06-03"];

function user(overrides: Partial<AdoptionUser>): AdoptionUser {
  return {
    user: "",
    cost_usd: 0,
    task_cost_usd: 0,
    chat_cost_usd: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    cached_tokens: 0,
    task_iterations: 0,
    chat_turns: 0,
    active_days: 0,
    prev_cost_usd: 0,
    prev_tokens: 0,
    daily_tokens: [0, 0, 0],
    ...overrides,
  };
}

function report(overrides: Partial<AdoptionReport>): AdoptionReport {
  return {
    from: "2026-06-01T00:00:00Z",
    to: "2026-06-04T00:00:00Z",
    prev_from: "2026-05-29T00:00:00Z",
    days: DAYS,
    users: [],
    inactive_users: [],
    daily: DAYS.map((day) => ({ day, cost_usd: 0, tokens: 0, actions: 0, active_users: 0 })),
    totals: {
      active_users: 0,
      prev_active_users: 0,
      new_active_users: 0,
      registered_users: 0,
      cost_usd: 0,
      prev_cost_usd: 0,
      tokens: 0,
      prev_tokens: 0,
      cached_tokens: 0,
      chat_turns: 0,
      task_iterations: 0,
    },
    sources: ["tasks", "chat", "accounts"],
    note: "Token volume measures how much someone uses the agents — an adoption signal, not a performance grade.",
    ...overrides,
  };
}

function mockReport(r: AdoptionReport) {
  adoption.mockReset();
  adoption.mockResolvedValue(r);
}

describe("AdoptionPanel", () => {
  it("renders the empty state when nothing was metered in the window", async () => {
    mockReport(report({}));
    render(<AdoptionPanel />);
    await waitFor(() => {
      expect(screen.getByText(/No recorded activity in this window/)).toBeTruthy();
    });
    // The honest-scope note renders even with no activity.
    expect(screen.getByTestId("adoption-note").textContent).toMatch(/adoption signal/);
  });

  it("renders KPI tiles with deltas, trends, leaderboard, and tiers", async () => {
    mockReport(
      report({
        users: [
          user({
            user: "alice@example.com",
            cost_usd: 3.5,
            prompt_tokens: 1200,
            completion_tokens: 300,
            chat_turns: 4,
            task_iterations: 2,
            active_days: 3,
            last_active: "2026-06-03",
            prev_tokens: 1000,
            daily_tokens: [500, 500, 500],
          }),
          user({
            user: "bob@example.com",
            cost_usd: 0.5,
            prompt_tokens: 100,
            completion_tokens: 20,
            chat_turns: 1,
            active_days: 1,
            last_active: "2026-06-01",
            daily_tokens: [120, 0, 0],
          }),
        ],
        daily: [
          { day: "2026-06-01", cost_usd: 1, tokens: 620, actions: 3, active_users: 2 },
          { day: "2026-06-02", cost_usd: 1.5, tokens: 500, actions: 2, active_users: 1 },
          { day: "2026-06-03", cost_usd: 1.5, tokens: 500, actions: 2, active_users: 1 },
        ],
        totals: {
          active_users: 2,
          prev_active_users: 1,
          new_active_users: 1,
          registered_users: 4,
          cost_usd: 4.0,
          prev_cost_usd: 2.0,
          tokens: 1620,
          prev_tokens: 1000,
          cached_tokens: 100,
          chat_turns: 5,
          task_iterations: 2,
        },
        inactive_users: [
          { email: "carol@example.com", created_at: "2026-03-01T00:00:00Z" },
          { email: "dave@example.com", created_at: "2026-05-01T00:00:00Z" },
        ],
      }),
    );
    render(<AdoptionPanel />);
    await screen.findByTestId("adoption-totals");

    // Headcount tile: active users with the seat denominator + adoption rate.
    expect(screen.getByText(/of 4 seats · 50% adoption/)).toBeTruthy();
    // Deltas: active users 2 vs 1 and spend $4 vs $2 are both ▲ 100%; the
    // tokens tile reads 1620 vs 1000 = ▲ 62%.
    expect(screen.getAllByText(/▲ 100%/)).toHaveLength(2);
    expect(screen.getAllByText(/▲ 62%/).length).toBeGreaterThan(0);

    // Two small multiples, never a dual-axis chart.
    expect(screen.getByRole("img", { name: /Tokens per day, 3 days/ })).toBeTruthy();
    expect(screen.getByRole("img", { name: /Active users per day, 3 days/ })).toBeTruthy();

    // Leaderboard: token order, active-day ratio, engagement tiers.
    const rows = screen.getAllByTestId("adoption-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("alice@example.com");
    expect(rows[0].textContent).toContain("3/3");
    expect(rows[0].textContent).toContain("power");
    // bob was active 1 of 3 days (33% ≥ 20%) → regular.
    expect(rows[1].textContent).toContain("regular");
    // bob has no previous-window baseline → the "new" chip.
    expect(rows[1].textContent).toContain("new");

    // Inactive seats listed by email.
    expect(screen.getByTestId("adoption-inactive").textContent).toContain("carol@example.com");
    expect(screen.getByText("Not yet active (2)")).toBeTruthy();
  });

  it("hides the seat roster when the accounts source is not wired", async () => {
    mockReport(report({ sources: ["tasks", "chat"] }));
    render(<AdoptionPanel />);
    await screen.findByTestId("adoption-totals");
    expect(screen.queryByTestId("adoption-inactive")).toBeNull();
    expect(screen.getByText(/seat roster unavailable/)).toBeTruthy();
  });

  it("re-queries when the range changes", async () => {
    mockReport(report({}));
    render(<AdoptionPanel />);
    await waitFor(() => expect(adoption).toHaveBeenCalled());
    expect(adoption).toHaveBeenLastCalledWith(
      expect.objectContaining({ from: expect.any(String) }),
    );
    fireEvent.change(screen.getByLabelText("Adoption window in days"), {
      target: { value: "90" },
    });
    await waitFor(() => expect(adoption).toHaveBeenCalledTimes(2));
  });

  it("surfaces a fetch failure", async () => {
    adoption.mockReset();
    adoption.mockRejectedValue(new Error("forbidden"));
    render(<AdoptionPanel />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load adoption report/)).toBeTruthy();
    });
  });

  it("Download CSV navigates to the CSV endpoint with the current window", async () => {
    mockReport(report({}));
    const original = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { href: "" } as unknown as Location,
    });
    try {
      render(<AdoptionPanel />);
      const btn = await screen.findByTestId("adoption-download-csv");
      fireEvent.click(btn);
      expect(window.location.href).toContain("/api/orchestrator/admin/usage/adoption?");
      expect(window.location.href).toContain("format=csv");
      expect(window.location.href).toMatch(/from=\d{4}-\d{2}-\d{2}/);
    } finally {
      Object.defineProperty(window, "location", {
        configurable: true,
        writable: true,
        value: original,
      });
    }
  });
});
