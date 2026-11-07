import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession, writeChatSession } from "./chatSessionStore";

// Opening one chat and then another before the first has loaded (#2043): the
// load that started LAST owns the view. The older response used to switch the
// active chat back to itself when it arrived late, and its `finally` cleared
// the newer load's spinner. Chats are opened with the rail's keyboard cursor
// (j/Enter), the path that stays usable while the rail shows its loading row.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225600 },
  { id: "conv-b", title: "Beta chat", persona: "default", model: "vendor/two", pinned: false, updated_at: 1767225500 },
  { id: "conv-c", title: "Gamma chat", persona: "default", model: "vendor/three", pinned: false, updated_at: 1767225400 },
];

type Deferred = { url: string; resolve: () => void };

let held: Deferred[] = [];
let chatBodies: Array<Record<string, unknown>> = [];

function mockBackend(heldConv = /^\/api\/conversations\/(conv-[bc])$/) {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (method === "POST" && url === "/api/chat") {
      chatBodies.push(JSON.parse(String(init?.body)) as Record<string, unknown>);
      return new Response("", { status: 500 });
    }
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    const m = heldConv.exec(url);
    if (m) {
      const conv = CONVS.find((c) => c.id === m[1]);
      return new Promise<Response>((resolve) => {
        held.push({ url, resolve: () => resolve(json({ conversation: conv, history: [] })) });
      });
    }
    if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/mcp-servers" || /\/mcp-servers$/.test(url)) return json({ servers: [] });
    if (/\/inflight$/.test(url)) return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
}

async function release(url: string) {
  await waitFor(() => expect(held.some((d) => d.url === url)).toBe(true));
  const i = held.findIndex((d) => d.url === url);
  const [d] = held.splice(i, 1);
  await act(async () => {
    d.resolve();
  });
}

const headerTitle = () => screen.getByTitle("Click to rename");
const key = (k: string) => fireEvent.keyDown(window, { key: k });

// Boot opens Alpha; then B (held) and C (held) are opened in that order.
async function openBThenC() {
  render(<ChatExperience initialUserEmail="user@example.com" />);
  await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
  key("j"); // cursor on Alpha
  key("j"); // Beta
  key("Enter");
  await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-b")).toBe(true));
  key("j"); // Gamma
  key("Enter");
  await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-c")).toBe(true));
}

beforeEach(() => {
  held = [];
  chatBodies = [];
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
});

