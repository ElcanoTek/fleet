import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";
import { HISTORY_WINDOW_LIMIT } from "./historyWindow";
import type { HistoryEntry } from "./history";

// jsdom has no layout, so the real virtualizer mounts no transcript rows.
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: (opts: { count: number; getItemKey: (i: number) => string | number }) => ({
    getVirtualItems: () =>
      Array.from({ length: opts.count }, (_, i) => ({
        index: i,
        key: opts.getItemKey(i),
        start: i * 180,
        size: 180,
        end: (i + 1) * 180,
        lane: 0,
      })),
    getTotalSize: () => opts.count * 180,
    measureElement: () => {},
    scrollToIndex: () => {},
    getOffsetForIndex: () => [0, "start"],
  }),
}));

// Opening a long conversation loads its newest turns only, and "Load earlier
// messages" pages back (docs/CHAT-HISTORY-WINDOW.md). The fake backend below
// is the server's contract: `?limit=N` answers the newest window, `&before=ID`
// the page older than ID, `?from=ID` everything from ID on, each with a
// `history_page` while older entries remain.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225600 },
  { id: "conv-b", title: "Beta chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225500 },
];

const turn = (n: number): HistoryEntry[] => [
  { id: n * 10, role: "user", type: "text", content: { text: `question ${n}` } },
  { id: n * 10 + 1, role: "assistant", type: "text", content: { text: `answer ${n}` } },
];
// Six turns, ids 10..61. A pacing panel was drawn in turn 1, long before the
// window, and never since.
const PANEL: HistoryEntry[] = [
  {
    id: 12,
    role: "assistant",
    type: "tool_call",
    content: {
      id: "ui-pace",
      name: "show_ui",
      input: JSON.stringify({ title: "Pacing board", panel: "pacing", components: [{ type: "text", text: "On track at 61%" }] }),
    },
  },
  { id: 13, role: "tool", type: "tool_result", content: { id: "ui-pace", name: "show_ui", text: "UI_DISPLAYED card_id=ui-pace", is_err: false } },
];
const TURNS = [1, 2, 3, 4, 5, 6].map(turn);
const totalsBefore = (start: number) => {
  const turns = TURNS.filter((t) => (t[0].id as number) < start).length;
  return { cost_usd: turns * 0.25, prompt_tokens: turns * 100, cached_tokens: turns * 50, turns };
};
// The window that starts at turn `first` (1-based): its entries and its page.
const windowFrom = (first: number, through = 6) => {
  const start = first * 10;
  return {
    history: TURNS.slice(first - 1, through).flat(),
    ...(first > 1
      ? { history_page: { has_more: true, next_before: start, earlier: { cards: PANEL, totals: totalsBefore(start) } } }
      : { history_page: { has_more: false, next_before: start } }),
  };
};

type Held = { url: string; release: () => void };
let held: Held[] = [];
// Requests the test wants held open until it releases them, by URL.
let holdUrls = new Set<string>();
// Overrides for single URLs (a failure, a moved window).
let overrides = new Map<string, () => Response>();
let reads: string[] = [];

const FIRST = `/api/conversations/conv-a?limit=${HISTORY_WINDOW_LIMIT}`;
const PAGE_BEFORE_5 = `/api/conversations/conv-a?limit=${HISTORY_WINDOW_LIMIT}&before=50`;
const PAGE_BEFORE_3 = `/api/conversations/conv-a?limit=${HISTORY_WINDOW_LIMIT}&before=30`;

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const answer = (url: string): Response => {
    const override = overrides.get(url);
    if (override) return override();
    if (url === "/api/session") return json({ email: "user@example.com" });
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    // The newest window is turns 5–6; pages are two turns each.
    if (url === FIRST) return json({ conversation: CONVS[0], ...windowFrom(5) });
    if (url === PAGE_BEFORE_5) return json({ conversation: CONVS[0], ...windowFrom(3, 4) });
    if (url === PAGE_BEFORE_3) return json({ conversation: CONVS[0], ...windowFrom(1, 2), history: [...TURNS[0], ...PANEL, ...TURNS[1]].sort((a, b) => (a.id as number) - (b.id as number)) });
    const from = /^\/api\/conversations\/conv-a\?from=(\d+)$/.exec(url);
    if (from) return json({ conversation: CONVS[0], ...windowFrom(Number(from[1]) / 10) });
    if (url.startsWith("/api/conversations/conv-b?")) return json({ conversation: CONVS[1], history: turn(9) });
    if (/\/api\/conversations\/conv-[ab]\/inflight$/.test(url)) return json({ inflight: false });
    if (/\/api\/conversations\/conv-[ab]\/mcp-servers$/.test(url)) return json({ servers: [] });
    return json({}, 404);
  };
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (/^\/api\/conversations\/conv-[ab]\?/.test(url)) reads.push(url);
    if (holdUrls.has(url)) {
      holdUrls.delete(url);
      return new Promise<Response>((resolve) => {
        held.push({ url, release: () => resolve(answer(url)) });
      });
    }
    return answer(url);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

