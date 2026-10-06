import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ChatExperience, type ConversationSummary } from "./chat-experience";
import { ChatToastProvider } from "./ChatToasts";
import { clearChatSession } from "./chatSessionStore";

// Full-mount coverage for the sharing decisions on the rail (#24–#28, #52):
// every way a SHARED chat loses its audience asks first with the shared-file
// count; moving a private chat into a shared project never shares it but says
// so, with the one-click share; New project lands on the new project's home.

const SHARED_PROJECT = {
  id: "p-team",
  owner_email: "user@example.com",
  name: "Quant",
  team_id: "Elcano",
  mcp_servers: [],
  created_at: 1,
  updated_at: 2,
};
const PERSONAL_PROJECT = {
  id: "p-mine",
  owner_email: "user@example.com",
  name: "Scratch",
  mcp_servers: [],
  created_at: 1,
  updated_at: 1,
};

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
    id: "conv-s",
    title: "Shared chat",
    persona: "default",
    model: "vendor/one",
    pinned: false,
    updated_at: 1767225500,
    project_id: "p-team",
    team_visible: true,
  },
];

function mockBackend() {
  const json = (body: unknown, status = 200) =>
    new Response(JSON.stringify(body), { status });
  const projects = [SHARED_PROJECT, PERSONAL_PROJECT];
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (method === "POST" && url === "/api/projects") {
      const body = JSON.parse(String(init?.body)) as { name: string; team_shared: boolean };
      const created = {
        ...PERSONAL_PROJECT,
        id: "p-new",
        name: body.name,
        team_id: body.team_shared ? "Elcano" : undefined,
      };
      projects.push(created);
      return json(created, 201);
    }
    if (method === "POST" && url.endsWith("/share-with-team"))
      return json({ team_visible: true, shared_files: 2, total_files: 2 });
    if (method !== "GET") return json({});
    if (url === "/api/conversations") return json({ conversations: CONVS });
    if (url === "/api/conversations?archived=true") return json({ conversations: [] });
    if (url === "/api/conversations/conv-a")
      return json({ conversation: CONVS[0], history: [] });
    if (url === "/api/conversations/conv-a/inflight") return json({ inflight: false });
    if (url === "/api/conversations/conv-s")
      return json({ conversation: CONVS[1], history: [] });
    if (url === "/api/conversations/conv-s/inflight") return json({ inflight: false });
    if (url === "/api/projects") return json({ projects });
    if (url === "/api/me/team")
      return json({ email: "user@example.com", role: "member", team_id: "Elcano", admin: false });
    if (url === "/api/conversations/conv-s/outputs")
      return json({ outputs: [], total: 3, shared_count: 3, team_visible: true });
    return json({}, 404);
  });
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

async function mountChat() {
  render(
    <ChatToastProvider>
      <ChatExperience initialUserEmail="user@example.com" />
    </ChatToastProvider>,
  );
  await waitFor(() =>
    expect(screen.getByTitle("Click to rename")).toHaveTextContent("Alpha chat"),
  );
  // Projects loaded into the rail.
  await screen.findByRole("button", { name: "Open project Quant" });
}

