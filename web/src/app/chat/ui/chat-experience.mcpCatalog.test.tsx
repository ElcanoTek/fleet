import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary, type MCPServerInfo } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// The Tools picker's catalog loads race (#2025): opening chat A then chat B
// (or the new-chat preview landing after a conversation's catalog) used to
// let whichever response arrived LAST paint the picker. A response now applies
// only while it is the latest load and its conversation is still open. The
// picker's badge counts the connectors that are on, so each catalog below
// turns on a different number of them.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225600 },
  { id: "conv-b", title: "Beta chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225500 },
];

function catalog(on: number): { servers: MCPServerInfo[] } {
  return {
    servers: Array.from({ length: 4 }, (_, i) => ({
      name: `srv${i}`,
      description: "",
      tools: [],
      tool_count: 1,
      enabled: i < on,
    })),
  };
}

type Deferred = { url: string; resolve: (body: unknown) => void };

let held: Deferred[] = [];

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url === "/api/mcp-servers" || /^\/api\/conversations\/conv-[ab]\/mcp-servers$/.test(url)) {
      return new Promise<Response>((resolve) => {
        held.push({ url, resolve: (body) => resolve(json(body)) });
      });
    }
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/conversations/conv-b") return json({ conversation: CONVS[1], history: [] });
    if (/\/api\/conversations\/conv-[ab]\/inflight$/.test(url)) return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
}

async function release(url: string, body: unknown) {
  await waitFor(() => expect(held.some((d) => d.url === url)).toBe(true));
  const i = held.findIndex((d) => d.url === url);
  const [d] = held.splice(i, 1);
  await act(async () => {
    d.resolve(body);
  });
}

const badge = () => screen.queryByTestId("chat-tools-badge")?.textContent ?? "0";
const headerTitle = () => screen.getByTitle("Click to rename");

beforeEach(() => {
  held = [];
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

describe("Tools picker catalog loads", () => {
  it("keeps the open chat's catalog when an older chat's load lands after it", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await release("/api/mcp-servers", catalog(1));

    // A's catalog is still in flight when the user opens B.
    await waitFor(() => expect(held.some((d) => d.url === "/api/conversations/conv-a/mcp-servers")).toBe(true));
    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));

    await release("/api/conversations/conv-b/mcp-servers", catalog(3));
    await waitFor(() => expect(badge()).toBe("3"));

    // A answers last: it must not repaint B's picker.
    await release("/api/conversations/conv-a/mcp-servers", catalog(2));
    expect(badge()).toBe("3");
  });

  it("ignores the new-chat preview when it lands after the conversation's catalog", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));

    await release("/api/conversations/conv-a/mcp-servers", catalog(2));
    await waitFor(() => expect(badge()).toBe("2"));

    await release("/api/mcp-servers", catalog(4));
    expect(badge()).toBe("2");
  });
});