async function release(url: string) {
  const i = held.findIndex((h) => h.url === url);
  expect(i, `no held request for ${url}`).toBeGreaterThanOrEqual(0);
  const [h] = held.splice(i, 1);
  await act(async () => {
    h.release();
  });
}

const headerTitle = () => screen.getByTitle("Click to rename");
const loadEarlier = () => screen.queryByRole("button", { name: /Load(ing)? earlier messages/ });
const transcript = () => screen.getByRole("region", { name: "Conversation" });
// The questions on screen, in document order.
const questions = () =>
  Array.from(transcript().querySelectorAll('[data-testid="user-message-bubble"]')).map((el) => el.textContent?.trim());

async function openAlpha() {
  render(<ChatExperience initialUserEmail="user@example.com" />);
  await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
  await waitFor(() => expect(questions()).toEqual(["question 5", "question 6"]));
}

beforeEach(() => {
  held = [];
  holdUrls = new Set();
  overrides = new Map();
  reads = [];
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
  window.localStorage.clear();
});

describe("opening a long conversation", () => {
  it("asks for the newest window, not the whole history", async () => {
    mockBackend();
    await openAlpha();

    expect(reads).toEqual([FIRST]);
    // The newest turns are drawn; the older ones were never downloaded.
    expect(within(transcript()).getByText("answer 6")).toBeInTheDocument();
    expect(within(transcript()).queryByText("answer 4")).not.toBeInTheDocument();
    // …and the way to them is the first thing in the transcript.
    const button = loadEarlier();
    expect(button).toBeInTheDocument();
    expect(button).toHaveAccessibleName("Load earlier messages");
    expect(button).toHaveAttribute("aria-busy", "false");
    const first = transcript().querySelector("[data-index='0']");
    expect(first?.contains(button)).toBe(true);
  });

  it("shows no control for a conversation that fits in one window", async () => {
    mockBackend();
    overrides.set(FIRST, () => new Response(JSON.stringify({ conversation: CONVS[0], ...windowFrom(1) })));
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(questions()).toHaveLength(6));
    expect(loadEarlier()).not.toBeInTheDocument();
  });

  it("treats a server that names no page as the whole conversation", async () => {
    // A server from before the window existed ignores `limit` and answers
    // with everything and no history_page.
    mockBackend();
    overrides.set(FIRST, () => new Response(JSON.stringify({ conversation: CONVS[0], history: TURNS.flat() })));
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(questions()).toHaveLength(6));
    expect(loadEarlier()).not.toBeInTheDocument();
  });
});

