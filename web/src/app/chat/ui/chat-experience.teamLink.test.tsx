import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";
import type { BranchOrigin } from "./teamSharing";
import { withoutHistoryWindow } from "./historyWindow.testing";

// One chat, one door (docs/TEAM-SHARING.md): a teammate's Multiplayer chat
// opens LIVE for someone who may take part in it, whichever way they reach
// it — a team link (?team=<id>, Share's "Copy link for <team>") or the branch
// banner's link back — and the read-only viewer opens only for everyone else.
// A team link used to open the read-only viewer unconditionally, and its one
// action (Branch) split a live chat in two.

const OWN: ConversationSummary = {
  id: "conv-own",
  title: "My branch",
  persona: "default",
  model: "vendor/one",
  pinned: false,
  updated_at: 1767225600,
};
const LIVE: ConversationSummary = {
  id: "conv-live",
  title: "Pre-Loaded Opti Workbench",
  persona: "default",
  model: "vendor/one",
  pinned: false,
  updated_at: 1767225500,
  user_email: "abbey@example.com",
  multiplayer: true,
  team_visible: true,
};

const ORIGIN: BranchOrigin = {
  source_conversation_id: "conv-live",
  source_owner_email: "abbey@example.com",
  source_title: LIVE.title,
  branched_at: 1767225000,
  copied_files: [],
  withheld_files: [],
  source_still_shared: true,
  source_multiplayer: true,
};

type Backend = {
  teamLink?: Record<string, unknown>;
  // GET /api/conversations/conv-live: 200 for a participant, 404 once the
  // owner turned Multiplayer off or removed them.
  liveStatus?: number;
  origin?: BranchOrigin | null;
};

function mockBackend({ teamLink, liveStatus = 200, origin = null }: Backend) {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = withoutHistoryWindow(String(input));
    if (init?.method && init.method !== "GET") return json({}, 500);
    if (url === "/api/conversations") return json({ conversations: [OWN] });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-live/team-link")
      return json({ viewer_email: "kartik@example.com", ...teamLink });
    if (url === "/api/conversations/conv-own")
      return json({ conversation: OWN, branch_origin: origin, history: [] });
    if (url === "/api/conversations/conv-live")
      return liveStatus === 200
        ? json({ conversation: LIVE, history: [] })
        : json({ error: "not found" }, liveStatus);
    if (url === "/api/conversations/conv-live/team-view")
      return json({
        id: "conv-live",
        title: LIVE.title,
        owner_email: "abbey@example.com",
        team_id: "Reklaim Internal",
        updated_at: 1767225500,
        messages: [
          { id: 1, role: "user", type: "text", content: { text: "Load the opti workbench." } },
          { id: 2, role: "assistant", type: "text", content: { text: "Loaded." } },
        ],
        project_id: "",
        project_name: "",
        files: [],
        files_truncated: false,
        viewer_branch: null,
      });
    if (/\/inflight$/.test(url)) return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const fetched = (fetchMock: ReturnType<typeof mockBackend>, url: string) =>
  fetchMock.mock.calls.some(([u]) => withoutHistoryWindow(String(u)) === url);

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

// The live chat: its title in the chat header (a participant's header is
// plain text, not the owner's rename button), and no viewer anywhere.
async function expectLiveChat() {
  await waitFor(() =>
    expect(
      screen
        .getAllByRole("heading", { level: 2 })
        .some((h) => h.textContent === "Pre-Loaded Opti Workbench"),
    ).toBe(true),
  );
  expect(screen.queryByTestId("team-chat-viewer")).toBeNull();
  expect(screen.queryByRole("button", { name: /Branch to continue/ })).toBeNull();
}

// The read-only viewer, with its one action.
async function expectReadOnlyViewer() {
  expect(await screen.findByTestId("team-chat-viewer")).toBeInTheDocument();
  expect(await screen.findByTestId("team-branch-cta")).toHaveTextContent(/Read-only/);
}

describe("a team link", () => {
  it("opens a Multiplayer chat the viewer takes part in live, not read-only", async () => {
    window.history.replaceState(null, "", "/chat?team=conv-live");
    const fetchMock = mockBackend({ teamLink: { status: "multiplayer" } });
    render(<ChatExperience initialUserEmail="kartik@example.com" />);

    await expectLiveChat();
    expect(window.location.search).toBe("");
    expect(fetched(fetchMock, "/api/conversations/conv-live")).toBe(true);
    // The viewer, and its Branch action, never opened.
    expect(fetched(fetchMock, "/api/conversations/conv-live/team-view")).toBe(false);
    // The boot opened the link in its place, not the most recent own chat.
    expect(fetched(fetchMock, "/api/conversations/conv-own")).toBe(false);
  });

  it("falls back to the read-only viewer when the live chat refuses the viewer", async () => {
    window.history.replaceState(null, "", "/chat?team=conv-live");
    const fetchMock = mockBackend({ teamLink: { status: "multiplayer" }, liveStatus: 404 });
    render(<ChatExperience initialUserEmail="kartik@example.com" />);

    await expectReadOnlyViewer();
    expect(fetched(fetchMock, "/api/conversations/conv-live")).toBe(true);
    expect(fetched(fetchMock, "/api/conversations/conv-live/team-view")).toBe(true);
    expect(screen.queryByText("Unable to load conversation.")).toBeNull();
  });

  it("opens the read-only viewer for a teammate who may not take part", async () => {
    window.history.replaceState(null, "", "/chat?team=conv-live");
    const fetchMock = mockBackend({ teamLink: { status: "open" } });
    render(<ChatExperience initialUserEmail="kartik@example.com" />);

    await expectReadOnlyViewer();
    expect(fetched(fetchMock, "/api/conversations/conv-live/team-view")).toBe(true);
    // The live chat is never asked for: the server already said no.
    expect(fetched(fetchMock, "/api/conversations/conv-live")).toBe(false);
  });
});

describe("the branch banner's link back", () => {
  it("opens a Multiplayer source the viewer takes part in live", async () => {
    window.history.replaceState(null, "", "/chat");
    const fetchMock = mockBackend({ origin: ORIGIN });
    render(<ChatExperience initialUserEmail="kartik@example.com" />);

    fireEvent.click(await screen.findByRole("button", { name: "Open Abbey’s chat" }));
    await expectLiveChat();
    expect(fetched(fetchMock, "/api/conversations/conv-live")).toBe(true);
    expect(fetched(fetchMock, "/api/conversations/conv-live/team-view")).toBe(false);
  });

  it("opens a source that is only shared with the team read-only", async () => {
    window.history.replaceState(null, "", "/chat");
    const fetchMock = mockBackend({ origin: { ...ORIGIN, source_multiplayer: false } });
    render(<ChatExperience initialUserEmail="kartik@example.com" />);

    fireEvent.click(await screen.findByRole("button", { name: "Open Abbey’s chat" }));
    await expectReadOnlyViewer();
    expect(fetched(fetchMock, "/api/conversations/conv-live/team-view")).toBe(true);
    expect(fetched(fetchMock, "/api/conversations/conv-live")).toBe(false);
  });
});
