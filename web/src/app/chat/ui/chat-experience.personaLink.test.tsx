import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// The new-chat link Settings → Personas uses (?persona=<ref>&draft=<text>,
// docs/USER-PERSONAS.md): it opens a FRESH chat on the persona — not the most
// recent conversation — puts the draft in the composer without sending it,
// and strips both params from the URL.

const CONVS: ConversationSummary[] = [
  { id: "conv-a", title: "Alpha chat", persona: "assistant", model: "vendor/one", pinned: false, updated_at: 1767225600 },
];

function mockBackend() {
  const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method && init.method !== "GET") return json({}, 500);
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/personas")
      return json({
        personas: ["assistant"],
        default: "assistant",
        user_personas: [
          { ref: "user:my-twin", name: "my-twin", display_name: "My Twin", description: "", kind: "twin", autonomy: "advise" },
        ],
      });
    if (url === "/api/conversations/conv-a") return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/conversations/conv-a/inflight") return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
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
  window.history.replaceState(null, "", "/");
});

describe("persona new-chat link", () => {
  it("opens a fresh chat on the persona with the draft in the composer", async () => {
    window.history.replaceState(null, "", "/chat?persona=user%3Amy-twin&draft=Help%20me%20build%20my%20twin");
    const fetchMock = mockBackend();
    render(<ChatExperience initialUserEmail="user@example.com" />);

    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Persona — My Twin" })).toBeInTheDocument(),
    );
    const composer = await screen.findByDisplayValue("Help me build my twin");
    expect(composer.tagName).toBe("TEXTAREA");
    expect(window.location.search).toBe("");
    // The most recent conversation is NOT opened over the new chat.
    expect(fetchMock.mock.calls.some(([u]) => String(u) === "/api/conversations/conv-a")).toBe(false);
  });

  it("lets the user click Share in the composer from the start of a new chat and preserves the draft, title, and pre-chat options", async () => {
    window.history.replaceState(null, "", "/chat?persona=user%3Amy-twin&draft=Draft%20before%20sharing");
    const fetchMock = mockBackend();
    const base = fetchMock.getMockImplementation()!;
    let createdPayload: Record<string, unknown> | null = null;
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === "/api/conversations" && init?.method === "POST") {
        createdPayload = JSON.parse(String(init.body ?? "{}")) as Record<string, unknown>;
        return new Response(
          JSON.stringify({
            id: "conv-new",
            title: createdPayload.title || "New conversation",
            persona: "user:my-twin",
            model: "vendor/one",
            pinned: false,
            updated_at: 1767225700,
            auto_approve: Boolean(createdPayload.auto_approve),
          }),
          { status: 200 },
        );
      }
      return base(input, init);
    });
    render(<ChatExperience initialUserEmail="user@example.com" />);

    const composer = await screen.findByDisplayValue("Draft before sharing");
    expect(composer.tagName).toBe("TEXTAREA");

    // Enable auto-approve before clicking Share to verify pre-chat options are seeded.
    fireEvent.click(screen.getByTestId("composer-more-button"));
    fireEvent.click(screen.getByRole("menuitemcheckbox", { name: /Auto-approve tools in this chat/ }));

    const shareBtn = screen.getByTestId("composer-share-button");
    fireEvent.click(shareBtn);

    const dialog = await screen.findByRole("dialog", { name: "Share this chat" });
    expect(dialog).toBeInTheDocument();
    expect(createdPayload).toMatchObject({
      title: "Draft before sharing",
      persona: "user:my-twin",
      auto_approve: true,
    });
    // Draft stays intact in the composer after promoting onto the materialized conversation.
    expect(screen.getByDisplayValue("Draft before sharing")).toBeInTheDocument();

    // Close the share dialog and verify that on the empty materialized chat
    // ("Draft before sharing"), because no messages have been sent yet, the
    // user can still switch the persona.
    fireEvent.keyDown(dialog, { key: "Escape" });
    fireEvent.click(screen.getByTestId("composer-more-button"));
    const personaItem = screen.getByRole("menuitem", { name: "Persona — My Twin" });
    expect(personaItem).not.toBeDisabled();
    fireEvent.click(personaItem);
    const listbox = screen.getByRole("listbox", { name: "Persona" });
    fireEvent.click(within(listbox).getByRole("option", { name: "Assistant" }));

    fireEvent.click(screen.getByTestId("composer-more-button"));
    expect(
      screen.getByRole("menuitem", { name: "Persona — Assistant" }),
    ).not.toBeDisabled();
  });
});
