import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { FleetNotices } from "./FleetNotices";
import {
  historyToMessages,
  isNoticeOnlyMessage,
  MODEL_UPGRADED_NOTICE_KIND,
  type HistoryEntry,
} from "./history";

afterEach(() => {
  cleanup();
});

describe("model upgrade chat notices", () => {
  it("attaches model_upgraded notice metadata to the preceding assistant reply", () => {
    const entries: HistoryEntry[] = [
      { role: "user", type: "text", content: { text: "Check Q4 pacing" } },
      { role: "assistant", type: "text", content: { text: "Q4 pacing is on track." } },
      {
        role: "assistant",
        type: "turn_summary",
        content: { model: "google/gemini-4-argon", cost_usd: 0.01 },
      },
      {
        role: "system",
        type: "notice",
        content: {
          kind: MODEL_UPGRADED_NOTICE_KIND,
          from_model: "google/gemini-4-argon",
          to_model: "google/gemini-4-hydrogen",
          text: "Model upgraded from Google: Gemini 4 Argon to Google: Gemini 4 Hydrogen for new replies in this chat.",
        },
      },
    ];

    const messages = historyToMessages(entries);
    expect(messages).toHaveLength(2);
    const assistant = messages[1];
    expect(isNoticeOnlyMessage(assistant)).toBe(false);
    expect(assistant.notices).toEqual([
      "Model upgraded from Google: Gemini 4 Argon to Google: Gemini 4 Hydrogen for new replies in this chat.",
    ]);
    expect(assistant.noticeItems).toEqual([
      {
        kind: MODEL_UPGRADED_NOTICE_KIND,
        fromModel: "google/gemini-4-argon",
        toModel: "google/gemini-4-hydrogen",
        text: "Model upgraded from Google: Gemini 4 Argon to Google: Gemini 4 Hydrogen for new replies in this chat.",
      },
    ]);
  });

  it("identifies a standalone notice container after compaction as notice-only", () => {
    const entries: HistoryEntry[] = [
      { role: "user", type: "text", content: { text: "Earlier question" } },
      { role: "assistant", type: "text", content: { text: "Earlier answer" } },
      {
        role: "system",
        type: "summary",
        content: { text: "Compacted summary", removed_turns: 2, summary_tokens: 120 },
      },
      {
        role: "system",
        type: "notice",
        content: {
          kind: MODEL_UPGRADED_NOTICE_KIND,
          from_model: "google/gemini-4-argon",
          to_model: "google/gemini-4-hydrogen",
          text: "Model upgraded to Google: Gemini 4 Hydrogen.",
        },
      },
    ];

    const messages = historyToMessages(entries);
    expect(messages).toHaveLength(4);
    expect(messages[2].kind).toBe("summary");
    expect(isNoticeOnlyMessage(messages[1])).toBe(false);
    expect(isNoticeOnlyMessage(messages[2])).toBe(false);
    expect(isNoticeOnlyMessage(messages[3])).toBe(true);
  });

  it("renders a dedicated upgrade banner with friendly model display names", () => {
    render(
      <FleetNotices
        notices={["Model upgraded."]}
        items={[
          {
            kind: MODEL_UPGRADED_NOTICE_KIND,
            fromModel: "google/gemini-4-argon",
            toModel: "google/gemini-4-hydrogen",
            text: "Model upgraded.",
          },
          {
            kind: "approval_resume_skipped",
            text: "Automatic continue skipped (hourly cap reached).",
          },
        ]}
      />,
    );

    const card = screen.getByTestId("model-upgrade-notice");
    expect(card).toHaveTextContent("Model upgraded to Google: Gemini 4 Hydrogen");
    expect(card).toHaveTextContent("Previously Google: Gemini 4 Argon");
    expect(screen.getByText("Automatic continue skipped (hourly cap reached).")).toBeInTheDocument();
  });
});
