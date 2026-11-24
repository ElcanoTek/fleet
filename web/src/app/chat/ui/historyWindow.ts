// The windowed conversation load (docs/CHAT-HISTORY-WINDOW.md).
//
// Opening a conversation asks for its newest turns, not its whole history: a
// long thread is megabytes of tool output the reader will mostly never scroll
// back to. "Load earlier messages" pages back, and what the entries not loaded
// contribute to views of the WHOLE conversation (dashboard panels, the answers
// cards were given, the cost total) arrives with each page as `earlier`.
//
// Pure: no React and no fetch, so the rules here unit-test without jsdom.

import {
  historyToMessages,
  type Approval,
  type HistoryEntry,
  type MemoryProposal,
  type Message,
} from "./history";

/**
 * How many history entries the first load and each "Load earlier" page ask
 * for. The server widens a page back to the start of the turn the limit falls
 * in, so a page is whole turns and can hold more.
 */
export const HISTORY_WINDOW_LIMIT = 80;

/** The cost and tokens of the turns that are not loaded (the server's sum). */
export type EarlierTotals = {
  costUsd: number;
  promptTokens: number;
  cachedTokens: number;
  turns: number;
};

/** A windowed response's `history_page`, validated. */
export type HistoryPage = {
  hasMore: boolean;
  /** The id the window starts at: `before` for the next page, `from` to re-read. */
  nextBefore: number;
  /** Card entries (show_ui calls, their results, card answers) older than the window. */
  earlierCards: HistoryEntry[];
  earlierTotals: EarlierTotals | null;
  /** The compaction summary's id when it is older than the window. */
  earlierSummaryId: number | null;
};

/**
 * What the client holds about a conversation it has only part of. A
 * conversation with no window is one held whole.
 */
export type HistoryWindow = {
  /** The id the loaded part starts at. */
  startId: number;
  /**
   * Messages rebuilt from the cards older than the loaded part. Never drawn
   * in the transcript: they stand in front of the loaded messages wherever
   * card state and the dashboard board are derived, so a panel drawn before
   * the window is still on the board.
   */
  prelude: Message[];
  earlierTotals: EarlierTotals | null;
  earlierSummaryId: number | null;
  /**
   * Settled approval cards whose tool call is older than the loaded part.
   * Each is attached to its message when a page that holds it loads.
   */
  deferredApprovals: Approval[];
};

/** A totals sum with the turns that are not loaded added in. */
export function withEarlierTotals<
  T extends { costUsd: number; promptTokens: number; cachedTokens: number; turns: number; cachedPercent: number | null },
>(loaded: T, earlier: EarlierTotals | null | undefined): T {
  if (!earlier) return loaded;
  const promptTokens = loaded.promptTokens + earlier.promptTokens;
  const cachedTokens = loaded.cachedTokens + earlier.cachedTokens;
  return {
    ...loaded,
    costUsd: loaded.costUsd + earlier.costUsd,
    promptTokens,
    cachedTokens,
    turns: loaded.turns + earlier.turns,
    // Token-weighted, as conversationTotals computes it.
    cachedPercent: promptTokens > 0 ? Math.floor((cachedTokens / promptTokens) * 100) : null,
  };
}