async function openRowMenu(title: string) {
  // A project chat sits under its (collapsed) project; reveal it first.
  if (!screen.queryByRole("button", { name: `Conversation options for ${title}` })) {
    fireEvent.click(screen.getByRole("button", { name: /^Project Quant \(/ }));
  }
  fireEvent.click(
    await screen.findByRole("button", { name: `Conversation options for ${title}` }),
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

describe("shared chats warn before they lose their audience", () => {
  it("B34: archiving a shared chat asks first, with the shared-file count", async () => {
    const fetchMock = mockBackend();
    await mountChat();
    await openRowMenu("Shared chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Archive/ }));
    const dialog = await screen.findByRole("dialog", { name: "Archive “Shared chat”?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "Archiving stops sharing this chat with Elcano, along with its 3 shared files.",
      ),
    );
    // Nothing was archived yet.
    expect(fetchMock.mock.calls.some(([u]) => String(u).endsWith("/archive"))).toBe(false);
    fireEvent.click(await screen.findByRole("button", { name: "Archive chat" }));
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(([u]) => String(u) === "/api/conversations/conv-s/archive"),
      ).toBe(true),
    );
  });

  it("archives a private chat straight away, as before", async () => {
    const fetchMock = mockBackend();
    await mountChat();
    await openRowMenu("Alpha chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Archive/ }));
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(([u]) => String(u) === "/api/conversations/conv-a/archive"),
      ).toBe(true),
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("B33: deleting a shared chat uses the team copy; a private one keeps today's", async () => {
    mockBackend();
    await mountChat();
    await openRowMenu("Shared chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Delete/ }));
    const dialog = await screen.findByRole("dialog", { name: "Delete “Shared chat”?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "The chat and its files are deleted. Elcano will lose access to this chat and its 3 shared files.",
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await openRowMenu("Alpha chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: /^Delete/ }));
    expect(await screen.findByRole("dialog", { name: "Delete chat?" })).toBeInTheDocument();
  });

  it("B35: moving a shared chat to a personal project asks first", async () => {
    mockBackend();
    await mountChat();
    await openRowMenu("Shared chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Move to project" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Scratch" }));
    const dialog = await screen.findByRole("dialog", { name: "Move to Scratch?" });
    await waitFor(() =>
      expect(dialog).toHaveTextContent(
        "Scratch isn’t shared with Elcano, so “Shared chat” will stop being shared, along with 3 shared files.",
      ),
    );
    expect(screen.getByRole("button", { name: "Move and stop sharing" })).toBeInTheDocument();
  });
});

describe("moving only moves (B11)", () => {
  it("says the chat is still Only you, and shares it from the toast", async () => {
    const fetchMock = mockBackend();
    await mountChat();
    await openRowMenu("Alpha chat");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Move to project" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "Quant" }));
    const toast = await screen.findByTestId("chat-toast");
    expect(toast).toHaveTextContent("Moved to Quant. Only you can see it.");
    // Moving never shared it.
    expect(
      fetchMock.mock.calls.some(([u]) => String(u).endsWith("/share-with-team")),
    ).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Share with Elcano" }));
    await waitFor(() =>
      expect(screen.getByTestId("chat-toast")).toHaveTextContent(
        "“Alpha chat” is shared with Elcano, with 2 files.",
      ),
    );
    expect(
      fetchMock.mock.calls.some(
        ([u]) => String(u) === "/api/conversations/conv-a/share-with-team",
      ),
    ).toBe(true);
  });
});

describe("New project (B27)", () => {
  it("opens from the rail's + and lands on the new project's home", async () => {
    const fetchMock = mockBackend();
    await mountChat();
    fireEvent.click(screen.getByRole("button", { name: "Create project" }));
    const dialog = await screen.findByRole("dialog", { name: "New project" });
    fireEvent.change(screen.getByPlaceholderText("e.g. Knowertech: Q4 planning"), {
      target: { value: "Q4 planning" },
    });
    fireEvent.click(within(dialog).getByRole("radio", { name: /Elcano/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Create project" }));
    await waitFor(() => expect(dialog).not.toBeInTheDocument());
    const post = fetchMock.mock.calls.find(
      ([u, init]) => String(u) === "/api/projects" && init?.method === "POST",
    );
    expect(JSON.parse(String(post?.[1]?.body))).toMatchObject({
      name: "Q4 planning",
      team_shared: true,
    });
    expect(await screen.findByTestId("project-home")).toBeInTheDocument();
  });
});

describe("output share markers (B17)", () => {
  it("re-reads the per-file states when the project home closes", async () => {
    const fetchMock = mockBackend();
    await mountChat();
    const outputReads = () =>
      fetchMock.mock.calls.filter(
        ([u, init]) =>
          String(u) === "/api/conversations/conv-s/outputs" && (init?.method ?? "GET") === "GET",
      ).length;

    fireEvent.click(screen.getByRole("button", { name: /^Project Quant \(/ }));
    fireEvent.click(await screen.findByText("Shared chat"));
    await waitFor(() => expect(screen.getByTitle("Click to rename")).toHaveTextContent("Shared chat"));
    await waitFor(() => expect(outputReads()).toBeGreaterThanOrEqual(1));
    const before = outputReads();

    // Sources lives on the project home; its toggles change the markers.
    fireEvent.click(screen.getByRole("button", { name: "Open project Quant" }));
    await screen.findByTestId("project-home");
    fireEvent.click(screen.getByRole("button", { name: "Back to chat" }));
    await waitFor(() => expect(screen.queryByTestId("project-home")).toBeNull());
    await waitFor(() => expect(outputReads()).toBeGreaterThan(before));
  });
});

describe("Move and share is one action (A1)", () => {
  it("puts the chat back when the share half is refused", async () => {
    const fetchMock = mockBackend();
    const base = fetchMock.getMockImplementation()!;
    fetchMock.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? "GET") === "POST" && String(input).endsWith("/share-with-team")) {
        return new Response("an archived chat can't be shared with your team; unarchive it first", {
          status: 409,
        });
      }
      return base(input, init);
    });
    await mountChat();
    fireEvent.click(screen.getByRole("button", { name: /^Share/ }));
    fireEvent.click(await screen.findByRole("button", { name: "Move and share" }));

    const dialog = await screen.findByRole("dialog");
    await waitFor(() => expect(dialog).toHaveTextContent("It was moved back where it was."));
    const moves = fetchMock.mock.calls
      .filter(([u, i]) => String(u) === "/api/conversations/conv-a/project" && i?.method === "POST")
      .map(([, i]) => JSON.parse(String(i?.body)) as { project_id: string });
    expect(moves.map((m) => m.project_id)).toEqual(["p-team", ""]);
  });
});
