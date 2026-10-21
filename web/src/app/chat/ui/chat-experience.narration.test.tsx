import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience } from "./chat-experience";
import { ChatToastProvider } from "./ChatToasts";
import { clearChatSession } from "./chatSessionStore";

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

// The default chat view while the agent works (owner + designer review,
// 2026-10-20). A tool-using turn streams the model's narration ahead of each
// call; the default view used to render it as the reply — sentences glued
// together, internals on show, and the working status gone from the first
// word on. It now shows the status line until the answer arrives, and "Show
// details" carries the narration as "Working notes". Driven through the real
// ChatExperience with a stream the test feeds frame by frame, so each
// assertion is what a person sees at that moment of the turn.

const CONV = "conv-narration";
const NARRATION_1 = "Let me look up the deal first.";
const NARRATION_2 = "Found it. Now I'll check its targeting.";
const ANSWER = "The deal is live and targets the US only.";

type Frame = { event: string; data: unknown };

function controlledStream() {
  const encoder = new TextEncoder();
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  let id = 0;
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      controller = c;
    },
  });
  return {
    body,
    async push(...frames: Frame[]) {
      await act(async () => {
        for (const f of frames) {
          id += 1;
          controller.enqueue(
            encoder.encode(`id: ${id}\nevent: ${f.event}\ndata: ${JSON.stringify(f.data)}\n\n`),
          );
        }
        // Let the pump read, parse and apply (text.delta honors a 250 ms
        // minimum-thinking delay on the first chunk).
        await new Promise((r) => setTimeout(r, 300));
      });
    },
    async close() {
      await act(async () => {
        controller.close();
        await new Promise((r) => setTimeout(r, 50));
      });
    },
  };
}

function mockBackend(stream: ReturnType<typeof controlledStream>) {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      if (method === "POST" && url === "/api/chat") {
        return new Response(stream.body, {
          status: 200,
          headers: { "content-type": "text/event-stream", "X-Fleet-Conversation-Id": CONV },
        });
      }
      if (url === "/api/conversations") return json({ conversations: [] });
      if (url === "/api/conversations?archived=true") return json({ conversations: [] });
      if (url.endsWith("/inflight")) return json({ inflight: false });
      if (url.includes("/queue")) return json({ items: [] });
      return json({}, 404);
    }),
  );
}

async function sendPrompt() {
  render(
    <ChatToastProvider>
      <ChatExperience initialUserEmail="user@example.com" />
    </ChatToastProvider>,
  );
  const composer = await screen.findByRole("textbox", { name: /message/i });
  fireEvent.change(composer, { target: { value: "Is deal 42 live?" } });
  fireEvent.keyDown(composer, { key: "Enter" });
  await screen.findByTestId("assistant-working-status");
}

const status = () => screen.getByTestId("assistant-working-status");
const detailsToggle = () => screen.getByRole("button", { name: /show details|hide details/i });

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
  try {
    window.localStorage.clear();
  } catch {
    // storage unavailable: nothing to reset
  }
});

describe("the default view while the agent works", () => {
  it("shows a status line, not the narration, and keeps the narration for Show details", async () => {
    const stream = controlledStream();
    mockBackend(stream);
    await sendPrompt();
    expect(status()).toHaveTextContent("Thinking");

    // The model narrates, then calls a tool: the status line stays up and
    // names the step; the narration never shows as the reply.
    await stream.push({ event: "text.delta", data: { text: NARRATION_1 } });
    expect(screen.queryByText(NARRATION_1)).toBeNull();
    expect(status()).toHaveTextContent("Thinking");
    await stream.push({
      event: "tool.call",
      data: { id: "call-1", name: "bash", input: '{"command":"lookup 42"}' },
    });
    expect(status()).toHaveTextContent("Running shell");
    expect(screen.queryByText(NARRATION_1)).toBeNull();

    // The next step's narration arrives already separated by the server.
    await stream.push(
      { event: "tool.result", data: { id: "call-1", name: "bash", text: "ok", is_err: false } },
      { event: "text.delta", data: { text: `\n\n${NARRATION_2}` } },
      { event: "tool.call", data: { id: "call-2", name: "bash", input: '{"command":"targeting 42"}' } },
    );
    expect(screen.queryByText(/Found it/)).toBeNull();
    expect(status()).toHaveTextContent("Running shell");

    // Show details: the narration is there, as two paragraphs, not
    // "first.Found it."
    fireEvent.click(detailsToggle());
    const notes = (await screen.findByText("Working notes")).closest("div") as HTMLElement;
    expect(within(notes).getByText(NARRATION_1)).toBeInTheDocument();
    expect(within(notes).getByText(NARRATION_2)).toBeInTheDocument();
    expect(notes.textContent).not.toContain("first.Found");
    fireEvent.click(detailsToggle());

    // The answer streams and is replaced by the persisted final text.
    await stream.push(
      { event: "tool.result", data: { id: "call-2", name: "bash", text: "US", is_err: false } },
      { event: "text.delta", data: { text: `\n\n${ANSWER}` } },
      { event: "text.replace", data: { text: ANSWER } },
      { event: "turn.completed", data: { model: "vendor/one", cost_usd: 0 } },
    );
    await stream.close();
    expect(await screen.findByText(ANSWER)).toBeInTheDocument();
    expect(screen.queryByTestId("assistant-working-status")).toBeNull();
    expect(screen.queryByText(NARRATION_1)).toBeNull();
    expect(screen.queryByText(NARRATION_2)).toBeNull();
  });

  it("streams a long answer as before, without waiting for the turn to end", async () => {
    const stream = controlledStream();
    mockBackend(stream);
    await sendPrompt();
    const long = `${"The deal is live. ".repeat(20)}Targeting is US only.`;
    await stream.push({ event: "text.delta", data: { text: long } });
    await waitFor(() => expect(screen.getByText(/Targeting is US only\./)).toBeInTheDocument());
    expect(screen.queryByTestId("assistant-working-status")).toBeNull();
  });

  it("keeps a stopped turn's whole partial transcript, narration included", async () => {
    const stream = controlledStream();
    mockBackend(stream);
    await sendPrompt();
    await stream.push(
      { event: "text.delta", data: { text: NARRATION_1 } },
      { event: "tool.call", data: { id: "call-1", name: "bash", input: "{}" } },
      { event: "tool.result", data: { id: "call-1", name: "bash", text: "ok", is_err: false } },
      { event: "text.delta", data: { text: `\n\n${NARRATION_2}` } },
      { event: "turn.cancelled", data: { model: "vendor/one" } },
    );
    await stream.close();
    expect(await screen.findByText(NARRATION_1)).toBeInTheDocument();
    expect(screen.getByText(NARRATION_2)).toBeInTheDocument();
  });
});
