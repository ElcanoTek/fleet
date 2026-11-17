import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import GenerativeCard, { resetPendingHolds } from "./GenerativeCard";
import { SubmissionBubble } from "./SubmissionBubble";
import { parseCardSpec, parseSubmissionMessage } from "./model";
import { deriveGenUiState, GenUiContext } from "./transcript";

const SAMPLE_SPEC = parseCardSpec(
  JSON.stringify({
    title: "Clone PMP Deal",
    components: [
      {
        type: "text_input",
        id: "deal_name",
        label: "Deal name",
        required: true,
        value: "Q4 Video PMP",
      },
    ],
    actions: [{ id: "clone", label: "Clone deal" }],
  }),
)!;

describe("GenerativeCard Autonomous 10s countdown and pause UI", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-11-17T10:00:00Z"));
    resetPendingHolds();
    window.localStorage.clear();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("renders server deadline countdown, locks action buttons at 0s, and never submits from client", () => {
    const onSubmit = vi.fn();
    const onAutoSubmitDue = vi.fn();
    const deadlineMs = Date.now() + 10_000;

    render(
      <GenerativeCard
        cardId="call_auto_1"
        spec={SAMPLE_SPEC}
        storageScope="conv-123"
        autoSubmit={{
          id: "auto-id-1",
          call_id: "call_auto_1",
          action_id: "clone",
          deadline_ms: deadlineMs,
          remaining_ms: 10_000,
          status: "pending",
        }}
        onSubmit={onSubmit}
        onAutoSubmitDue={onAutoSubmitDue}
      />,
    );

    const countdown = screen.getByTestId("genui-autosubmit-countdown");
    expect(countdown).toHaveTextContent("Auto-submitting in 10s");
    expect(screen.getByTestId("genui-autosubmit-pause")).toHaveTextContent("Pause");
    const submitBtn = screen.getByRole("button", { name: "Clone deal" });
    expect(submitBtn).not.toBeDisabled();

    act(() => {
      vi.advanceTimersByTime(11_000);
    });

    // Single-firer rule: client NEVER invokes onSubmit at t=10s; it locks action buttons and notifies onAutoSubmitDue once.
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onAutoSubmitDue).toHaveBeenCalledTimes(1);
    expect(onAutoSubmitDue).toHaveBeenCalledWith("call_auto_1");
    expect(screen.getByTestId("genui-autosubmit-countdown")).toHaveTextContent("Auto-submitting...");
    expect(screen.queryByTestId("genui-autosubmit-pause")).not.toBeInTheDocument();
    expect(submitBtn).toBeDisabled();
  });

  it("pauses immediately and POSTs /autosubmit/pause when Pause is clicked", async () => {
    const deadlineMs = Date.now() + 8_000;
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          ui_autosubmits: [
            {
              id: "auto-id-2",
              call_id: "call_auto_2",
              action_id: "clone",
              deadline_ms: deadlineMs,
              remaining_ms: 0,
              status: "paused",
            },
          ],
        }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(
      <GenerativeCard
        cardId="call_auto_2"
        spec={SAMPLE_SPEC}
        storageScope="conv-abc"
        autoSubmit={{
          id: "auto-id-2",
          call_id: "call_auto_2",
          action_id: "clone",
          deadline_ms: deadlineMs,
          remaining_ms: 8_000,
          status: "pending",
        }}
        onSubmit={vi.fn()}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("genui-autosubmit-pause"));
    });

    expect(screen.queryByTestId("genui-autosubmit-countdown")).not.toBeInTheDocument();
    expect(screen.getByTestId("genui-autosubmit-paused")).toHaveTextContent("Auto-submit paused");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/chat/conversations/conv-abc/autosubmit/pause");
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ call_id: "call_auto_2" });
  });

  it("locks card as submitting instead of paused if /autosubmit/pause reports status=fired", async () => {
    const deadlineMs = Date.now() + 1_000;
    const onDue = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          ui_autosubmits: [
            {
              id: "auto-id-race",
              call_id: "call_auto_race",
              action_id: "clone",
              deadline_ms: deadlineMs,
              remaining_ms: 0,
              status: "fired",
            },
          ],
        }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    render(
      <GenerativeCard
        cardId="call_auto_race"
        spec={SAMPLE_SPEC}
        storageScope="conv-race"
        autoSubmit={{
          id: "auto-id-race",
          call_id: "call_auto_race",
          action_id: "clone",
          deadline_ms: deadlineMs,
          remaining_ms: 1_000,
          status: "pending",
        }}
        onAutoSubmitDue={onDue}
        onSubmit={vi.fn()}
      />,
    );

    await act(async () => {
      fireEvent.click(screen.getByTestId("genui-autosubmit-pause"));
      await Promise.resolve();
    });

    expect(screen.queryByTestId("genui-autosubmit-paused")).not.toBeInTheDocument();
    expect(screen.getByTestId("genui-autosubmit-countdown")).toHaveTextContent("Auto-submitting...");
    expect(screen.getByRole("button", { name: "Clone deal" })).toBeDisabled();
    expect(onDue).toHaveBeenCalledWith("call_auto_race");
  });

  it("uses remaining_ms so skewed client clocks still show accurate 10s countdown", () => {
    // Simulate server deadline 60s in the past relative to skewed client clock, but remaining_ms = 9000.
    const skewedServerDeadline = Date.now() - 60_000;
    render(
      <GenerativeCard
        cardId="call_skew"
        spec={SAMPLE_SPEC}
        storageScope="conv-skew"
        autoSubmit={{
          id: "auto-skew",
          call_id: "call_skew",
          action_id: "clone",
          deadline_ms: skewedServerDeadline,
          remaining_ms: 9_000,
          status: "pending",
        }}
        onSubmit={vi.fn()}
      />,
    );

    expect(screen.getByTestId("genui-autosubmit-countdown")).toHaveTextContent("Auto-submitting in 9s");
    expect(screen.getByRole("button", { name: "Clone deal" })).not.toBeDisabled();
  });

  it("pauses immediately on focusing or editing an input inside the card", () => {
    const onPauseAutoSubmit = vi.fn();
    const deadlineMs = Date.now() + 9_000;

    render(
      <GenerativeCard
        cardId="call_auto_3"
        spec={SAMPLE_SPEC}
        storageScope="conv-xyz"
        autoSubmit={{
          call_id: "call_auto_3",
          action_id: "clone",
          deadline_ms: deadlineMs,
          status: "pending",
        }}
        onPauseAutoSubmit={onPauseAutoSubmit}
        onSubmit={vi.fn()}
      />,
    );

    const input = screen.getByLabelText(/Deal name/);
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "Edited Deal Name" } });

    expect(screen.getByTestId("genui-autosubmit-paused")).toBeInTheDocument();
    expect(onPauseAutoSubmit).toHaveBeenCalledTimes(1);
    expect(onPauseAutoSubmit).toHaveBeenCalledWith("call_auto_3");
  });

  it("renders Auto-submitted indicator on locked card and SubmissionBubble when auto=1", () => {
    const raw = '[UI submission] card=call_auto_4 action=clone auto=1\n```json\n{"deal_name":"Q4 Video PMP"}\n```';
    const sub = parseSubmissionMessage(raw)!;
    const state = deriveGenUiState([
      {
        id: 1,
        role: "assistant",
        content: "",
        state: "done",
        toolCalls: [
          {
            id: "call_auto_4",
            name: "show_ui",
            input: JSON.stringify(SAMPLE_SPEC),
            state: "done",
            resultText: "UI_DISPLAYED",
          },
        ],
      },
    ]);

    render(
      <GenUiContext.Provider value={state}>
        <GenerativeCard
          cardId="call_auto_4"
          spec={SAMPLE_SPEC}
          submission={sub}
          onSubmit={vi.fn()}
        />
        <SubmissionBubble submission={sub} raw={raw} />
      </GenUiContext.Provider>,
    );

    expect(screen.getByTestId("genui-submitted-auto")).toHaveTextContent("Auto-submitted");
    expect(screen.getByTestId("genui-auto-submitted")).toHaveTextContent("Auto-submitted");
  });
});
