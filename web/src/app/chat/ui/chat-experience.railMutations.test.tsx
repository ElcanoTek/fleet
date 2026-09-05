import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";

// Full-mount coverage for the rail's delete / rename / archive mutations
// against a backend that says no. Each used to fail silently: the confirm
// dialog closed and then `await`ed a function that threw on a non-2xx, so the
// failure was an unhandled rejection — the chat stayed in the rail, nothing
// said why, and (for the header rename) the title input stayed disabled for
// good. Now every path reports through the rail toast (role="alert") and
// leaves the user's context intact.

const CONVS: ConversationSummary[] = [
  {
    id: "conv-a",
    title: "Alpha chat",
    persona: "default",
    model: "vendor/one",
    pinned: false,
    updated_at: 1767225600,
  },
  {
    id: "conv-b",
    title: "Beta chat",
    persona: "default",
    model: "vendor/one",
    pinned: false,
    updated_at: 1767225500,
  },
];

type Reply = Response | Error;

// mockBackend answers the cold-boot reads ChatExperience needs (the two
// conversation lists, the active conversation, its inflight probe) and 404s
// every other nice-to-have loader, which they all tolerate. Writes go to
// `onWrite`, which decides the reply — a Response to return, or an Error to
// throw as a network failure.
function mockBackend(onWrite: (url: string, init: RequestInit) => Reply) {
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status });
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method && init.method !== "GET") {
      const reply = onWrite(url, init);
      if (reply instanceof Error) throw reply;
      return reply;
    }
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-a")
      return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/conversations/conv-a/inflight") return json({ inflight: false });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

// The header's click-to-rename title; the rail row is also a button named
// after the chat, so the header is found by its own affordance.
const headerTitle = () => screen.getByTitle("Click to rename");

async function mountChat() {
  render(<ChatExperience initialUserEmail="user@example.com" />);
  // Bootstrap done once the active chat's title is in the header.
  await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
}

// Opens the rail row's kebab and picks one of its items.
async function pickRowAction(title: string, item: string) {
  fireEvent.click(screen.getByRole("button", { name: `Conversation options for ${title}` }));
  fireEvent.click(await screen.findByRole("menuitem", { name: item }));
}

beforeEach(() => {
  // jsdom has neither; the rail collapse hook and the transcript read the
  // former, and the transcript scrolls with the latter.
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
  // The mirror effect persists a warm snapshot once bootstrapped; the next
  // test must cold-start.
  clearChatSession();
});

describe("deleting a chat against a failing backend", () => {
  it("keeps the confirm open, keeps the chat, and says why on a non-2xx", async () => {
    const fetchMock = mockBackend(() => new Response("db locked", { status: 500 }));
    await mountChat();

    await pickRowAction("Beta chat", "Delete");
    const dialog = await screen.findByRole("dialog", { name: "Delete chat?" });
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    const toast = await screen.findByRole("alert");
    expect(toast).toHaveTextContent("Couldn't delete the chat (HTTP 500).");
    // The dialog is still up with Delete re-enabled to retry, and the chat is
    // still in the rail — no local state moved before the server answered.
    expect(dialog).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Conversation options for Beta chat" })).toBeInTheDocument();
    expect(
      fetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE"),
    ).toHaveLength(1);
  });

  it("reports a network failure the same way", async () => {
    mockBackend(() => new TypeError("Failed to fetch"));
    await mountChat();

    await pickRowAction("Beta chat", "Delete");
    await screen.findByRole("dialog", { name: "Delete chat?" });
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't delete the chat — network error.",
    );
    expect(screen.getByRole("dialog", { name: "Delete chat?" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Conversation options for Beta chat" })).toBeInTheDocument();
  });

  it("closes the confirm and drops the row when the server agrees", async () => {
    mockBackend(() => new Response(JSON.stringify({}), { status: 200 }));
    await mountChat();

    await pickRowAction("Beta chat", "Delete");
    await screen.findByRole("dialog", { name: "Delete chat?" });
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Delete chat?" })).not.toBeInTheDocument(),
    );
    expect(
      screen.queryByRole("button", { name: "Conversation options for Beta chat" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("renaming the active chat from the header against a failing backend", () => {
  it("re-enables the input, restores the title, and says why", async () => {
    mockBackend(() => new TypeError("Failed to fetch"));
    await mountChat();

    fireEvent.click(headerTitle());
    const input = await screen.findByRole("textbox", { name: "Rename chat" });
    fireEvent.change(input, { target: { value: "Renamed chat" } });
    fireEvent.blur(input);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't rename the chat — network error.",
    );
    // The optimistic title rolled back on the header and in the rail, and the
    // rename input is gone rather than stuck disabled.
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
    expect(screen.queryByRole("textbox", { name: "Rename chat" })).not.toBeInTheDocument();
    expect(screen.queryByText("Renamed chat")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Conversation options for Alpha chat" })).toBeInTheDocument();
  });

  it("reports the status of a non-2xx and rolls back", async () => {
    mockBackend(() => new Response("", { status: 409 }));
    await mountChat();

    fireEvent.click(headerTitle());
    const input = await screen.findByRole("textbox", { name: "Rename chat" });
    fireEvent.change(input, { target: { value: "Renamed chat" } });
    fireEvent.blur(input);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't rename the chat (HTTP 409).",
    );
    await waitFor(() => expect(headerTitle()).toHaveTextContent("Alpha chat"));
  });
});

describe("archiving a chat against a failing backend", () => {
  it("moves the row back to the active list and says why", async () => {
    mockBackend((url) =>
      url.endsWith("/archive") ? new TypeError("Failed to fetch") : new Response("{}"),
    );
    await mountChat();

    await pickRowAction("Beta chat", "Archive");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't archive the chat — network error.",
    );
    // The optimistic hop rolled back: the row is an ordinary active row again
    // (its kebab is back) and the Archived section has nothing to show.
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Conversation options for Beta chat" }),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByRole("button", { name: /Archived conversations/ })).not.toBeInTheDocument();
  });
});

describe("creating a share link against a failing backend", () => {
  it("reports inside the Share dialog, not through the rail toast", async () => {
    mockBackend((url) =>
      url.endsWith("/share") ? new Response("nope", { status: 500 }) : new Response("{}", { status: 200 }),
    );
    await mountChat();

    await pickRowAction("Beta chat", "Share…");
    const dialog = await screen.findByRole("dialog", { name: "Share this chat" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create link" }));

    // The dialog carries its own failure line…
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't create the link (HTTP 500).");
    // …and the viewport-level rail toast stays quiet: a share failure is not a
    // rail failure, and the two used to share one state.
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(within(dialog).getByRole("button", { name: "Create link" })).toBeEnabled();
  });
});
