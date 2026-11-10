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

    expect(
      formatScheduleCadence({
        id: "3",
        conversation_id: "c1",
        kind: "patrol",
        title: "Weekday Digest",
        prompt: "check inbox",
        cron: "0 9 * * 1-5",
        timezone: "America/New_York",
        status: "active",
        run_count: 4,
        next_run_at: 2000,
        created_at: 1000,
        updated_at: 1000,
      }),
    ).toBe("Weekdays at 09:00");
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
    const autoApprove = screen.getByRole("switch", { name: "Auto-approve" });
    expect(autoApprove).toHaveAttribute("aria-checked", "true");
    fireEvent.click(autoApprove);
    expect(onToggleAutoApprove).toHaveBeenCalledTimes(1);

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

  it("shows auto-approve as a switch that is off, with a fixed label", () => {
    render(
      <PatrolBanner
        schedules={[sampleSchedule]}
        isAutoApprove={false}
        isStreaming={false}
        onToggleAutoApprove={vi.fn()}
        onRunNow={vi.fn()}
        onTogglePause={vi.fn()}
        onCancel={vi.fn()}
      />,
    );

    // The label never reads "Auto-approve off": that read as a status, not
    // as the control that turns it on. The state lives in aria-checked.
    const autoApprove = screen.getByRole("switch", { name: "Auto-approve" });
    expect(autoApprove).toHaveAttribute("aria-checked", "false");
    expect(screen.queryByText(/Auto-approve off/)).not.toBeInTheDocument();
    expect(autoApprove).toHaveAttribute("title", expect.stringContaining("Click to turn it on"));
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

    const popover = screen.getByTestId("patrol-popover");
    expect(popover).toBeInTheDocument();
    expect(popover).toHaveClass("picker-modal");
    const oneHourBtn = screen.getByRole("button", { name: "Every 1h" });
    expect(oneHourBtn).toHaveClass("pill-btn");
    fireEvent.click(oneHourBtn);
    expect(oneHourBtn).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "Start patrol" }));

    await waitFor(() => expect(onStartPatrol).toHaveBeenCalledTimes(1));
    expect(onStartPatrol.mock.calls[0][0]).toMatchObject({
      title: "Recurring Patrol",
      prompt: "Check Deal 123 pacing and update the dashboard.",
      cron: "0 * * * *",
      autoApprove: true,
    });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("lets users customize a recurring schedule or one-time timer without typing raw cron", async () => {
    const onStartPatrol = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();

    render(
      <PatrolPopover
        initialPrompt="Summarize unread emails."
        onStartPatrol={onStartPatrol}
        onClose={onClose}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Custom" }));
    expect(screen.getByTestId("patrol-custom-builder")).toBeInTheDocument();

    fireEvent.change(
      screen.getByTestId("patrol-popover-schedule-frequency"),
      { target: { value: "weekdays" } },
    );
    fireEvent.change(
      screen.getByTestId("patrol-popover-schedule-time"),
      { target: { value: "08:30" } },
    );

    fireEvent.click(screen.getByRole("button", { name: "Start patrol" }));
    await waitFor(() => expect(onStartPatrol).toHaveBeenCalledTimes(1));
    expect(onStartPatrol.mock.calls[0][0]).toMatchObject({
      prompt: "Summarize unread emails.",
      cron: "30 8 * * 1-5",
    });
  });

  it("clamps custom timer duration when switching from minutes to hours", async () => {
    const onStartPatrol = vi.fn().mockResolvedValue(undefined);
    const onClose = vi.fn();

    render(
      <PatrolPopover
        initialPrompt="Follow up once."
        onStartPatrol={onStartPatrol}
        onClose={onClose}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Custom" }));
    fireEvent.click(screen.getByRole("button", { name: "One-time timer" }));

    fireEvent.change(screen.getByLabelText("Timer duration"), {
      target: { value: "1440" },
    });
    fireEvent.change(screen.getByLabelText("Timer unit"), {
      target: { value: "hours" },
    });

    fireEvent.click(screen.getByRole("button", { name: "Start timer" }));
    await waitFor(() => expect(onStartPatrol).toHaveBeenCalledTimes(1));
    expect(onStartPatrol.mock.calls[0][0]).toMatchObject({
      durationSeconds: 168 * 3600,
    });
  });
});
