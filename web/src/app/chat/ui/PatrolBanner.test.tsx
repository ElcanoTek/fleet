import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import {
  formatScheduleCadence,
  formatScheduleMeta,
  PatrolBanner,
} from "./PatrolBanner";
import { editBody, PatrolsDialog } from "./PatrolsDialog";
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

const nowSec = () => Math.floor(Date.now() / 1000);

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
  next_run_at: nowSec() + 3600,
  created_at: nowSec() - 7200,
  updated_at: nowSec() - 60,
};

const timerSchedule: ChatSchedule = {
  ...sampleSchedule,
  id: "sched-2",
  kind: "timer",
  title: "Follow up on deal IDs",
  prompt: "Confirm the deal IDs are live.",
  cron: undefined,
  duration_seconds: 7200,
  run_count: 0,
  max_runs: 1,
};

describe("formatScheduleMeta", () => {
  it("leads with Paused for a paused patrol and drops the countdown", () => {
    const now = nowSec();
    expect(formatScheduleMeta({ ...sampleSchedule, next_run_at: now + 3600 }, now)).toBe(
      "Every 2h · in 1h · Run #3",
    );
    expect(formatScheduleMeta({ ...sampleSchedule, status: "paused" }, now)).toBe(
      "Paused · Every 2h · Run #3",
    );
  });
});

function renderBar(overrides: Partial<Parameters<typeof PatrolBanner>[0]> = {}) {
  const props = {
    schedules: [sampleSchedule],
    approvalMode: "auto" as const,
    isStreaming: false,
    onChangeApprovalMode: vi.fn(),
    onRunNow: vi.fn(),
    onTogglePause: vi.fn(),
    onHide: vi.fn(),
    onReview: vi.fn(),
    ...overrides,
  };
  render(<PatrolBanner {...props} />);
  return props;
}

