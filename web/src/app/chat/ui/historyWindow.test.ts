import { describe, expect, it } from "vitest";
import { conversationTotals, historyToMessages, type Approval, type HistoryEntry, type MemoryProposal, type Message } from "./history";
import {
  attachDeferredApprovals,
  attachLeftoverApprovals,
  attachTranscriptCards,
  earlierPageQuery,
  HISTORY_WINDOW_LIMIT,
  historyReadQuery,
  parseHistoryPage,
  prependOlderMessages,
  preludeMessages,
  preludeRetiredCount,
  withEarlierTotals,
  type HistoryWindow,
} from "./historyWindow";
import { withoutHistoryWindow } from "./historyWindow.testing";
import { deriveGenUiState } from "./genui/transcript";
import { deriveBoard, EMPTY_BOARD_PREFS } from "./dashboard/board";

// The windowed conversation load (docs/CHAT-HISTORY-WINDOW.md): what the
// client asks for, how an older page joins the loaded messages, and how the
// views of the whole conversation stay right when only part of it is loaded.

const user = (id: number, text: string): HistoryEntry => ({ id, role: "user", type: "text", content: { text } });
const reply = (id: number, text: string): HistoryEntry => ({ id, role: "assistant", type: "text", content: { text } });
const call = (id: number, callId: string, name: string, input: string): HistoryEntry => ({
  id,
  role: "assistant",
  type: "tool_call",
  content: { id: callId, name, input },
});
const result = (id: number, callId: string, name: string, text: string): HistoryEntry => ({
  id,
  role: "tool",
  type: "tool_result",
  content: { id: callId, name, text, is_err: false },
});
const card = (id: number, callId: string, spec: object): HistoryEntry[] => [
  call(id, callId, "show_ui", JSON.stringify(spec)),
  result(id + 1, callId, "show_ui", `UI_DISPLAYED card_id=${callId}`),
];
const text = (t: string) => ({ type: "text", text: t });

const heldWindow = (over: Partial<HistoryWindow> = {}): HistoryWindow => ({
  startId: 40,
  prelude: [],
  earlierTotals: null,
  earlierSummaryId: null,
  deferredApprovals: [],
  ...over,
});

describe("parseHistoryPage", () => {
  it("reads a window with more behind it", () => {
    const page = parseHistoryPage({
      has_more: true,
      next_before: 40,
      earlier: {
        cards: [call(3, "ui1", "show_ui", "{}"), null, "junk", { nope: true }],
        totals: { cost_usd: 1.5, prompt_tokens: 600, cached_tokens: 300, turns: 6 },
        summary_id: 12,
      },
    });
    expect(page).toEqual({
      hasMore: true,
      nextBefore: 40,
      earlierCards: [call(3, "ui1", "show_ui", "{}")],
      earlierTotals: { costUsd: 1.5, promptTokens: 600, cachedTokens: 300, turns: 6 },
      earlierSummaryId: 12,
    });
  });

  it("is null when nothing older exists or the response names no page", () => {
    // An older server, a whole-history read, a window that reaches the start.
    for (const raw of [undefined, null, "x", {}, { has_more: false, next_before: 40 }, { has_more: true }, { has_more: true, next_before: 0 }, { has_more: "true", next_before: 40 }]) {
      expect(parseHistoryPage(raw)).toBeNull();
    }
  });

  it("tolerates a page with no outline", () => {
    expect(parseHistoryPage({ has_more: true, next_before: 7 })).toEqual({
      hasMore: true,
      nextBefore: 7,
      earlierCards: [],
      earlierTotals: null,
      earlierSummaryId: null,
    });
  });
});

describe("the query of a conversation read", () => {
  it("asks for the newest window on a first open and on a navigation", () => {
    expect(historyReadQuery(undefined, { startOver: false, heldWhole: false })).toBe(`?limit=${HISTORY_WINDOW_LIMIT}`);
    // A navigation starts over even when part, or all, is already held.
    expect(historyReadQuery(heldWindow(), { startOver: true, heldWhole: true })).toBe(`?limit=${HISTORY_WINDOW_LIMIT}`);
    expect(historyReadQuery(undefined, { startOver: true, heldWhole: true })).toBe(`?limit=${HISTORY_WINDOW_LIMIT}`);
  });

  it("re-reads exactly what is held on any other load", () => {
    // Held in part: from where the held part starts, so a background reload
    // never drops the earlier messages the reader loaded.
    expect(historyReadQuery(heldWindow({ startId: 412 }), { startOver: false, heldWhole: true })).toBe("?from=412");
    // Held whole: the whole history, as before the window existed.
    expect(historyReadQuery(undefined, { startOver: false, heldWhole: true })).toBe("");
  });

  it("pages back from where the held part starts", () => {
    expect(earlierPageQuery(heldWindow({ startId: 412 }))).toBe(`?limit=${HISTORY_WINDOW_LIMIT}&before=412`);
  });

  it("test backends can match the conversation whatever window was asked for", () => {
    expect(withoutHistoryWindow("/api/conversations/conv-a?limit=80")).toBe("/api/conversations/conv-a");
    expect(withoutHistoryWindow("/api/conversations/conv-a?from=412")).toBe("/api/conversations/conv-a");
    for (const other of [
      "/api/conversations/conv-a?limit=80&before=412",
      "/api/conversations/conv-a?omit_history=1",
      "/api/conversations/conv-a/inflight",
      "/api/conversations?archived=true",
    ]) {
      expect(withoutHistoryWindow(other)).toBe(other);
    }
  });
});

