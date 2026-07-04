import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
import { UsagePanel } from "./UsagePanel";
import type { UsageBucket, UsageReport } from "@/app/shared/lib/orchestratorApi";

// UsagePanel renders the GET /admin/usage read model (#601 part 1): KPI tiles,
// a single-hue bar/column chart, the full table twin, and the honest-scope
// pricing note (#289).

const usage = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    usage: (...args: unknown[]) => usage(...args),
  },
}));

afterEach(() => cleanup());

function bucket(overrides: Partial<UsageBucket>): UsageBucket {
  return {
    key: "",
    cost_usd: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    cached_tokens: 0,
    task_cost_usd: 0,
    chat_cost_usd: 0,
    task_iterations: 0,
    chat_turns: 0,
    ...overrides,
  };
}

function report(overrides: Partial<UsageReport>): UsageReport {
  return {
    group_by: "user",
    from: "2026-06-01T00:00:00Z",
    to: "2026-07-01T00:00:00Z",
    buckets: [],
    totals: bucket({}),
    sources: ["tasks", "chat"],
    note: "Dollar figures cover only runs with model pricing available (#289).",
    ...overrides,
  };
}

function mockReport(r: UsageReport) {
  usage.mockReset();
  usage.mockResolvedValue(r);
}

describe("UsagePanel (#601)", () => {
  it("renders the empty state when nothing was metered in the window", async () => {
    mockReport(report({}));
    render(<UsagePanel />);
    await waitFor(() => {
      expect(screen.getByText(/No recorded usage in this window/)).toBeTruthy();
    });
    // The honest-scope note renders even with no buckets.
    expect(screen.getByTestId("usage-note").textContent).toMatch(/pricing/);
  });

  it("renders totals tiles, per-bucket bars, and the table twin", async () => {
    mockReport(
      report({
        buckets: [
          bucket({
            key: "alice@example.com",
            cost_usd: 3.5,
            task_cost_usd: 2.5,
            chat_cost_usd: 1.0,
            prompt_tokens: 1200,
            completion_tokens: 300,
            cached_tokens: 100,
            task_iterations: 4,
            chat_turns: 2,
          }),
          bucket({ key: "", cost_usd: 1.25, task_cost_usd: 1.25, prompt_tokens: 500, task_iterations: 1 }),
        ],
        totals: bucket({
          cost_usd: 4.75,
          task_cost_usd: 3.75,
          chat_cost_usd: 1.0,
          prompt_tokens: 1700,
          completion_tokens: 300,
          cached_tokens: 100,
          task_iterations: 5,
          chat_turns: 2,
        }),
      }),
    );
    render(<UsagePanel />);
    await screen.findByTestId("usage-totals");
    expect(screen.getByText("$4.75")).toBeTruthy();
    // Token totals sit beside dollars (the #289 honest-scope requirement).
    expect(screen.getByText("1.7K")).toBeTruthy();
    // The empty key is labeled honestly, not hidden.
    expect(screen.getAllByText("(unattributed)").length).toBeGreaterThan(0);
    // Bars + table rows for both buckets.
    expect(screen.getByTestId("usage-bars")).toBeTruthy();
    expect(screen.getAllByTestId("usage-row")).toHaveLength(2);
    // Sources footer says where the numbers came from.
    expect(screen.getByText(/sources: tasks \+ chat/)).toBeTruthy();
  });

  it("re-queries with the selected grouping dimension", async () => {
    mockReport(report({}));
    render(<UsagePanel />);
    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(usage).toHaveBeenLastCalledWith(
      expect.objectContaining({ groupBy: "user", from: expect.any(String) }),
    );

    fireEvent.change(screen.getByLabelText("Usage grouping dimension"), {
      target: { value: "model" },
    });
    await waitFor(() =>
      expect(usage).toHaveBeenLastCalledWith(expect.objectContaining({ groupBy: "model" })),
    );
  });

  it("renders day buckets as a time-series column chart", async () => {
    mockReport(
      report({
        group_by: "day",
        buckets: [
          bucket({ key: "2026-06-01", cost_usd: 1, prompt_tokens: 100 }),
          bucket({ key: "2026-06-02", cost_usd: 2, prompt_tokens: 200 }),
        ],
        totals: bucket({ cost_usd: 3, prompt_tokens: 300 }),
      }),
    );
    render(<UsagePanel />);
    await screen.findByTestId("usage-table");
    expect(screen.queryByTestId("usage-bars")).toBeNull();
    expect(screen.getByRole("img", { name: /Cost per day, 2 buckets/ })).toBeTruthy();
    // The x-axis is labeled with the first/last bucket dates.
    expect(screen.getAllByText("2026-06-01").length).toBeGreaterThan(0);
  });

  it("surfaces a fetch failure", async () => {
    usage.mockReset();
    usage.mockRejectedValue(new Error("forbidden"));
    render(<UsagePanel />);
    await waitFor(() => {
      expect(screen.getByText(/Failed to load usage report/)).toBeTruthy();
    });
  });

  it("Download CSV navigates to the CSV endpoint with the current filters", async () => {
    mockReport(report({ buckets: [bucket({ key: "alice@example.com", cost_usd: 1 })] }));
    // Stub navigation so the click doesn't actually change location.
    const original = window.location;
    // @ts-expect-error redefining for the test
    delete window.location;
    // @ts-expect-error minimal stub
    window.location = { href: "" };
    try {
      render(<UsagePanel />);
      const btn = await screen.findByTestId("usage-download-csv");
      fireEvent.click(btn);
      expect(window.location.href).toContain("/api/orchestrator/admin/usage?");
      expect(window.location.href).toContain("format=csv");
      expect(window.location.href).toContain("group_by=user");
      expect(window.location.href).toMatch(/from=\d{4}-\d{2}-\d{2}/);
    } finally {
      window.location = original;
    }
  });
});