describe("loadConversation supersession (#2043)", () => {
  it("keeps the newer chat open when the older chat's load lands last", async () => {
    mockBackend();
    await openBThenC();

    await release("/api/conversations/conv-c");
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Gamma chat"));

    // B answers last: it must not pull the view back to B.
    await release("/api/conversations/conv-b");
    expect(headerTitle()).toHaveTextContent("Gamma chat");

    // …and the composer still sends to the chat on screen.
    const composer = await screen.findByRole("textbox", { name: /message/i });
    fireEvent.change(composer, { target: { value: "hello" } });
    fireEvent.keyDown(composer, { key: "Enter" });
    // Generous: under a fully loaded suite run the send path can take more
    // than waitFor's default second to reach fetch.
    await waitFor(() => expect(chatBodies.length).toBe(1), { timeout: 5000 });
    expect(chatBodies[0].conversation_id).toBe("conv-c");
  }, 15000);

  it("leaves the spinner up while the newer load is still in flight", async () => {
    mockBackend();
    await openBThenC();
    expect(screen.getByText("Loading conversation...")).toBeInTheDocument();

    // The superseded load finishing first neither opens B nor clears the
    // spinner that belongs to C's load.
    await release("/api/conversations/conv-b");
    expect(screen.getByText("Loading conversation...")).toBeInTheDocument();
    expect(headerTitle()).not.toHaveTextContent("Beta chat");

    await release("/api/conversations/conv-c");
    await waitFor(() => expect(screen.queryByText("Loading conversation...")).not.toBeInTheDocument());
    expect(headerTitle()).toHaveTextContent("Gamma chat");
  });

  it("a tab-return refresh that outlasts a navigation does not take the view back", async () => {
    // A warm return to Alpha with a turn that looked mid-stream when the tab
    // was left: the tab-return refresh will want to reload it.
    writeChatSession({
      messagesByConv: new Map([
        ["conv-a", [
          { id: 1, role: "user", content: "hi", state: "done" },
          { id: 2, role: "assistant", content: "partial", state: "streaming" },
        ]],
      ]),
      conversations: CONVS,
      archivedConversations: [],
      activeConversationId: "conv-a",
      personas: [],
      selectedPersona: "default",
      selectedModel: "vendor/one",
      skills: [],
      serverConfig: { lockdownAvailable: false, lockdownOnly: false, lockdownAllowedModels: [], uploadMaxBytes: 0 },
      userEmail: "user@example.com",
    });
    mockBackend();
    let releaseInflight: (() => void) | null = null;
    const base = vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      const url = String(input);
      // A backend-down session answer skips the warm background revalidation.
      if (url === "/api/session") return new Response("", { status: 503 });
      if (url === "/api/conversations/conv-a/inflight") {
        return new Promise<Response>((resolve) => {
          releaseInflight = () => resolve(new Response(JSON.stringify({ inflight: false }), { status: 200 }));
        });
      }
      return base(input, init);
    });

    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));

    // Tab return: the refresh waits on Alpha's liveness probe…
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await waitFor(() => expect(releaseInflight).not.toBeNull());
    // …while the user opens Beta.
    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-b")).toBe(true));

    await act(async () => {
      releaseInflight!();
    });
    await release("/api/conversations/conv-b");
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));
    // Settle any reload the refresh may have started; Beta must stay.
    await act(async () => {});
    expect(headerTitle()).toHaveTextContent("Beta chat");
  });

  it("opening a chat with a live stream while another loads clears the spinner", async () => {
    mockBackend();
    const base = vi.mocked(fetch).getMockImplementation()!;
    // Alpha's turn streams and never finishes, so Alpha stays attached.
    vi.mocked(fetch).mockImplementation(async (input, init) => {
      if ((init?.method ?? "GET") === "POST" && String(input) === "/api/chat") {
        return new Response(new ReadableStream<Uint8Array>({ start() {} }), {
          status: 200,
          headers: { "content-type": "text/event-stream", "X-Fleet-Conversation-Id": "conv-a" },
        });
      }
      return base(input, init);
    });
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    const composer = await screen.findByRole("textbox", { name: /message/i });
    fireEvent.change(composer, { target: { value: "keep going" } });
    fireEvent.keyDown(composer, { key: "Enter" });
    const calls = () => vi.mocked(fetch).mock.calls.map(([u, i]) => `${i?.method ?? "GET"} ${String(u)}`);
    await waitFor(() => expect(calls()).toContain("POST /api/chat"));
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    const alphaReads = () => calls().filter((c) => c === "GET /api/conversations/conv-a").length;
    const alphaReadsBefore = alphaReads();

    // Open Beta (held), then go back to the live Alpha before Beta answers.
    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-b")).toBe(true));
    expect(screen.getByText("Loading conversation...")).toBeInTheDocument();
    key("j"); // cursor on Alpha
    key("Enter");
    await waitFor(() => expect(screen.queryByText("Loading conversation...")).not.toBeInTheDocument());

    await release("/api/conversations/conv-b");
    expect(screen.queryByText("Loading conversation...")).not.toBeInTheDocument();
    expect(headerTitle()).toHaveTextContent("Alpha chat");
    // Alpha reopened through the attached short-circuit: no history refetch.
    expect(alphaReads()).toBe(alphaReadsBefore);
  });

  it("the boot restore finishing does not clear the spinner of a chat opened meanwhile", async () => {
    mockBackend(/^\/api\/conversations\/(conv-[ab])$/);
    render(<ChatExperience initialUserEmail="user@example.com" />);
    // Boot is restoring Alpha (held); the user opens Beta from the keyboard.
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-a")).toBe(true));
    key("j"); // Alpha
    key("j"); // Beta
    key("Enter");
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-b")).toBe(true));

    await release("/api/conversations/conv-a");
    expect(screen.getByText("Loading conversation...")).toBeInTheDocument();

    await release("/api/conversations/conv-b");
    await waitFor(() => expect(screen.queryByText("Loading conversation...")).not.toBeInTheDocument());
    expect(headerTitle()).toHaveTextContent("Beta chat");
  });
});