describe("prependOlderMessages", () => {
  const loaded = historyToMessages([user(40, "newer question"), reply(41, "newer answer")]);
  const older = historyToMessages([user(30, "older question"), reply(31, "older answer")]);

  it("puts the page in front, with ids no loaded message has", () => {
    const merged = prependOlderMessages(loaded, older);
    expect(merged.map((m) => m.content)).toEqual(["older question", "older answer", "newer question", "newer answer"]);
    expect(new Set(merged.map((m) => m.id)).size).toBe(merged.length);
    expect(merged.every((m) => m.id !== 0)).toBe(true);
    // Ids still ascend through the transcript.
    expect(merged.map((m) => m.id)).toEqual([...merged.map((m) => m.id)].sort((a, b) => a - b));
  });

  it("leaves the loaded messages themselves untouched, so nothing remounts", () => {
    const merged = prependOlderMessages(loaded, older);
    expect(merged.slice(-2)[0]).toBe(loaded[0]);
    expect(merged.slice(-2)[1]).toBe(loaded[1]);
  });

  it("keeps ids unique across page after page, and beside a live message", () => {
    const live: Message = { id: Date.now(), role: "assistant", content: "streaming", state: "streaming" };
    let all = [...loaded, live];
    for (let page = 0; page < 5; page++) {
      const base = 20 - page * 4;
      all = prependOlderMessages(all, historyToMessages([user(base, `q${page}`), reply(base + 1, `a${page}`)]));
    }
    expect(all).toHaveLength(13);
    expect(new Set(all.map((m) => m.id)).size).toBe(13);
    expect(all[all.length - 1]).toBe(live);
    expect(all[0].content).toBe("q4");
  });

  it("does not add a message that is already loaded", () => {
    // A reload reached back over the same turn while the page was in flight.
    const overlapping = historyToMessages([user(30, "older question"), reply(31, "older answer"), user(40, "newer question"), reply(41, "newer answer")]);
    const merged = prependOlderMessages(loaded, overlapping);
    expect(merged.map((m) => m.content)).toEqual(["older question", "older answer", "newer question", "newer answer"]);
    // The same page arriving twice adds nothing the second time.
    expect(prependOlderMessages(merged, older)).toBe(merged);
    expect(prependOlderMessages(loaded, [])).toBe(loaded);
  });
});

