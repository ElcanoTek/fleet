import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// "Summarize earlier messages" streams for a while (#1999). The stream belongs
// to the chat it summarizes: switching chats mid-stream must abort it and must
// not drag the user back — the completion used to reload the summarized chat
// over whichever chat was open by then.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225600 },
  { id: "conv-b", title: "Beta chat", persona: "default", model: "vendor/one", pinned: false, updated_at: 1767225500 },
];

const HISTORY = [
  { role: "user", type: "text", content: { text: "Long question" } },
  { role: "assistant", type: "text", content: { text: "Long answer" } },
];

let summarizeCalled = false;
let summarizeSignal: AbortSignal | undefined;
let pushStream: (chunk: string) => void;
let endStream: () => void;
let convAReads = 0;

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/conversations/conv-a/summarize" && init?.method === "POST") {
      summarizeCalled = true;
      summarizeSignal = init.signal ?? undefined;
      const encoder = new TextEncoder();
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          pushStream = (chunk) => controller.enqueue(encoder.encode(chunk));
          endStream = () => controller.close();
          init.signal?.addEventListener("abort", () => {
            try {
              controller.error(new DOMException("aborted", "AbortError"));
            } catch {
              /* already closed */
            }
          });
        },
      });
      return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });
    }
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-a") {
      convAReads++;
      return json({ conversation: CONVS[0], history: HISTORY });
    }
    if (url === "/api/conversations/conv-b") return json({ conversation: CONVS[1], history: HISTORY });
    if (/\/api\/conversations\/conv-[ab]\/inflight$/.test(url)) return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

const headerTitle = () => screen.getByTitle("Click to rename");

async function startSummarize() {
  fireEvent.click(screen.getByRole("button", { name: "More options" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: /Summarize earlier messages/ }));
  await screen.findByRole("dialog", { name: "Summarize earlier messages?" });
  fireEvent.click(screen.getByRole("button", { name: "Summarize" }));
  await waitFor(() => expect(summarizeCalled).toBe(true));
}

beforeEach(() => {
  summarizeCalled = false;
  summarizeSignal = undefined;
  convAReads = 0;
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

describe("summarizing a chat", () => {
  it("reloads the chat it summarized when it completes", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await startSummarize();
    const readsBefore = convAReads;

    await act(async () => {
      pushStream('event: summary.delta\ndata: {"text":"Short"}\n\n');
      pushStream("event: summary.completed\ndata: {}\n\n");
    });

    await waitFor(() => expect(convAReads).toBeGreaterThan(readsBefore));
    expect(headerTitle()).toHaveTextContent("Alpha chat");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("switching chats mid-stream aborts it and leaves the new chat open", async () => {
    mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await startSummarize();
    await act(async () => {
      pushStream('event: summary.delta\ndata: {"text":"Partial summ"}\n\n');
    });

    fireEvent.click(screen.getByRole("button", { name: "Beta chat" }));
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Beta chat"));
    const readsAfterSwitch = convAReads;

    // A completion that still arrives changes nothing.
    await act(async () => {
      try {
        pushStream("event: summary.completed\ndata: {}\n\n");
        endStream();
      } catch {
        /* the stream already errored on abort */
      }
    });
    await new Promise((r) => setTimeout(r, 20));

    expect(headerTitle()).toHaveTextContent("Beta chat");
    expect(convAReads).toBe(readsAfterSwitch);
    // Neither the abort nor the old chat's progress reaches this chat.
    expect(screen.queryByText(/Partial summ/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Summarizing failed/)).not.toBeInTheDocument();
    // And the request itself was let go.
    expect(summarizeSignal?.aborted).toBe(true);
  });
});
