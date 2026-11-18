import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";
import type { ChatSchedule } from "./history";

// A scheduled patrol run skipped for a waiting approval card queues no turn,
// so nothing streams to the open chat. The page re-reads the schedules once
// the patrol's slot has passed, and the bar's "Patrol on hold" note appears.

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
  // The slot passed 19s ago, so the re-read is due about a second from now.
  const before = patrol({ next_run_at: nowSec - 19 });
  const after = patrol({
    next_run_at: nowSec + 7200,
    skipped: { reason: "approval_pending", count: 1, slots: [nowSec - 19] },
  });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
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

describe("Patrol on hold", () => {
  it("re-reads the schedules after a slot passes and shows the skipped-run note", async () => {
    const fetchMock = mockBackend(Math.floor(Date.now() / 1000));
    render(<ChatExperience initialUserEmail="user@example.com" />);

    await screen.findByTestId("patrol-banner");
    expect(screen.queryByTestId("patrol-skipped-note")).toBeNull();

    await waitFor(
      () => expect(screen.getByTestId("patrol-skipped-note").textContent).toContain("Patrol on hold"),
      { timeout: 4000 },
    );
    expect(
      fetchMock.mock.calls.some(([u]) => String(u) === "/api/conversations/conv-a/schedules"),
    ).toBe(true);
  });
});