describe("approval cards and a window", () => {
  const messages = historyToMessages([
    user(40, "send it"),
    call(41, "send-new", "send_email", "{}"),
    result(42, "send-new", "send_email", "APPROVAL_REQUIRED"),
    reply(43, "Waiting on you."),
  ]);
  const approval = (id: string, toolCallId: string | undefined, over: Partial<Approval> = {}): Approval =>
    ({ id, tool: "send_email", summary: {}, status: "approved", toolCallId, ...over }) as Approval;
  const memory: MemoryProposal = { id: "m1", content: "likes charts", status: "pending" } as MemoryProposal;
  const placeholderId = () => 999_999;
  const approvalIds = (m: Message) => (m.approvals ?? []).map((a) => a.id);

  it("anchors a card to the message holding its tool call", () => {
    const { messages: out, deferred } = attachTranscriptCards(messages, [approval("a-new", "send-new")], [memory], {
      moreNotLoaded: true,
      placeholderId,
    });
    expect(approvalIds(out[1])).toEqual(["a-new"]);
    expect(out[1].memoryProposals?.map((p) => p.id)).toEqual(["m1"]);
    expect(deferred).toEqual([]);
    // The input is not mutated.
    expect(messages[1].approvals).toBeUndefined();
  });

  it("holds back a settled card whose call is older than the loaded part", () => {
    const old = approval("a-old", "send-old");
    const { messages: out, deferred } = attachTranscriptCards(messages, [old, approval("a-new", "send-new")], [], {
      moreNotLoaded: true,
      placeholderId,
    });
    // Not piled onto the last reply: it waits for the page that holds its call.
    expect(approvalIds(out[1])).toEqual(["a-new"]);
    expect(deferred).toEqual([old]);
  });

  it("never holds back a card that is waiting on the reader or still running", () => {
    const pending = approval("a-pending", "send-old", { status: "pending" });
    // A running card is still "pending", flagged executing (hydrateResolvedApproval).
    const running = approval("a-running", "send-old", { status: "pending", executing: true });
    const noCall = approval("a-legacy", undefined);
    const { messages: out, deferred } = attachTranscriptCards(messages, [pending, running, noCall], [], {
      moreNotLoaded: true,
      placeholderId,
    });
    expect(deferred).toEqual([]);
    expect(approvalIds(out[1])).toEqual(["a-pending", "a-running", "a-legacy"]);
  });

  it("with the whole conversation loaded, an unmatched card goes to the last reply as it always did", () => {
    const { messages: out, deferred } = attachTranscriptCards(messages, [approval("a-old", "send-old")], [], {
      moreNotLoaded: false,
      placeholderId,
    });
    expect(deferred).toEqual([]);
    expect(approvalIds(out[1])).toEqual(["a-old"]);
  });

  it("parks cards on a placeholder when there is no assistant message", () => {
    const onlyUser = historyToMessages([user(40, "hello")]);
    const { messages: out } = attachTranscriptCards(onlyUser, [approval("a1", undefined)], [memory], {
      moreNotLoaded: false,
      placeholderId,
    });
    expect(out).toHaveLength(2);
    expect(out[1]).toMatchObject({ id: 999_999, role: "assistant", content: "" });
    expect(approvalIds(out[1])).toEqual(["a1"]);
    expect(out[1].memoryProposals).toEqual([memory]);
  });

  it("places a held-back card when its page loads, and keeps the rest waiting", () => {
    const page = historyToMessages([user(20, "send the old one"), call(21, "send-old", "send_email", "{}"), reply(22, "Sent.")]);
    const mine = approval("a-old", "send-old");
    const older = approval("a-older", "send-oldest");
    const { messages: out, deferred } = attachDeferredApprovals(page, [mine, older]);
    expect(approvalIds(out[1])).toEqual(["a-old"]);
    expect(deferred).toEqual([older]);
    expect(page[1].approvals).toBeUndefined();
    // Once the first message is loaded, what no page held goes to the last reply.
    const all = attachLeftoverApprovals(out, deferred);
    expect(approvalIds(all[1])).toEqual(["a-old", "a-older"]);
    expect(attachLeftoverApprovals(out, [])).toBe(out);
  });
});

