import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// Web Storage can throw on every access (Safari private mode, a full quota,
// site data blocked). Chat used to read "chat-show-stats" in a mount effect
// and write it inside a state updater unguarded, so a throwing storage took
// the whole view down (#2002). These mount chat against such a storage.

const CONVS: ConversationSummary[] = [
  {
    id: "conv-a",
    title: "Alpha chat",
    persona: "default",
    model: "vendor/one",
    pinned: false,
    updated_at: 1767225600,
  },
];

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === "/api/conversations") return json({ conversations: CONVS });
      if (url === "/api/conversations?archived=true") return json({ conversations: [] });
      if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
      if (url === "/api/conversations/conv-a/inflight") return json({ inflight: false });
      return json({}, 404);
    }),
  );
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
});

describe("chat with a storage that throws", () => {
  it("mounts, and the details toggle still works for the page", async () => {
    for (const method of ["getItem", "setItem", "removeItem", "key"] as const) {
      vi.spyOn(Storage.prototype, method).mockImplementation(() => {
        throw new DOMException("The operation is insecure.", "SecurityError");
      });
    }
    mockBackend();

    render(<ChatExperience initialUserEmail="user@example.com" />);
    await waitFor(() => expect(screen.getByTitle("Click to rename")).toHaveTextContent("Alpha chat"));

    fireEvent.click(screen.getByRole("button", { name: "More options" }));
    fireEvent.click(await screen.findByRole("menuitemcheckbox", { name: /Show details/ }));
    expect(await screen.findByRole("menuitemcheckbox", { name: /Hide details/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });
});
