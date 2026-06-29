import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { SLAReportPanel } from "./SLAReportPanel";
import type { SLAReport } from "@/app/shared/lib/orchestratorApi";

// SLAReportPanel renders the GET /sla-report aggregation (#274): per-task-name
// p50/p95 actual duration, breach rate, sample count, and a sparkline.

const slaReport = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    slaReport: (...args: unknown[]) => slaReport(...args),
  },
}));

afterEach(() => cleanup());

function mockReport(report: SLAReport) {
  slaReport.mockReset();
  slaReport.mockResolvedValue(report);
}

describe("SLAReportPanel (#274)", () => {
  it("renders the empty state when no SLA-tracked runs exist in the window", async () => {
    mockReport({ period: "last_7_days", window_days: 7, tasks: [] });
    render(<SLAReportPanel />);
    await waitFor(() => {
      expect(screen.getByText(/No SLA-tracked task runs in this window/)).toBeTruthy();
    });
  });

  it("renders one row per task bucket with the expected/p50/p95/breach fields", async () => {
    mockReport({
      period: "last_7_days",
      window_days: 7,
      tasks: [
        {
          task_name: "daily-report",
          expected_minutes: 15,
          p50_actual_minutes: 12,
          p95_actual_minutes: 28,
          breach_rate_pct: 8.3,
          sample_count: 12,
        },
      ],
    });
    render(<SLAReportPanel />);
    await screen.findByTestId("sla-report-row");
    expect(screen.getByText("daily-report")).toBeTruthy();
    expect(screen.getByText("15m")).toBeTruthy();
    expect(screen.getByText("12.0m")).toBeTruthy();
    expect(screen.getByText("28.0m")).toBeTruthy();
    expect(screen.getByText("8.3%")).toBeTruthy();
    expect(screen.getByText("12")).toBeTruthy();
  });

  it("tags a high-breach-rate row with the fail badge tone", async () => {
    mockReport({
      period: "last_7_days",
      window_days: 7,
      tasks: [
        {
          task_name: "runaway",
          expected_minutes: 5,
          p50_actual_minutes: 18,
          p95_actual_minutes: 40,
          breach_rate_pct: 75,
          sample_count: 4,
        },
      ],
    });
    render(<SLAReportPanel />);
    await screen.findByTestId("sla-report-row");
    const badge = screen.getByText("75.0%");
    expect(badge.className).toContain("sla-badge-fail");
  });
});