function finiteNumber(v: unknown): number | null {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

/**
 * parseHistoryPage reads `history_page`. Null means the response named no
 * window with more behind it: an older server, a read of the whole history, or
 * a window that already reaches the first message. The conversation is then
 * held whole.
 */
export function parseHistoryPage(raw: unknown): HistoryPage | null {
  if (!raw || typeof raw !== "object") return null;
  const page = raw as { has_more?: unknown; next_before?: unknown; earlier?: unknown };
  const nextBefore = finiteNumber(page.next_before);
  if (page.has_more !== true || nextBefore === null || nextBefore <= 0) return null;
  const earlier = (page.earlier && typeof page.earlier === "object" ? page.earlier : {}) as {
    cards?: unknown;
    totals?: unknown;
    summary_id?: unknown;
  };
  const cards = Array.isArray(earlier.cards)
    ? (earlier.cards.filter(
        (e) => !!e && typeof e === "object" && typeof (e as HistoryEntry).type === "string",
      ) as HistoryEntry[])
    : [];
  const totals = (earlier.totals && typeof earlier.totals === "object" ? earlier.totals : null) as {
    cost_usd?: unknown;
    prompt_tokens?: unknown;
    cached_tokens?: unknown;
    turns?: unknown;
  } | null;
  const turns = finiteNumber(totals?.turns) ?? 0;
  const summaryId = finiteNumber(earlier.summary_id);
  return {
    hasMore: true,
    nextBefore,
    earlierCards: cards,
    earlierTotals:
      totals && turns > 0
        ? {
            costUsd: finiteNumber(totals.cost_usd) ?? 0,
            promptTokens: finiteNumber(totals.prompt_tokens) ?? 0,
            cachedTokens: finiteNumber(totals.cached_tokens) ?? 0,
            turns,
          }
        : null,
    earlierSummaryId: summaryId !== null && summaryId > 0 ? summaryId : null,
  };
}

/**
 * The query of a conversation read.
 *  - a conversation held in part is re-read from where the held part starts,
 *    so a background reload never shrinks what the reader has loaded and the
 *    recovery checks compare like with like;
 *  - a conversation held whole is re-read whole;
 *  - otherwise (first open, or a navigation, which starts over) the newest
 *    window.
 */
export function historyReadQuery(
  window: HistoryWindow | undefined,
  options: { startOver: boolean; heldWhole: boolean },
): string {
  if (!options.startOver) {
    if (window) return `?from=${encodeURIComponent(String(window.startId))}`;
    if (options.heldWhole) return "";
  }
  return `?limit=${HISTORY_WINDOW_LIMIT}`;
}

/** The query of the page older than a window. */
export function earlierPageQuery(window: HistoryWindow): string {
  return `?limit=${HISTORY_WINDOW_LIMIT}&before=${encodeURIComponent(String(window.startId))}`;
}

// Message ids are the key every patch, row and jump is addressed by, so they
// must be unique within a conversation. historyToMessages numbers a load
// 1..N and a live message takes a timestamp, so an older page is numbered
// downward from the lowest id in use (never 0), and the prelude sits in a band
// of its own far below any page.
const PRELUDE_ID_BASE = -1_000_000_000_000;

function renumber(messages: Message[], firstId: number): Message[] {
  return messages.map((m, i) => ({ ...m, id: firstId + i }));
}

/**
 * preludeMessages rebuilds the cards older than a window as messages, for
 * card-state and board derivation only (see HistoryWindow.prelude).
 */
export function preludeMessages(cards: HistoryEntry[]): Message[] {
  if (cards.length === 0) return [];
  // One message per card and per answer. The entries between them (the
  // prompts that separated the turns) are not here, so building the list in
  // one pass would fold every card into a single message, and the board
  // orders and retires panels by the message their card sits in.
  type Segment = { entries: HistoryEntry[]; callId?: string; answered: boolean };
  const segments: Segment[] = [];
  for (const e of cards) {
    const id = (e.content as { id?: unknown } | undefined)?.id;
    if (e.type === "tool_result") {
      // A result joins the first call with its id still waiting for one
      // (calls made in one step are answered out of step, and a provider can
      // reuse an id).
      const open = segments.find((s) => s.callId !== undefined && s.callId === id && !s.answered);
      if (open) {
        open.entries.push(e);
        open.answered = true;
      }
      continue;
    }
    segments.push({
      entries: [e],
      callId: e.type === "tool_call" && typeof id === "string" ? id : undefined,
      answered: false,
    });
  }
  return renumber(
    segments.flatMap((s) => historyToMessages(s.entries)),
    PRELUDE_ID_BASE,
  );
}

/**
 * prependOlderMessages puts a page of older messages in front of the loaded
 * ones. A message already loaded (by its persisted id: a reload can reach
 * back over the same turns while the page is in flight) is not added twice,
 * and the loaded messages keep their identity and ids, so nothing on screen
 * remounts.
 */
export function prependOlderMessages(loaded: Message[], older: Message[]): Message[] {
  if (older.length === 0) return loaded;
  const have = new Set<number>();
  let lowest = 1;
  let oldestLoaded = Number.POSITIVE_INFINITY;
  for (const m of loaded) {
    if (m.id < lowest) lowest = m.id;
    if (typeof m.dbId === "number") {
      have.add(m.dbId);
      if (m.dbId < oldestLoaded) oldestLoaded = m.dbId;
    }
  }
  // Older means older: a row at or past the oldest loaded one is a turn the
  // loaded part already covers, whichever way it was grouped there.
  const fresh = older.filter(
    (m) => !(typeof m.dbId === "number" && (have.has(m.dbId) || m.dbId >= oldestLoaded)),
  );
  if (fresh.length === 0) return loaded;
  return [...renumber(fresh, Math.min(0, lowest) - fresh.length), ...loaded];
}

/**
 * attachTranscriptCards re-attaches approval cards and memory proposals to a
 * freshly loaded transcript, so a reload keeps the shape it had live. Each
 * approval anchors to the message holding its tool call; a card with no
 * matching call (older rows, mock turns, promote cards) falls back to the last
 * assistant message, and with no assistant message at all to a placeholder.
 *
 * When part of the conversation is not loaded (`moreNotLoaded`), a SETTLED
 * card that names a tool call the loaded part does not hold belongs to an
 * older message: it is returned in `deferred` rather than piled onto the last
 * reply, and attachDeferredApprovals places it when its page loads. A card
 * still waiting on the reader, or still running, is never deferred.
 */
export function attachTranscriptCards(
  messages: Message[],
  approvalCards: Approval[],
  memoryCards: MemoryProposal[],
  options: { moreNotLoaded: boolean; placeholderId: () => number },
): { messages: Message[]; deferred: Approval[] } {
  if (approvalCards.length === 0 && memoryCards.length === 0) {
    return { messages, deferred: [] };
  }
  const next = messages.slice();
  const lastAssistantIdx = (() => {
    for (let i = next.length - 1; i >= 0; i--) {
      if (next[i].role === "assistant") return i;
    }
    return -1;
  })();
  // toolCallId → message index, so each card lands where its call ran.
  const messageForCall = new Map<string, number>();
  next.forEach((m, idx) => {
    for (const tc of m.toolCalls ?? []) messageForCall.set(tc.id, idx);
  });
  const attachAt = new Map<number, Approval[]>();
  let orphaned: Approval[] = [];
  const deferred: Approval[] = [];
  for (const card of approvalCards) {
    const idx = card.toolCallId ? messageForCall.get(card.toolCallId) : undefined;
    if (
      idx === undefined &&
      options.moreNotLoaded &&
      card.toolCallId &&
      // "pending" covers a card still running too: it stays pending, flagged
      // executing, until its result lands (hydrateResolvedApproval).
      card.status !== "pending"
    ) {
      deferred.push(card);
      continue;
    }
    const target = idx ?? (lastAssistantIdx >= 0 ? lastAssistantIdx : -1);
    if (target >= 0) {
      attachAt.set(target, [...(attachAt.get(target) ?? []), card]);
    } else {
      orphaned = [...orphaned, card];
    }
  }
  for (const [idx, cards] of attachAt) {
    next[idx] = {
      ...next[idx],
      approvals: [...(next[idx].approvals ?? []), ...cards],
    };
  }
  if (memoryCards.length > 0 && lastAssistantIdx >= 0) {
    next[lastAssistantIdx] = {
      ...next[lastAssistantIdx],
      memoryProposals: [
        ...(next[lastAssistantIdx].memoryProposals ?? []),
        ...memoryCards,
      ],
    };
  }
  if (orphaned.length > 0 || (memoryCards.length > 0 && lastAssistantIdx < 0)) {
    // No assistant message exists yet — park the cards on a placeholder so
    // they still have somewhere to live.
    next.push({
      id: options.placeholderId(),
      role: "assistant",
      content: "",
      state: "done",
      approvals: orphaned,
      memoryProposals: lastAssistantIdx < 0 ? memoryCards : [],
    });
  }
  return { messages: next, deferred };
}

/**
 * attachDeferredApprovals places the deferred cards whose tool call a newly
 * loaded page holds, and returns the ones still waiting for an older page.
 */
export function attachDeferredApprovals(
  page: Message[],
  deferred: Approval[],
): { messages: Message[]; deferred: Approval[] } {
  if (deferred.length === 0) return { messages: page, deferred };
  const messageForCall = new Map<string, number>();
  page.forEach((m, idx) => {
    for (const tc of m.toolCalls ?? []) messageForCall.set(tc.id, idx);
  });
  const next = page.slice();
  const waiting: Approval[] = [];
  for (const card of deferred) {
    const idx = card.toolCallId ? messageForCall.get(card.toolCallId) : undefined;
    if (idx === undefined) {
      waiting.push(card);
      continue;
    }
    next[idx] = { ...next[idx], approvals: [...(next[idx].approvals ?? []), card] };
  }
  return { messages: next, deferred: waiting };
}

/**
 * attachLeftoverApprovals places the deferred cards no page held, once the
 * first message is loaded: on the last assistant message, where a card with
 * no matching call has always gone.
 */
export function attachLeftoverApprovals(messages: Message[], leftover: Approval[]): Message[] {
  if (leftover.length === 0) return messages;
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role !== "assistant") continue;
    const next = messages.slice();
    next[i] = { ...next[i], approvals: [...(next[i].approvals ?? []), ...leftover] };
    return next;
  }
  return messages;
}

/**
 * How many prelude messages are older than the compaction summary, when the
 * summary itself is older than the loaded part: the board reads a panel whose
 * card sits before the summary as retired, exactly as it does when the whole
 * conversation is loaded.
 */
export function preludeRetiredCount(window: HistoryWindow | undefined): number {
  if (!window || window.earlierSummaryId === null) return 0;
  const summaryId = window.earlierSummaryId;
  return window.prelude.filter((m) => typeof m.dbId === "number" && m.dbId < summaryId).length;
}