describe("PatrolBanner", () => {
  it("renders the patrol summary with Run now, Pause and Hide, and no stop control", async () => {
    const props = renderBar();
    expect(screen.getByTestId("patrol-banner")).toBeInTheDocument();
    expect(screen.getByText("Pacing & Margin Patrol")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
    await waitFor(() => expect(props.onRunNow).toHaveBeenCalledWith("sched-1"));
    fireEvent.click(screen.getByRole("button", { name: "Pause" }));
    await waitFor(() => expect(props.onTogglePause).toHaveBeenCalledWith("sched-1", "active"));

    // Ryan (Raptive, 2026-11-17) clicked the bar's "×" to dismiss it and it
    // cancelled his patrol. Nothing on the bar may stop a patrol now.
    expect(screen.queryByRole("button", { name: /stop|cancel|remove/i })).toBeNull();
    const hide = screen.getByRole("button", { name: "Hide patrol bar. The patrol keeps running." });
    fireEvent.click(hide);
    expect(props.onHide).toHaveBeenCalledTimes(1);
  });

  it("disables Run now while the patrol is paused and offers Resume", () => {
    renderBar({ schedules: [{ ...sampleSchedule, status: "paused" }] });
    expect(screen.getByRole("button", { name: "Run now" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Resume" })).toBeEnabled();
  });

  it("shows the approval mode as a dropdown and switches without asking for Manual/Auto", async () => {
    const user = userEvent.setup();
    const props = renderBar({ approvalMode: "auto" });
    const trigger = screen.getByRole("button", { name: "Approval mode: Auto-approve" });
    await user.click(trigger);
    const menu = await screen.findByTestId("patrol-mode-menu-content");
    expect(within(menu).getByRole("menuitemradio", { name: /Auto-approve/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await user.click(within(menu).getByRole("menuitemradio", { name: /Manual/ }));
    expect(props.onChangeApprovalMode).toHaveBeenCalledWith("manual");
  });

  it("asks before switching an armed patrol to Autonomous", async () => {
    const user = userEvent.setup();
    const props = renderBar({ approvalMode: "manual" });
    await user.click(screen.getByRole("button", { name: "Approval mode: Manual" }));
    const menu = await screen.findByTestId("patrol-mode-menu-content");
    await user.click(within(menu).getByRole("menuitemradio", { name: /Autonomous/ }));
    expect(props.onChangeApprovalMode).not.toHaveBeenCalled();
    expect(screen.getByTestId("patrol-mode-confirm")).toBeInTheDocument();

    await user.click(screen.getByRole("menuitem", { name: "Keep current" }));
    expect(screen.queryByTestId("patrol-mode-confirm")).toBeNull();
    expect(props.onChangeApprovalMode).not.toHaveBeenCalled();

    await user.click(within(menu).getByRole("menuitemradio", { name: /Autonomous/ }));
    await user.click(screen.getByTestId("patrol-mode-confirm-autonomous"));
    expect(props.onChangeApprovalMode).toHaveBeenCalledWith("autonomous");
  });

  it("switches a paused patrol to Autonomous without the extra confirm", async () => {
    const user = userEvent.setup();
    const props = renderBar({
      approvalMode: "manual",
      schedules: [{ ...sampleSchedule, status: "paused" }],
    });
    await user.click(screen.getByRole("button", { name: "Approval mode: Manual" }));
    const menu = await screen.findByTestId("patrol-mode-menu-content");
    await user.click(within(menu).getByRole("menuitemradio", { name: /Autonomous/ }));
    expect(props.onChangeApprovalMode).toHaveBeenCalledWith("autonomous");
  });

  it("hides the approval-mode control from someone who cannot change it", () => {
    renderBar({ onChangeApprovalMode: undefined });
    expect(screen.queryByTestId("patrol-mode-menu")).toBeNull();
  });

  it("flags a waiting approval with a Review button", () => {
    const props = renderBar({ pendingApprovals: 1 });
    expect(screen.getByTestId("patrol-bar-attention")).toHaveTextContent(
      "An action is waiting for your approval",
    );
    fireEvent.click(screen.getByRole("button", { name: "Review" }));
    expect(props.onReview).toHaveBeenCalledTimes(1);
  });
});

// The card's expand toggle: the one button on it carrying aria-expanded.
function cardToggle(index: number) {
  const card = screen.getAllByTestId("patrols-card")[index];
  return within(card).getByRole("button", { expanded: false });
}

function renderDialog(overrides: Partial<Parameters<typeof PatrolsDialog>[0]> = {}) {
  const props = {
    schedules: [sampleSchedule, timerSchedule],
    approvalMode: "auto" as const,
    canChangeApprovalMode: true,
    autonomousAccepted: true,
    onRequireAutonomousAccept: vi.fn(),
    onChangeApprovalMode: vi.fn(),
    barShown: true,
    onSetBarShown: vi.fn(),
    onReview: vi.fn(),
    onRunNow: vi.fn(),
    onTogglePause: vi.fn(),
    onRemove: vi.fn().mockResolvedValue(true),
    onSave: vi.fn().mockResolvedValue(true),
    onStart: vi.fn().mockResolvedValue(true),
    onClose: vi.fn(),
    ...overrides,
  };
  render(<PatrolsDialog {...props} />);
  return props;
}

describe("PatrolsDialog", () => {
  it("lists the chat's patrols and toggles the Patrol bar", () => {
    const props = renderDialog({ barShown: false });
    expect(screen.getByRole("dialog", { name: "Patrols & timers" })).toBeInTheDocument();
    expect(screen.getAllByTestId("patrols-card")).toHaveLength(2);
    // A hidden bar is still reachable here: the switch shows it again.
    const toggle = screen.getByRole("switch", { name: "Show bar above composer" });
    expect(toggle).toHaveAttribute("aria-checked", "false");
    fireEvent.click(toggle);
    expect(props.onSetBarShown).toHaveBeenCalledWith(true);
    // With patrols listed, the new-patrol form waits behind its button.
    expect(screen.queryByTestId("patrol-form-new")).toBeNull();
    expect(screen.getByTestId("patrols-add")).toBeInTheDocument();
  });

  it("removes a patrol only on a second, confirming click", async () => {
    const props = renderDialog();
    fireEvent.click(cardToggle(0));
    const remove = screen.getByTestId("patrols-remove");
    expect(remove).toHaveTextContent("Remove patrol");
    fireEvent.click(remove);
    expect(props.onRemove).not.toHaveBeenCalled();
    expect(remove).toHaveTextContent("Confirm remove");
    fireEvent.click(remove);
    await waitFor(() => expect(props.onRemove).toHaveBeenCalledWith("sched-1"));
  });

  it("drops a pending remove confirmation when the card collapses", () => {
    const props = renderDialog();
    const header = cardToggle(0);
    fireEvent.click(header);
    fireEvent.click(screen.getByTestId("patrols-remove"));
    fireEvent.click(header);
    fireEvent.click(header);
    expect(screen.getByTestId("patrols-remove")).toHaveTextContent("Remove patrol");
    expect(props.onRemove).not.toHaveBeenCalled();
  });

  it("calls a timer's stop Cancel timer", () => {
    renderDialog();
    fireEvent.click(cardToggle(1));
    expect(screen.getByTestId("patrols-remove")).toHaveTextContent("Cancel timer");
  });

  it("edits the task and sends only what changed", async () => {
    const props = renderDialog();
    fireEvent.click(cardToggle(0));
    fireEvent.click(screen.getByRole("button", { name: "Edit schedule & task" }));
    const form = screen.getByTestId("patrol-form-edit");
    fireEvent.change(within(form).getByRole("textbox", { name: "What to check & do each run" }), {
      target: { value: "Check margin only." },
    });
    fireEvent.click(within(form).getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(props.onSave).toHaveBeenCalledTimes(1));
    expect(props.onSave).toHaveBeenCalledWith("sched-1", { prompt: "Check margin only." }, "auto");
    await waitFor(() => expect(screen.queryByTestId("patrol-form-edit")).toBeNull());
  });

  it("starts a new patrol from the add form", async () => {
    const props = renderDialog({ initialPrompt: "Check Deal 123 pacing." });
    fireEvent.click(screen.getByTestId("patrols-add"));
    const form = screen.getByTestId("patrol-form-new");
    fireEvent.click(within(form).getByRole("button", { name: "Every 1h" }));
    fireEvent.click(within(form).getByRole("button", { name: "Start patrol" }));
    await waitFor(() => expect(props.onStart).toHaveBeenCalledTimes(1));
    expect(vi.mocked(props.onStart).mock.calls[0][0]).toMatchObject({
      title: "Recurring Patrol",
      prompt: "Check Deal 123 pacing.",
      cron: "0 * * * *",
      autoApprove: true,
      approvalMode: "auto",
      // Adding next to a running patrol must add, never overwrite it.
      allowMultiple: true,
    });
  });

  it("asks before the dialog's mode control switches a running patrol to Autonomous", () => {
    const props = renderDialog();
    fireEvent.click(cardToggle(0));
    fireEvent.click(screen.getByTestId("patrols-mode-autonomous"));
    expect(props.onChangeApprovalMode).not.toHaveBeenCalled();
    const confirm = screen.getByTestId("patrols-autonomous-confirm");
    fireEvent.click(within(confirm).getByRole("button", { name: "Keep current" }));
    expect(props.onChangeApprovalMode).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId("patrols-mode-autonomous"));
    fireEvent.click(screen.getByRole("button", { name: "Switch to Autonomous" }));
    expect(props.onChangeApprovalMode).toHaveBeenCalledWith("autonomous");
  });

  it("asks before the edit form picks Autonomous while a patrol runs", () => {
    renderDialog();
    fireEvent.click(cardToggle(0));
    fireEvent.click(screen.getByRole("button", { name: "Edit schedule & task" }));
    const form = screen.getByTestId("patrol-form-edit");
    fireEvent.click(within(form).getByRole("radio", { name: "Autonomous" }));
    expect(within(form).getByRole("radio", { name: "Autonomous" })).toHaveAttribute("aria-checked", "false");
    fireEvent.click(within(form).getByRole("button", { name: "Switch to Autonomous" }));
    expect(within(form).getByRole("radio", { name: "Autonomous" })).toHaveAttribute("aria-checked", "true");
  });

  it("keeps the remove confirmation armed when the DELETE fails", async () => {
    const props = renderDialog({ onRemove: vi.fn().mockResolvedValue(false) });
    fireEvent.click(cardToggle(0));
    const remove = screen.getByTestId("patrols-remove");
    fireEvent.click(remove);
    fireEvent.click(remove);
    await waitFor(() => expect(props.onRemove).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId("patrols-remove")).toHaveTextContent("Confirm remove");
  });
});

describe("PatrolsDialog in a sealed (lockdown) chat", () => {
  it("manages existing patrols but offers no way to start one", () => {
    renderDialog({ canCreate: false });
    expect(screen.getAllByTestId("patrols-card")).toHaveLength(2);
    expect(screen.queryByTestId("patrols-add")).toBeNull();
    expect(screen.queryByTestId("patrol-form-new")).toBeNull();
    fireEvent.click(cardToggle(0));
    expect(screen.getByTestId("patrols-remove")).toBeInTheDocument();
  });

  it("says so instead of showing the form once none are left", () => {
    renderDialog({ canCreate: false, schedules: [] });
    expect(screen.queryByTestId("patrol-form-new")).toBeNull();
    expect(screen.getByText("No patrols or timers in this chat.")).toBeInTheDocument();
  });
});

describe("PatrolsDialog with no patrols (first patrol)", () => {
  it("opens straight on the new-patrol form, with no Cancel", () => {
    renderDialog({ schedules: [] });
    const form = screen.getByTestId("patrol-form-new");
    expect(form).toBeInTheDocument();
    expect(within(form).queryByRole("button", { name: "Cancel" })).toBeNull();
    expect(screen.queryByTestId("patrols-add")).toBeNull();
  });

  it("builds a custom recurring schedule without typing raw cron", async () => {
    const props = renderDialog({ schedules: [], initialPrompt: "Summarize unread emails." });
    const repeats = screen.getByRole("group", { name: "Repeats" });
    fireEvent.click(within(repeats).getByRole("button", { name: "Custom" }));
    expect(screen.getByTestId("patrol-custom-builder")).toBeInTheDocument();
    fireEvent.change(screen.getByTestId("patrol-popover-schedule-frequency"), {
      target: { value: "weekdays" },
    });
    fireEvent.change(screen.getByTestId("patrol-popover-schedule-time"), {
      target: { value: "08:30" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Start patrol" }));
    await waitFor(() => expect(props.onStart).toHaveBeenCalledTimes(1));
    expect(vi.mocked(props.onStart).mock.calls[0][0]).toMatchObject({
      prompt: "Summarize unread emails.",
      cron: "30 8 * * 1-5",
    });
    // A chat's first patrol is a plain create.
    expect(vi.mocked(props.onStart).mock.calls[0][0].allowMultiple).toBeUndefined();
  });

  it("sets a one-time timer, clamping a custom duration when the unit changes", async () => {
    const props = renderDialog({ schedules: [], initialPrompt: "Follow up once." });
    const once = screen.getByRole("group", { name: "Once (timer)" });
    fireEvent.click(within(once).getByRole("button", { name: "Custom" }));
    fireEvent.change(screen.getByLabelText("Timer duration"), { target: { value: "1440" } });
    fireEvent.change(screen.getByLabelText("Timer unit"), { target: { value: "hours" } });
    fireEvent.click(screen.getByRole("button", { name: "Set timer" }));
    await waitFor(() => expect(props.onStart).toHaveBeenCalledTimes(1));
    expect(vi.mocked(props.onStart).mock.calls[0][0]).toMatchObject({ durationSeconds: 168 * 3600 });
  });

  it("asks for the one-time Autonomous notice before picking Autonomous", () => {
    const props = renderDialog({ schedules: [], autonomousAccepted: false });
    fireEvent.click(screen.getByRole("radio", { name: "Autonomous" }));
    expect(props.onRequireAutonomousAccept).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("radio", { name: "Autonomous" })).toHaveAttribute("aria-checked", "false");
  });
});

describe("editBody", () => {
  it("keeps cron and duration out of a label- or task-only edit", () => {
    expect(
      editBody(sampleSchedule, {
        title: "New name",
        prompt: sampleSchedule.prompt,
        kind: "patrol",
        cron: "0 */2 * * *",
        timezone: "America/New_York",
        approvalMode: "auto",
        scheduleChanged: false,
      }),
    ).toEqual({ title: "New name" });
  });

  it("sends a new cron with its zone, and turns a timer into a patrol", () => {
    expect(
      editBody(timerSchedule, {
        title: timerSchedule.title,
        prompt: timerSchedule.prompt,
        kind: "patrol",
        cron: "0 9 * * *",
        timezone: "UTC",
        approvalMode: "auto",
        scheduleChanged: true,
      }),
    ).toEqual({ cron: "0 9 * * *", timezone: "UTC" });
  });

  it("never re-arms a timer the form cannot show exactly (90s) on a rename", () => {
    expect(
      editBody({ ...timerSchedule, duration_seconds: 90 }, {
        title: "Renamed",
        prompt: timerSchedule.prompt,
        kind: "timer",
        durationSeconds: 120,
        approvalMode: "auto",
        scheduleChanged: false,
      }),
    ).toEqual({ title: "Renamed" });
  });
});
