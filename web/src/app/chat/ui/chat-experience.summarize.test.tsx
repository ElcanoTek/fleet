import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// "Summarize earlier messages" streams for a while (#1999). The stream belongs
// to the chat it summarizes: switching chats mid-stream must NOT abort it (the
// server only finishes the summary while the request is open) and must not
// drag the user back — the completion used to reload the summarized chat over
// whichever chat was open by then. Unmounting aborts it.

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
let convAReads = 0;
// Set when the client cancels the body after reading summary.completed —
// proof the stream was read to completion.
let readerCancelled = false;

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (url === "/api/conversations/conv-a/summarize" && init?.method === "POST") {
      summarizeCalled = true;
      summarizeSignal = init.signal ?? undefined;
      const encoder = new TextEncoder();
      const body = new ReadableStream<Uint8Array>({
        cancel() {
          readerCancelled = true;
        },
        start(controller) {
          pushStream = (chunk) => controller.enqueue(encoder.encode(chunk));
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
  readerCancelled = false;
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

  it("switching chats mid-stream keeps it running without moving the user", async () => {
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
    // Not aborted: the server is still writing the summary.
    expect(summarizeSignal?.aborted).toBe(false);
    // The summarized chat's progress card does not follow the user.
    expect(screen.queryByText(/Partial summ/)).not.toBeInTheDocument();

    await act(async () => {
      pushStream("event: summary.completed\ndata: {}\n\n");
    });

    // Read to completion: the client let go of the body after the last event.
    await waitFor(() => expect(readerCancelled).toBe(true));
    expect(summarizeSignal?.aborted).toBe(false);
    // And the completion did not reload the summarized chat over this one.
    expect(headerTitle()).toHaveTextContent("Beta chat");
    expect(convAReads).toBe(readsAfterSwitch);
    expect(screen.queryByText(/Summarizing failed/)).not.toBeInTheDocument();
  });

  it("unmounting aborts it", async () => {
    mockBackend();
    const { unmount } = render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    await startSummarize();
    expect(summarizeSignal?.aborted).toBe(false);
    unmount();
    expect(summarizeSignal?.aborted).toBe(true);
  });
});
