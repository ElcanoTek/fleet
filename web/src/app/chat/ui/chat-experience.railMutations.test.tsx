import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { clearChatSession } from "./chatSessionStore";
import { RECENT_SORT_ITEM } from "./conversationOrganization";
import { withoutHistoryWindow } from "./historyWindow.testing";

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
    const url = withoutHistoryWindow(String(input));
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

// The Chats heading's menu (sort, select, tidy up) and one of its items.
async function pickChatsMenuItem(testId: string) {
  fireEvent.click(screen.getByTestId("recent-sort"));
  fireEvent.click(await screen.findByTestId(testId));
}

// A chat's row in the rail (its primary button).
const railRow = (id: string) =>
  document.querySelector<HTMLElement>(`[data-conversation-id="${id}"] [data-rail-row]`)!;

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

    await pickRowAction("Beta chat", "Delete…");
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

    await pickRowAction("Beta chat", "Delete…");
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

    await pickRowAction("Beta chat", "Delete…");
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

describe("deleting the active chat when the next one fails to load", () => {
  it("still closes the confirm and drops the row, and says the load failed rather than the delete", async () => {
    // mockBackend answers GET /api/conversations/conv-a only, so once the
    // active Alpha chat is deleted the fallback load of Beta 404s and
    // loadConversation throws. The DELETE itself succeeded.
    mockBackend(() => new Response(JSON.stringify({}), { status: 200 }));
    await mountChat();

    await pickRowAction("Alpha chat", "Delete…");
    await screen.findByRole("dialog", { name: "Delete chat?" });
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));

    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Delete chat?" })).not.toBeInTheDocument(),
    );
    expect(
      screen.queryByRole("button", { name: "Conversation options for Alpha chat" }),
    ).not.toBeInTheDocument();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Deleted the chat, but couldn't open the next one.",
    );
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
    // The public link lives in its own collapsed row below the team block.
    fireEvent.click(within(dialog).getByRole("button", { name: /Share outside your team/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create public link" }));

    // The dialog carries its own failure line…
    const alert = await within(dialog).findByRole("alert");
    expect(alert).toHaveTextContent("Couldn't create the link (HTTP 500).");
    // …and the viewport-level rail toast stays quiet: a share failure is not a
    // rail failure, and the two used to share one state.
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(within(dialog).getByRole("button", { name: "Create public link" })).toBeEnabled();
  });
});

describe("bulk-pinning chats against a failing backend", () => {
  // patchConversationIds used to throw on a non-2xx from a `void` call site,
  // so a refused pin or label was an unhandled rejection: nothing said why
  // and the user could not tell it had failed (#1998).
  // Select mode starts from the Chats menu with nothing selected; the rows
  // then tick on a click.
  async function bulkPinBeta() {
    await pickChatsMenuItem("chats-menu-select");
    const pin = await screen.findByRole("button", { name: "Pin selected" });
    expect(pin).toBeDisabled();
    fireEvent.click(railRow("conv-b"));
    expect(railRow("conv-b")).toHaveAttribute("aria-pressed", "true");
    expect(railRow("conv-a")).toHaveAttribute("aria-pressed", "false");
    fireEvent.click(pin);
  }

  it("says why on a non-2xx and leaves the chat unpinned", async () => {
    const fetchMock = mockBackend((url) =>
      url === "/api/conversations/bulk" ? new Response("nope", { status: 500 }) : new Response("{}"),
    );
    await mountChat();

    await bulkPinBeta();

    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't update the chat (HTTP 500).");
    expect(
      fetchMock.mock.calls.filter(([url, init]) => url === "/api/conversations/bulk" && init?.method === "PATCH"),
    ).toHaveLength(1);
    // No local state moved: there is still nothing pinned.
    expect(screen.queryByText("Pinned")).not.toBeInTheDocument();
  });

  it("reports a network failure the same way", async () => {
    mockBackend((url) =>
      url === "/api/conversations/bulk" ? new TypeError("Failed to fetch") : new Response("{}"),
    );
    await mountChat();

    await bulkPinBeta();

    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't update the chat — network error.");
  });

  it("pins the chat when the server agrees", async () => {
    mockBackend(() => new Response("{}", { status: 200 }));
    await mountChat();

    await bulkPinBeta();

    // The rail grows its Pinned section once the server has agreed.
    expect(await screen.findByText("Pinned")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("the Chats menu, from the full page", () => {
  it("leaves select mode on Exit selection, and gives the menu back", async () => {
    mockBackend(() => new Response("{}", { status: 200 }));
    await mountChat();

    await pickChatsMenuItem("chats-menu-select");
    // While selecting, the rows are checkboxes, with no ⋯ and no Chats menu.
    expect(await screen.findByRole("button", { name: "Exit selection" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Conversation options for Beta chat" })).not.toBeInTheDocument();
    expect(screen.queryByTestId("recent-sort")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Exit selection" }));
    await waitFor(() => expect(screen.getByTestId("recent-sort")).toBeInTheDocument());
    expect(railRow("conv-b")).not.toHaveAttribute("aria-pressed");
  });

  it("asks before deleting the temporary chats, and says what is kept", async () => {
    const fetchMock = mockBackend(() => new Response("{}", { status: 200 }));
    await mountChat();

    fireEvent.click(screen.getByTestId("recent-sort"));
    const item = await screen.findByTestId("delete-temporary-chats");
    expect(item).toHaveTextContent("Delete temporary chats…");
    expect(item).toHaveTextContent("Keeps pinned, filed, shared and patrol chats.");
    fireEvent.click(item);

    expect(await screen.findByRole("dialog", { name: "Delete all temporary chats?" })).toBeInTheDocument();
    // Nothing is deleted by opening the question.
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE")).toHaveLength(0);
  });

  it("sorts by title from the menu, where the date groups step aside, and remembers it", async () => {
    mockBackend(() => new Response("{}", { status: 200 }));
    try {
      await mountChat();
      const sort = () => screen.getByTestId("recent-sort");
      const groups = () =>
        Array.from(document.querySelectorAll('[data-testid="unified-chat-list"] [data-date-group]')).map((g) =>
          g.getAttribute("aria-label"),
        );
      // Most recently active first, under the date each was last active.
      expect(sort()).toHaveAttribute("data-value", "activity");
      expect(groups()).toEqual(["Older"]);

      await pickChatsMenuItem("recent-sort-title");
      await waitFor(() => expect(sort()).toHaveAttribute("data-value", "title"));
      expect(sort()).toHaveAccessibleName("Chat list options. Sorted by: Title A–Z");
      // An A–Z list has no dates to group by.
      expect(groups()).toEqual([]);
      expect(window.localStorage.getItem(RECENT_SORT_ITEM)).toBe("title");
    } finally {
      window.localStorage.removeItem(RECENT_SORT_ITEM);
    }
  });
});
