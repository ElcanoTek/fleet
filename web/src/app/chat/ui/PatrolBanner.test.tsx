import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import {
  formatScheduleCadence,
  PatrolBanner,
  PatrolPopover,
} from "./PatrolBanner";
import type { ChatSchedule } from "./history";

describe("formatScheduleCadence", () => {
  it("formats recurring cron cadences and one-shot timers", () => {
    expect(
      formatScheduleCadence({
        id: "1",
        conversation_id: "c1",
        kind: "patrol",
        title: "Hourly",
        prompt: "check",
        cron: "0 * * * *",
        timezone: "UTC",
        status: "active",
        run_count: 2,
        next_run_at: 2000,
        created_at: 1000,
        updated_at: 1000,
      }),
    ).toBe("Every hour");

    expect(
      formatScheduleCadence({
        id: "2",
        conversation_id: "c1",
        kind: "timer",
        title: "Timer",
        prompt: "check",
        timezone: "UTC",
        duration_seconds: 1800,
        status: "active",
        run_count: 0,
        next_run_at: 2000,
        created_at: 1000,
        updated_at: 1000,
      }),
    ).toBe("One-shot (30m)");
  });
});

describe("PatrolBanner", () => {
  const sampleSchedule: ChatSchedule = {
    id: "sched-1",
    conversation_id: "conv-1",
    kind: "patrol",
    title: "Pacing & Margin Patrol",
    prompt: "Check margin and pacing every 2 hours.",
    cron: "0 */2 * * *",
    timezone: "America/New_York",
    status: "active",
    run_count: 3,
    next_run_at: Math.floor(Date.now() / 1000) + 3600,
    created_at: Math.floor(Date.now() / 1000) - 7200,
    updated_at: Math.floor(Date.now() / 1000) - 60,
  };

  it("renders active patrol summary and controls", async () => {
    const onRunNow = vi.fn();
    const onTogglePause = vi.fn();
    const onCancel = vi.fn();
    const onToggleAutoApprove = vi.fn();

    render(
      <PatrolBanner
        schedules={[sampleSchedule]}
        isAutoApprove={true}
        isStreaming={false}
        onToggleAutoApprove={onToggleAutoApprove}
        onRunNow={onRunNow}
        onTogglePause={onTogglePause}
        onCancel={onCancel}
      />,
    );

    expect(screen.getByTestId("patrol-banner")).toBeInTheDocument();
    expect(screen.getByText("Pacing & Margin Patrol")).toBeInTheDocument();
    expect(screen.getByText("Auto-approve on")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Run now/i }));
    await waitFor(() => expect(onRunNow).toHaveBeenCalledWith("sched-1"));

    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await waitFor(() =>
      expect(onTogglePause).toHaveBeenCalledWith("sched-1", "active"),
    );

    fireEvent.click(
      screen.getByRole("button", { name: "Stop Pacing & Margin Patrol" }),
    );
    await waitFor(() => expect(onCancel).toHaveBeenCalledWith("sched-1"));
  });
});

describe("PatrolPopover", () => {
  it("submits selected preset and prompt to onStartPatrol", async () => {
    const onStartPatrol = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();

    render(
      <PatrolPopover
        initialPrompt="Check Deal 123 pacing and update the dashboard."
        onStartPatrol={onStartPatrol}
        onClose={onClose}
      />,
    );

    expect(screen.getByTestId("patrol-popover")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Every 1h" }));
    fireEvent.click(screen.getByRole("button", { name: "Start patrol" }));

    await waitFor(() => expect(onStartPatrol).toHaveBeenCalledTimes(1));
    expect(onStartPatrol.mock.calls[0][0]).toMatchObject({
      title: "Trading Patrol",
      prompt: "Check Deal 123 pacing and update the dashboard.",
      cron: "0 * * * *",
      autoApprove: true,
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
