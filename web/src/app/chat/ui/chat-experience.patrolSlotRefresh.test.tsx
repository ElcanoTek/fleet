import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";
import type { ChatSchedule } from "./history";
import { withoutHistoryWindow } from "./historyWindow.testing";

// A scheduled patrol run that is turned away (the previous run still going,
// a full queue) queues no turn, so nothing streams to the open chat. The page
// re-reads the schedules once the patrol's slot has passed, and the bar's
// countdown moves on to the next slot instead of sticking at "due now".

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Patrol chat", persona: "assistant", model: "vendor/one", pinned: false, updated_at: 1767225600 },
];

function patrol(over: Partial<ChatSchedule>): ChatSchedule {
  return {
    id: "sched-1",
    conversation_id: "conv-a",
    kind: "patrol",
    title: "Deal patrol",
    prompt: "check",
    cron: "0 */2 * * *",
    timezone: "UTC",
    status: "active",
    run_count: 1,
    next_run_at: 0,
    ...over,
  };
}

function mockBackend(nowSec: number) {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  // The slot passed 19s ago, so the re-read is due one second after the page
  // works out the wait (the clock is held still below).
  const before = patrol({ next_run_at: nowSec - 19 });
  const after = patrol({ next_run_at: nowSec + 7200 });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = withoutHistoryWindow(String(input));
    if (init?.method && init.method !== "GET") return json({}, 500);
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/personas") return json({ personas: ["assistant"], default: "assistant" });
    if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [], schedules: [before] });
    if (url === "/api/conversations/conv-a/schedules") return json({ schedules: [after], approval_mode: "manual" });
    if (url === "/api/conversations/conv-a/inflight") return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

beforeEach(() => {
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => ({
      matches: false,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
    })),
  );
  Element.prototype.scrollIntoView = vi.fn();
  vi.spyOn(console, "error").mockImplementation(() => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  clearChatSession();
  window.history.replaceState(null, "", "/");
});

describe("Patrol bar after a slot passes", () => {
  it("re-reads the schedules and counts down to the next slot", async () => {
    // The page waits until 20s after the slot, measured from Date.now() when
    // its effect runs, and falls back to a 60s re-read if that moment has
    // already passed. With a running clock the test had under a second to boot
    // the whole chat before that happened, which a busy runner does not always
    // manage. Holding the clock on a whole second makes the wait exactly one
    // second however long the boot takes; the timer itself still runs in real
    // time.
    const nowSec = Math.floor(Date.now() / 1000);
    vi.spyOn(Date, "now").mockReturnValue(nowSec * 1000);
    const fetchMock = mockBackend(nowSec);
    render(<ChatExperience initialUserEmail="user@example.com" />);

    const bar = await screen.findByTestId("patrol-banner");
    expect(bar.textContent).toContain("due now");

    await waitFor(() => expect(screen.getByTestId("patrol-banner").textContent).toContain("in 2h"), {
      timeout: 4000,
    });
    expect(screen.getByTestId("patrol-banner").textContent).not.toContain("due now");
    expect(
      fetchMock.mock.calls.some(([u]) => String(u) === "/api/conversations/conv-a/schedules"),
    ).toBe(true);
  });

});