describe("Load earlier messages", () => {
  it("puts each older page in front, in order, until the first message", async () => {
    mockBackend();
    await openAlpha();

    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toEqual(["question 3", "question 4", "question 5", "question 6"]));
    expect(reads).toEqual([FIRST, PAGE_BEFORE_5]);
    // More remains: the control is still first.
    expect(transcript().querySelector("[data-index='0']")?.contains(loadEarlier())).toBe(true);

    fireEvent.click(loadEarlier()!);
    await waitFor(() =>
      expect(questions()).toEqual(["question 1", "question 2", "question 3", "question 4", "question 5", "question 6"]),
    );
    expect(reads).toEqual([FIRST, PAGE_BEFORE_5, PAGE_BEFORE_3]);
    // The first message is loaded: nothing earlier to offer.
    expect(loadEarlier()).not.toBeInTheDocument();
    // Every row is there once.
    expect(within(transcript()).getAllByText(/^answer \d$/)).toHaveLength(6);
  });

  it("keeps the reader where they were: the view moves by the height that arrived above", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    // jsdom lays nothing out; give the scroller a height and an offset that
    // sticks, and count the writes to it.
    const scroller = await screen.findByRole("region", { name: "Conversation" });
    let scrollTop = 0;
    let writes = 0;
    Object.defineProperty(scroller, "scrollTop", {
      configurable: true,
      get: () => scrollTop,
      set: (v: number) => {
        scrollTop = v;
        writes += 1;
      },
    });
    Object.defineProperty(scroller, "scrollHeight", { configurable: true, get: () => 6000 });
    Object.defineProperty(scroller, "clientHeight", { configurable: true, get: () => 600 });
    await waitFor(() => expect(questions()).toEqual(["question 5", "question 6"]));
    // Opening a chat puts the reader at its end, on the next frame and again
    // 80ms later. Wait for both: this test is about what happens after that,
    // when the reader has scrolled up to the top of what is loaded.
    await waitFor(() => expect(writes).toBeGreaterThanOrEqual(2));
    expect(scrollTop).toBe(6000);
    scrollTop = 40;
    fireEvent.scroll(scroller);
    const button = loadEarlier()!;
    button.focus();
    expect(document.activeElement).toBe(button);

    fireEvent.click(button);
    await waitFor(() => expect(questions()).toHaveLength(4));
    // Two turns (four rows of 180px in the test virtualizer) arrived above
    // the row the reader had first, so the offset grew by exactly that.
    expect(scrollTop).toBe(40 + 4 * 180);
    // The control the reader pressed moved away with the page; the keyboard
    // focus went to the transcript, not to the page body.
    expect(document.activeElement).toBe(scroller);
  });

  it("does not add a turn twice when a page overlaps what is loaded", async () => {
    mockBackend();
    // The page reaches forward over turn 5, which is already on screen.
    overrides.set(PAGE_BEFORE_5, () => new Response(JSON.stringify({ conversation: CONVS[0], ...windowFrom(3, 5) })));
    await openAlpha();

    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toEqual(["question 3", "question 4", "question 5", "question 6"]));
    expect(within(transcript()).getAllByText("answer 5")).toHaveLength(1);
  });

  it("is busy while its page loads, and a second press sends nothing", async () => {
    mockBackend();
    await openAlpha();
    holdUrls.add(PAGE_BEFORE_5);

    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(held.some((h) => h.url === PAGE_BEFORE_5)).toBe(true));
    const busy = loadEarlier()!;
    expect(busy).toHaveAttribute("aria-busy", "true");
    expect(busy).toHaveAttribute("aria-disabled", "true");
    expect(busy).toHaveAccessibleName("Loading earlier messages…");
    expect(within(transcript()).getByRole("status")).toHaveTextContent("Loading earlier messages…");
    fireEvent.click(busy);
    fireEvent.click(busy);
    expect(reads.filter((u) => u === PAGE_BEFORE_5)).toHaveLength(1);

    await release(PAGE_BEFORE_5);
    await waitFor(() => expect(questions()).toHaveLength(4));
    expect(loadEarlier()).toHaveAttribute("aria-busy", "false");
  });

  it("says when a page could not be loaded, and loads it on the next press", async () => {
    mockBackend();
    await openAlpha();
    overrides.set(PAGE_BEFORE_5, () => new Response("boom", { status: 500 }));

    fireEvent.click(loadEarlier()!);
    await waitFor(() =>
      expect(within(transcript()).getByRole("status")).toHaveTextContent("Could not load earlier messages. Try again."),
    );
    // Nothing was lost or half-applied.
    expect(questions()).toEqual(["question 5", "question 6"]);
    expect(loadEarlier()).toHaveAttribute("aria-busy", "false");

    overrides.delete(PAGE_BEFORE_5);
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toHaveLength(4));
    expect(within(transcript()).queryByText(/Could not load earlier messages/)).not.toBeInTheDocument();
  });

  it("drops a page that arrives after the conversation was opened afresh elsewhere in its history", async () => {
    mockBackend();
    await openAlpha();
    holdUrls.add(PAGE_BEFORE_5);
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(held.some((h) => h.url === PAGE_BEFORE_5)).toBe(true));

    // While the page is in flight the chat is reopened from the rail, and by
    // now a new turn has moved its newest window forward: it starts at turn 6.
    overrides.set(FIRST, () => new Response(JSON.stringify({ conversation: CONVS[0], ...windowFrom(6) })));
    // (The rail's keyboard cursor: j lands on Alpha, Enter opens it.)
    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "Enter" });
    await waitFor(() => expect(questions()).toEqual(["question 6"]));

    // The page was the one before turn 5. It does not adjoin what is held now.
    await release(PAGE_BEFORE_5);
    expect(questions()).toEqual(["question 6"]);
    expect(within(transcript()).queryByText("answer 4")).not.toBeInTheDocument();
    // The control works from where the window now starts.
    expect(loadEarlier()).toHaveAttribute("aria-busy", "false");
  });

  it("a page for one chat does not land in another", async () => {
    mockBackend();
    await openAlpha();
    holdUrls.add(PAGE_BEFORE_5);
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(held.some((h) => h.url === PAGE_BEFORE_5)).toBe(true));

    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));
    await waitFor(() => expect(questions()).toEqual(["question 9"]));
    await release(PAGE_BEFORE_5);
    // Beta is untouched, and shows no control of Alpha's.
    expect(questions()).toEqual(["question 9"]);
    expect(loadEarlier()).not.toBeInTheDocument();
  });
});