describe("cards older than the loaded part", () => {
  // Two refreshes of a keyed panel, an unkeyed display card, and a form the
  // user answered: all before the window.
  const earlier: HistoryEntry[] = [
    ...card(3, "ui-pace", { title: "Pacing", panel: "pacing", components: [text("v1")] }),
    ...card(8, "ui-notes", { title: "Notes", components: [text("keep")] }),
    ...card(12, "ui-form", { title: "Budget", components: [{ type: "text_input", id: "amount", label: "Amount" }], actions: [{ id: "submit", label: "Submit" }] }),
    user(15, '[UI submission] card=ui-form action=submit\n```json\n{"amount":"12"}\n```'),
    ...card(20, "ui-pace-2", { title: "Pacing", panel: "pacing", components: [text("v2")] }),
  ];
  const loaded = historyToMessages([user(40, "and now?"), reply(41, "Still on track.")]);

  it("become one message per card, with ids of their own", () => {
    const prelude = preludeMessages(earlier);
    // Four cards and one answer.
    expect(prelude.map((m) => m.role)).toEqual(["assistant", "assistant", "assistant", "user", "assistant"]);
    expect(prelude.filter((m) => m.role === "assistant").every((m) => m.toolCalls?.length === 1)).toBe(true);
    const ids = new Set([...prelude, ...loaded].map((m) => m.id));
    expect(ids.size).toBe(prelude.length + loaded.length);
    // …and below any page a "Load earlier" can number.
    const paged = prependOlderMessages(loaded, historyToMessages([user(30, "q"), reply(31, "a")]));
    expect(Math.max(...prelude.map((m) => m.id))).toBeLessThan(Math.min(...paged.map((m) => m.id)));
    expect(preludeMessages([])).toEqual([]);
  });

  it("pairs results with their calls when calls were answered out of step", () => {
    const prelude = preludeMessages([
      call(1, "a", "show_ui", JSON.stringify({ title: "A", components: [text("a")] })),
      call(2, "b", "show_ui", JSON.stringify({ title: "B", components: [text("b")] })),
      result(3, "b", "show_ui", "UI_DISPLAYED card_id=b"),
      result(4, "a", "show_ui", "UI_DISPLAYED card_id=a"),
    ]);
    expect(prelude.map((m) => m.toolCalls?.[0]).map((tc) => [tc?.id, tc?.resultText])).toEqual([
      ["a", "UI_DISPLAYED card_id=a"],
      ["b", "UI_DISPLAYED card_id=b"],
    ]);
  });

  it("keep their panels on the board, at the version the whole conversation gives", () => {
    const cardMessages = [...preludeMessages(earlier), ...loaded];
    const board = deriveBoard(cardMessages, deriveGenUiState(cardMessages), EMPTY_BOARD_PREFS);
    expect(board.panels.map((p) => [p.key, p.spec.title, p.version])).toEqual([
      ["p:pacing", "Pacing", 2],
      ["c:ui-notes", "Notes", 1],
    ]);
    // The newest version is the one drawn.
    expect(JSON.stringify(board.panels[0].spec)).toContain("v2");
    // Without them the board would be empty: nothing in the window is a card.
    expect(deriveBoard(loaded, deriveGenUiState(loaded), EMPTY_BOARD_PREFS).panels).toEqual([]);

    // Exactly the board the whole history derives.
    const whole = historyToMessages([
      user(1, "start"),
      ...earlier.slice(0, 2),
      reply(5, "drawn"),
      user(7, "notes"),
      ...earlier.slice(2, 4),
      user(11, "form"),
      ...earlier.slice(4, 6),
      earlier[6],
      reply(16, "noted"),
      user(19, "refresh"),
      ...earlier.slice(7),
      user(40, "and now?"),
      reply(41, "Still on track."),
    ]);
    const wholeBoard = deriveBoard(whole, deriveGenUiState(whole), EMPTY_BOARD_PREFS);
    expect(board.panels.map((p) => [p.key, p.cardId, p.version, p.interactive, p.wide])).toEqual(
      wholeBoard.panels.map((p) => [p.key, p.cardId, p.version, p.interactive, p.wide]),
    );
  });

  it("keep the answer a card was given", () => {
    const cardMessages = [...preludeMessages(earlier), ...loaded];
    const genUi = deriveGenUiState(cardMessages);
    expect(genUi.submissions.get("ui-form")?.values).toEqual({ amount: "12" });
    expect(genUi.superseded.has("ui-pace")).toBe(true);
    expect(genUi.superseded.has("ui-pace-2")).toBe(false);
  });

  it("a card in the window still replaces one older than it", () => {
    const withRefresh = historyToMessages([
      user(40, "refresh"),
      ...card(41, "ui-pace-3", { title: "Pacing", panel: "pacing", components: [text("v3")] }),
      reply(43, "Refreshed."),
    ]);
    const cardMessages = [...preludeMessages(earlier), ...withRefresh];
    const board = deriveBoard(cardMessages, deriveGenUiState(cardMessages), EMPTY_BOARD_PREFS);
    const pacing = board.panels.find((p) => p.key === "p:pacing");
    expect(pacing?.cardId).toBe("ui-pace-3");
    expect(pacing?.version).toBe(3);
  });

  it("are retired when the compaction summary is older than the window too", () => {
    const prelude = preludeMessages(earlier);
    // The summary sits after the first two cards (entry 10).
    expect(preludeRetiredCount(heldWindow({ prelude, earlierSummaryId: 10 }))).toBe(2);
    expect(preludeRetiredCount(heldWindow({ prelude, earlierSummaryId: null }))).toBe(0);
    expect(preludeRetiredCount(undefined)).toBe(0);
  });
});

describe("withEarlierTotals", () => {
  it("adds the turns that are not loaded to the ones that are", () => {
    const loaded = conversationTotals([
      { costUsd: 0.5, promptTokens: 1000, completionTokens: 10, cachedTokens: 900, cacheCreationTokens: 0, durationMs: 5 },
    ]);
    const total = withEarlierTotals(loaded, { costUsd: 1.5, promptTokens: 3000, cachedTokens: 300, turns: 6 });
    expect(total).toEqual({ costUsd: 2, promptTokens: 4000, cachedTokens: 1200, turns: 7, cachedPercent: 30 });
    expect(withEarlierTotals(loaded, null)).toBe(loaded);
  });

  it("counts a conversation whose loaded turns recorded nothing", () => {
    const total = withEarlierTotals(conversationTotals([]), { costUsd: 1, promptTokens: 0, cachedTokens: 0, turns: 2 });
    expect(total).toMatchObject({ costUsd: 1, turns: 2, cachedPercent: null });
  });
});