describe("re-reading a conversation held in part", () => {
  it("asks for what is held, so the earlier messages the reader loaded stay", async () => {
    mockBackend();
    await openAlpha();
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toHaveLength(4));
    reads = [];

    // Leaving /chat and coming back revalidates the open chat in the
    // background. It re-reads from turn 3, where the held part starts — not
    // the newest window (which would drop turns 3–4 from under the reader)
    // and not the whole history.
    cleanup();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(reads).toContain("/api/conversations/conv-a?from=30"));
    expect(reads.filter((u) => u.startsWith("/api/conversations/conv-a?"))).toEqual(["/api/conversations/conv-a?from=30"]);
    await waitFor(() => expect(questions()).toEqual(["question 3", "question 4", "question 5", "question 6"]));
    expect(loadEarlier()).toBeInTheDocument();
  });

  it("opening the chat again from the rail starts over with the newest window", async () => {
    mockBackend();
    await openAlpha();
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toHaveLength(4));
    reads = [];

    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));
    fireEvent.click(screen.getByRole("button", { name: "Alpha chat" }));
    await waitFor(() => expect(questions()).toEqual(["question 5", "question 6"]));
    expect(reads[reads.length - 1]).toBe(FIRST);
  });
});

describe("dashboard view of a conversation held in part", () => {
  it("still shows a panel drawn before the loaded messages", async () => {
    mockBackend();
    await openAlpha();
    // The card is not in the window: the transcript does not hold it.
    expect(within(transcript()).queryByText("On track at 61%")).not.toBeInTheDocument();

    fireEvent.keyDown(window, { key: "d" });
    const board = await screen.findByTestId("dashboard-board");
    await waitFor(() => expect(within(board).getAllByTestId("dashboard-panel")).toHaveLength(1));
    expect(within(board).getByText("Pacing board")).toBeInTheDocument();
    await waitFor(() => expect(within(board).getByText("On track at 61%")).toBeInTheDocument());
    expect(screen.getByTestId("dashboard-panel-count")).toHaveTextContent("1");
  });

  it("draws the panel once when its own turn is loaded", async () => {
    mockBackend();
    await openAlpha();
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toHaveLength(4));
    fireEvent.click(loadEarlier()!);
    await waitFor(() => expect(questions()).toHaveLength(6));

    fireEvent.keyDown(window, { key: "d" });
    const board = await screen.findByTestId("dashboard-board");
    await waitFor(() => expect(within(board).getAllByTestId("dashboard-panel")).toHaveLength(1));
    expect(screen.getByTestId("dashboard-panel-count")).toHaveTextContent("1");
  });
});
