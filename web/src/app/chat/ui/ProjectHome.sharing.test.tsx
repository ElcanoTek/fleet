import { afterEach, describe, expect, it, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { ProjectHome } from "./ProjectHome";
import { ChatToastProvider } from "./ChatToasts";
import { buildGettingStarted, type GettingStartedInput } from "./ProjectGettingStarted";
import type { Project } from "./ProjectsModal";
import type { ConversationSummary } from "./chat-experience";
import type { SourcesGroup } from "./teamSharing";

// Fleet Projects sharing on the project home: the header pill, the
// getting-started card's four variants, "Share project first", the row pill's
// quick switch, New chat's menu, "Shared by your team", and Sources grouped by
// chat. The team's name always comes from data ("quant" here), never copy.

const SHARED: Project = {
  id: "p1",
  owner_email: "alice@x.com",
  name: "Quant",
  instructions: "",
  team_id: "quant",
  mcp_servers: [],
  created_at: 1767225600,
  updated_at: 1767225600,
};
const PERSONAL: Project = { ...SHARED, team_id: undefined };

const chat = (id: string, title: string, over: Partial<ConversationSummary> = {}) =>
  ({
    id,
    title,
    persona: "p",
    model: "m",
    pinned: false,
    updated_at: 1767225600,
    project_id: "p1",
    ...over,
  }) as ConversationSummary;

type Call = { url: string; method: string; body?: unknown };

function mockApi(routes: Record<string, unknown>, writes: Record<string, unknown> = {}) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      const method = init?.method ?? "GET";
      calls.push({
        url,
        method,
        body: init?.body ? JSON.parse(String(init.body)) : undefined,
      });
      const table = method === "GET" ? routes : writes;
      for (const [suffix, body] of Object.entries(table)) {
        if (url.endsWith(suffix)) {
          return new Response(JSON.stringify(body), { status: 200 });
        }
      }
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
  return calls;
}

const FRESH = { "/my-state": { kept_personal: false, has_shared_chat: false, sources_open: {} } };
const DONE = { "/my-state": { kept_personal: false, has_shared_chat: true, sources_open: {} } };

function renderHome(over: Partial<Parameters<typeof ProjectHome>[0]> = {}) {
  const props = {
    project: SHARED,
    chats: [chat("c1", "Spread study"), chat("c2", "Vol surface")],
    userEmail: "alice@x.com",
    isOwner: true,
    onBack: vi.fn(),
    onOpenChat: vi.fn(),
    onOpenTeamChat: vi.fn(),
    onNewChat: vi.fn(),
    onSaveInstructions: vi.fn(async () => true),
    onUpdateSettings: vi.fn(async () => true),
    onOpenSettings: vi.fn(),
    onOpenShareDialog: vi.fn(),
    onChatsChanged: vi.fn(),
    myTeam: "quant",
    ...over,
  };
  const utils = render(
    <ChatToastProvider>
      <ProjectHome {...props} />
    </ChatToastProvider>,
  );
  return { props, ...utils };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// ── Header ──────────────────────────────────────────────────────────────────

describe("project home header pill", () => {
  it("says Only you for a personal project", async () => {
    mockApi(DONE);
    renderHome({ project: PERSONAL });
    expect(await screen.findByTestId("project-visibility-pill")).toHaveTextContent("Only you");
  });

  it("opens the spec's popover for a shared project, with settings for owners", async () => {
    mockApi(DONE);
    const { props } = renderHome();
    fireEvent.click(await screen.findByRole("button", { name: /Shared with quant/ }));
    const pop = screen.getByRole("dialog", { name: "Who sees this project" });
    expect(pop).toHaveTextContent(
      "quant sees the instructions, Team learnings, and any chat shared with them.",
    );
    expect(pop).toHaveTextContent(
      "Each chat stays Only you until its owner shares it. Its files go with it.",
    );
    expect(pop).not.toHaveTextContent("one at a time");
    expect(pop).toHaveTextContent(
      "Chats here use the instructions, then Team learnings, then each person’s own memory. They don’t expire.",
    );
    // Settings are one surface (ProjectSettingsDialog), rendered by the
    // parent: the pill's link asks for it rather than opening its own copy.
    expect(props.onOpenSettings).not.toHaveBeenCalled();
    fireEvent.click(within(pop).getByRole("button", { name: "Project settings" }));
    expect(props.onOpenSettings).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("dialog", { name: "Settings for Quant" })).toBeNull();
  });

  it("offers no settings link to a member", async () => {
    mockApi(DONE);
    renderHome({ isOwner: false });
    fireEvent.click(await screen.findByRole("button", { name: /Shared with quant/ }));
    const pop = screen.getByRole("dialog", { name: "Who sees this project" });
    expect(within(pop).queryByRole("button", { name: "Project settings" })).toBeNull();
  });
});

// ── Getting-started card ────────────────────────────────────────────────────

describe("getting-started variants", () => {
  const base: GettingStartedInput = {
    projectName: "Bento",
    projectTeam: "elc",
    myTeam: "elc",
    isOwner: true,
    hasInstructions: false,
    myChatCount: 2,
    myPrivateChatCount: 2,
  };

  it("B6: owner, shared project", () => {
    const m = buildGettingStarted(base);
    expect(m.title).toBe("Get elc working here");
    expect(m.lead).toBe("Chats start as Only you. Share the ones elc should read and build on.");
    expect(m.steps.map((s) => s.label)).toEqual([
      "Project shared with elc",
      "Add instructions",
      "Share a chat with elc",
    ]);
    expect(m.steps[0].done).toBe(true);
    expect(m.steps[1].action?.label).toBe("Add");
    expect(m.steps[2].sub).toBe("Switch any chat below from Only you to elc.");
    expect(m.canKeepPersonal).toBe(false);
  });

  it("B15: step 3 names the chat from Share project first, with Share it", () => {
    const m = buildGettingStarted({ ...base, readyChat: { id: "c1", title: "Style guide" } });
    expect(m.steps[2].sub).toBe("“Style guide” is ready to share.");
    expect(m.steps[2].action).toMatchObject({ label: "Share it", kind: "share-ready-chat" });
  });

  it("B8: member, shared project", () => {
    const m = buildGettingStarted({ ...base, isOwner: false, myChatCount: 0, myPrivateChatCount: 0 });
    expect(m.title).toBe("Get started in Bento");
    expect(m.steps.map((s) => s.label)).toEqual([
      "Start a chat here, or branch one",
      "Share it with elc",
    ]);
    expect(m.steps[0].action?.label).toBe("New chat");
    expect(m.steps[1].sub).toBe("Available after step 1");
  });

  it("B12: owner, personal project, with Keep personal", () => {
    const m = buildGettingStarted({ ...base, projectTeam: "" });
    expect(m.title).toBe("Work on this with elc");
    expect(m.steps.map((s) => s.label)).toEqual([
      "Share Bento with elc",
      "Add instructions",
      "Share a chat with elc",
    ]);
    expect(m.steps[0].action).toMatchObject({ label: "Share project", kind: "share-project" });
    expect(m.steps[2].sub).toBe("Available after step 1");
    expect(m.canKeepPersonal).toBe(true);
  });

  it("B25: no team — guidance and no share step", () => {
    const m = buildGettingStarted({ ...base, projectTeam: "", myTeam: "", myChatCount: 0 });
    expect(m.title).toBe("Get started in Bento");
    expect(m.lead).toMatch(/^Sharing needs a team\. Admins: add yourself in Settings → Admin → Users/);
    expect(m.steps.map((s) => s.label)).toEqual(["Add instructions", "Start a chat"]);
    expect(m.canKeepPersonal).toBe(false);
  });

  it("ticks steps off from live data", () => {
    const m = buildGettingStarted({ ...base, hasInstructions: true });
    expect(m.steps[1].done).toBe(true);
    expect(m.steps[1].action).toBeUndefined();
  });
});

describe("getting-started card on the page", () => {
  it("shows until the person has shared a chat here, and X hides it for now", async () => {
    mockApi(FRESH);
    renderHome();
    const card = await screen.findByTestId("getting-started");
    expect(card).toHaveTextContent("GET STARTED");
    expect(card).toHaveTextContent("1 of 3");
    fireEvent.click(within(card).getByRole("button", { name: "Dismiss getting started" }));
    expect(screen.queryByTestId("getting-started")).toBeNull();
  });

  it("stays away once my-state says the person shared a chat", async () => {
    mockApi(DONE);
    renderHome();
    await screen.findByText("Spread study");
    await waitFor(() => expect(screen.queryByTestId("getting-started")).toBeNull());
  });

  it("Keep personal remembers the choice for this project", async () => {
    const calls = mockApi(FRESH, {
      "/my-state": { kept_personal: true, has_shared_chat: false, sources_open: {} },
    });
    renderHome({ project: PERSONAL });
    const card = await screen.findByTestId("getting-started");
    fireEvent.click(within(card).getByRole("button", { name: "Keep personal" }));
    expect(screen.queryByTestId("getting-started")).toBeNull();
    await waitFor(() =>
      expect(calls.find((c) => c.method === "PUT" && c.url.endsWith("/my-state"))?.body).toEqual({
        kept_personal: true,
      }),
    );
    expect(await screen.findByTestId("chat-toast")).toHaveTextContent(
      "Got it. Quant stays personal, and the card won’t come back.",
    );
  });

  it("Share project shares the project in one click", async () => {
    mockApi(FRESH);
    const { props } = renderHome({ project: PERSONAL });
    const card = await screen.findByTestId("getting-started");
    fireEvent.click(within(card).getByRole("button", { name: "Share project" }));
    await waitFor(() =>
      expect(props.onUpdateSettings).toHaveBeenCalledWith({ team_shared: true }),
    );
    expect(await screen.findByTestId("chat-toast")).toHaveTextContent(
      "Quant is shared with quant. Chats stay Only you.",
    );
  });
});

// ── Share project first (B14 / B15) ─────────────────────────────────────────

describe("Share project first", () => {
  const shareFirst = { conversationId: "c1", title: "Spread study" };

  it("opens with the confirm up; Cancel changes nothing", async () => {
    mockApi(FRESH);
    const { props } = renderHome({ project: PERSONAL, shareFirst });
    const dialog = await screen.findByRole("dialog", { name: "Share Quant with quant?" });
    expect(dialog).toHaveTextContent(
      "quant will see Quant’s instructions and Team learnings. Each chat stays Only you until you share it.",
    );
    expect(dialog).toHaveTextContent("Next, you can share “Spread study”.");
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog", { name: "Share Quant with quant?" })).toBeNull();
    expect(props.onUpdateSettings).not.toHaveBeenCalled();
  });

  it("confirming shares the project, then the card names the chat and Share it shares it", async () => {
    const calls = mockApi(FRESH, {
      "/share-with-team": { team_visible: true, shared_files: 2, total_files: 3 },
    });
    const { props, rerender } = renderHome({ project: PERSONAL, shareFirst });
    const dialog = await screen.findByRole("dialog", { name: "Share Quant with quant?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Share with quant" }));
    await waitFor(() =>
      expect(props.onUpdateSettings).toHaveBeenCalledWith({ team_shared: true }),
    );
    expect(await screen.findByTestId("chat-toast")).toHaveTextContent(
      "Quant is shared with quant. Now share your chat from the card.",
    );

    // The parent's project list refreshes with the team on it.
    rerender(
      <ChatToastProvider>
        <ProjectHome {...props} project={SHARED} shareFirst={shareFirst} />
      </ChatToastProvider>,
    );
    const card = await screen.findByTestId("getting-started");
    expect(card).toHaveTextContent("“Spread study” is ready to share.");
    fireEvent.click(within(card).getByRole("button", { name: "Share it" }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.url.endsWith("/conversations/c1/share-with-team"))?.body,
      ).toEqual({ visible: true }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("chat-toast")).toHaveTextContent(
        "“Spread study” is shared with quant, with 2 files.",
      ),
    );
    expect(screen.getByRole("button", { name: "Manage" })).toBeInTheDocument();
  });
});

// ── Your chats ──────────────────────────────────────────────────────────────

describe("Your chats", () => {
  it("New chat offers Only you / Shared with <team>, and shares the shared one after creating it", async () => {
    const calls = mockApi(DONE, {
      "/share-with-team": { team_visible: true, shared_files: 0, total_files: 0 },
    });
    const { props } = renderHome();
    fireEvent.click(await screen.findByRole("button", { name: /^New chat/ }));
    const menu = screen.getByRole("menu", { name: "New chat" });
    expect(menu).toHaveTextContent("Only youPrivate until you share it");
    expect(menu).toHaveTextContent("Shared with quantquant can read it and branch it");
    fireEvent.click(within(menu).getByRole("menuitem", { name: /Shared with quant/ }));
    expect(props.onNewChat).toHaveBeenCalledTimes(1);
    const afterCreate = (props.onNewChat as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(typeof afterCreate).toBe("function");
    await act(async () => {
      await afterCreate("new1");
    });
    expect(
      calls.find((c) => c.url.endsWith("/conversations/new1/share-with-team"))?.body,
    ).toEqual({ visible: true });
    expect(
      await screen.findByText("New chat shared with quant. Files it creates will be shared too."),
    ).toBeInTheDocument();
  });

  it("has no New chat menu without a team", async () => {
    mockApi(DONE);
    const { props } = renderHome({ project: PERSONAL, myTeam: "" });
    fireEvent.click(await screen.findByRole("button", { name: /^New chat/ }));
    expect(screen.queryByRole("menu")).toBeNull();
    expect(props.onNewChat).toHaveBeenCalledWith();
  });

  it("the row pill switches a chat to the team and confirms the files", async () => {
    const calls = mockApi(DONE, {
      "/share-with-team": { team_visible: true, shared_files: 3, total_files: 4 },
    });
    const { props } = renderHome();
    const row = (await screen.findByText("Spread study")).closest(
      "[data-testid=project-chat-row]",
    ) as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "Who can see this chat: Only you" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /^quant/ }));
    await waitFor(() =>
      expect(screen.getByTestId("chat-toast")).toHaveTextContent(
        "“Spread study” is shared with quant, with 3 files.",
      ),
    );
    expect(
      calls.find((c) => c.url.endsWith("/conversations/c1/share-with-team"))?.body,
    ).toEqual({ visible: true });
    expect(props.onChatsChanged).toHaveBeenCalled();
  });

  it("switching back to Only you asks first, with the shared-file count", async () => {
    const calls = mockApi(
      { ...DONE, "/conversations/c1/outputs": { outputs: [], total: 4, shared_count: 3, team_visible: true } },
      { "/share-with-team": { team_visible: false, shared_files: 3, total_files: 4 } },
    );
    renderHome({ chats: [chat("c1", "Spread study", { team_visible: true })] });
    fireEvent.click(await screen.findByRole("button", { name: "Who can see this chat: quant" }));
    fireEvent.click(screen.getByRole("menuitem", { name: /^Only you/ }));
    const confirm = await screen.findByTestId("row-unshare-confirm");
    await waitFor(() =>
      expect(confirm).toHaveTextContent("3 shared files will stop being shared too."),
    );
    expect(calls.some((c) => c.method === "POST")).toBe(false);
    fireEvent.click(within(confirm).getByRole("button", { name: "Stop sharing" }));
    await waitFor(() =>
      expect(
        calls.find((c) => c.url.endsWith("/conversations/c1/share-with-team"))?.body,
      ).toEqual({ visible: false }),
    );
    await waitFor(() =>
      expect(screen.getByTestId("chat-toast")).toHaveTextContent(
        "“Spread study” is Only you again, and 3 shared files stopped being shared.",
      ),
    );
  });

  it("More sharing options opens the share dialog", async () => {
    mockApi(DONE);
    const { props } = renderHome();
    const row = (await screen.findByText("Vol surface")).closest(
      "[data-testid=project-chat-row]",
    ) as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: /Who can see this chat/ }));
    fireEvent.click(screen.getByRole("menuitem", { name: /More sharing options/ }));
    expect(props.onOpenShareDialog).toHaveBeenCalledWith("c2");
  });
});

// ── Shared by your team ─────────────────────────────────────────────────────

describe("Shared by your team", () => {
  it("marks chats the viewer branched, and always opens the owner's chat", async () => {
    mockApi({
      ...DONE,
      "/team-conversations": {
        conversations: [
          {
            id: "t1",
            title: "Basis trade",
            user_email: "bob@x.com",
            updated_at: 1,
            viewer_branch: { conversation_id: "mine1", branched_at: 1, changed_since: false },
          },
        ],
      },
    });
    const { props } = renderHome();
    const row = (await screen.findByText("Basis trade")).closest("button") as HTMLElement;
    expect(row).toHaveTextContent("You branched this");
    fireEvent.click(row);
    expect(props.onOpenTeamChat).toHaveBeenCalledWith("t1");
  });
});

// ── Sources ─────────────────────────────────────────────────────────────────

const GROUPS: SourcesGroup[] = [
  {
    conversation_id: "c1",
    title: "Spread study",
    owner_email: "alice@x.com",
    mine: true,
    team_visible: true,
    is_branch: false,
    last_active_at: 300,
    file_count: 2,
    shared_count: 1,
    files: [
      { path: "out/a.csv", name: "a.csv", size: 2048, modified_at: 100, shared: true, output: true, your_copy: false },
      { path: "out/b.csv", name: "b.csv", size: 10, modified_at: 200, shared: false, output: true, your_copy: false },
      { path: "scratch.txt", name: "scratch.txt", size: 1, modified_at: 50, shared: true, output: false, your_copy: false },
    ],
  },
  {
    conversation_id: "c2",
    title: "Vol surface",
    owner_email: "alice@x.com",
    mine: true,
    team_visible: false,
    is_branch: false,
    last_active_at: 100,
    file_count: 1,
    shared_count: 1,
    files: [
      { path: "v.json", name: "v.json", size: 10, modified_at: 10, shared: true, output: true, your_copy: false },
    ],
  },
  {
    conversation_id: "t1",
    title: "Basis trade",
    owner_email: "bob@x.com",
    mine: false,
    team_visible: true,
    is_branch: false,
    last_active_at: 200,
    file_count: 1,
    shared_count: 1,
    files: [
      { path: "x.xlsx", name: "x.xlsx", size: 10, modified_at: 10, shared: true, output: true, your_copy: false },
    ],
  },
  {
    conversation_id: "br1",
    title: "Basis trade (branch)",
    owner_email: "alice@x.com",
    mine: true,
    team_visible: false,
    is_branch: true,
    branched_at: 1767225600,
    last_active_at: 50,
    file_count: 1,
    shared_count: 1,
    files: [
      { path: "x.xlsx", name: "x.xlsx", size: 10, modified_at: 10, shared: true, output: true, your_copy: true },
    ],
  },
];

describe("Sources", () => {
  const groupEl = (id: string) =>
    document.querySelector(`[data-testid=sources-group][data-conversation-id="${id}"]`) as HTMLElement;
  const findGroup = (id: string) =>
    waitFor(() => {
      const g = groupEl(id);
      expect(g).not.toBeNull();
      return g;
    });

  it("groups by chat: From your team first, then Your chats; the most recent group is open", async () => {
    mockApi({ ...DONE, "/files": { groups: GROUPS, truncated: false } });
    renderHome();
    const panel = await screen.findByTestId("project-sources");
    await within(panel).findByText("From your team");
    const order = [...panel.querySelectorAll("[data-testid=sources-group]")].map((g) =>
      g.getAttribute("data-conversation-id"),
    );
    expect(order).toEqual(["t1", "c1", "c2", "br1"]);
    expect(within(panel).getByText("Your chats")).toBeInTheDocument();
    expect(groupEl("t1")).toHaveTextContent("Shared by bob@x.com");
    expect(groupEl("c1")).toHaveTextContent("2 files · 1 shared");
    expect(groupEl("c2")).toHaveTextContent("Only you");
    // Only the most recently active group is open by default.
    expect(within(groupEl("c1")).getByRole("button", { expanded: true })).toBeInTheDocument();
    expect(within(groupEl("c2")).getByRole("button", { expanded: false })).toBeInTheDocument();
  });

  it("lists newest first, marks shared files, and toggles only outputs of a shared chat", async () => {
    const calls = mockApi(
      { ...DONE, "/files": { groups: GROUPS, truncated: false } },
      {
        "/outputs/share": {
          outputs: [
            { path: "out/a.csv", name: "a.csv", size: 1, modified_at: 1, shared: true },
            { path: "out/b.csv", name: "b.csv", size: 1, modified_at: 1, shared: true },
          ],
          total: 2,
          shared_count: 2,
          team_visible: true,
        },
      },
    );
    renderHome();
    await screen.findByText("b.csv");
    const files = within(groupEl("c1")).getAllByTestId("sources-file");
    expect(files.map((f) => f.textContent?.split(/\d/)[0])).toEqual(["b.csv", "a.csv", "scratch.txt"]);
    expect(files[1]).toHaveTextContent("· Shared");
    expect(files[0]).not.toHaveTextContent("· Shared");
    // Non-outputs are download-only.
    expect(within(files[2]).queryByRole("button", { pressed: true })).toBeNull();
    expect(within(files[2]).queryByRole("button", { pressed: false })).toBeNull();

    fireEvent.click(within(files[0]).getByRole("button", { name: /b\.csv is only you/ }));
    await waitFor(() =>
      expect(calls.find((c) => c.url.endsWith("/conversations/c1/outputs/share"))?.body).toEqual({
        path: "out/b.csv",
        shared: true,
      }),
    );
    await waitFor(() => expect(groupEl("c1")).toHaveTextContent("2 files · 2 shared"));
  });

  it("a private chat's group is download-only, with the note", async () => {
    mockApi({
      "/my-state": { kept_personal: false, has_shared_chat: true, sources_open: { c2: true } },
      "/files": { groups: GROUPS, truncated: false },
    });
    renderHome();
    await waitFor(() => expect(within(groupEl("c2")).getByText("v.json")).toBeInTheDocument());
    expect(groupEl("c2")).toHaveTextContent("Share this chat and its files go with it.");
    expect(within(groupEl("c2")).queryByRole("button", { pressed: true })).toBeNull();
    expect(within(groupEl("c2")).getByRole("button", { name: "Download v.json" })).toBeInTheDocument();
  });

  it("labels branch copies Your copy", async () => {
    mockApi({
      "/my-state": { kept_personal: false, has_shared_chat: true, sources_open: { br1: true } },
      "/files": { groups: GROUPS, truncated: false },
    });
    renderHome();
    await waitFor(() => expect(groupEl("br1")).toHaveTextContent("Your copy"));
  });

  it("remembers a group opened or closed, per person per project", async () => {
    const calls = mockApi({ ...DONE, "/files": { groups: GROUPS, truncated: false } });
    renderHome();
    await screen.findByText("b.csv");
    fireEvent.click(within(groupEl("c2")).getByRole("button", { expanded: false }));
    expect(await within(groupEl("c2")).findByText("v.json")).toBeInTheDocument();
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === "PUT" && c.url.endsWith("/my-state"))?.body,
      ).toEqual({ sources_open: { c2: true } }),
    );
  });

  it("sends only the toggled group, never the whole cached map", async () => {
    const calls = mockApi({
      "/my-state": {
        kept_personal: false,
        has_shared_chat: true,
        sources_open: { c1: true, br1: true },
      },
      "/files": { groups: GROUPS, truncated: false },
    });
    renderHome();
    await screen.findByText("b.csv");
    fireEvent.click(within(groupEl("c2")).getByRole("button", { expanded: false }));
    expect(await within(groupEl("c2")).findByText("v.json")).toBeInTheDocument();
    await waitFor(() =>
      expect(
        calls.find((c) => c.method === "PUT" && c.url.endsWith("/my-state"))?.body,
      ).toEqual({ sources_open: { c2: true } }),
    );
    // The other remembered groups stay open locally.
    expect(within(groupEl("c1")).getByText("b.csv")).toBeInTheDocument();
  });

  it("sourcesFocus opens that chat's group", async () => {
    mockApi({ ...DONE, "/files": { groups: GROUPS, truncated: false } });
    renderHome({ sourcesFocus: "c2" });
    expect(await within(await findGroup("c2")).findByText("v.json")).toBeInTheDocument();
  });

  it("downloads a teammate's file through the team-files route", async () => {
    const calls = mockApi({ ...DONE, "/files": { groups: GROUPS, truncated: false } });
    renderHome({ sourcesFocus: "t1" });
    const btn = await within(await findGroup("t1")).findByRole("button", {
      name: "Download x.xlsx",
    });
    URL.createObjectURL = vi.fn(() => "blob:x");
    URL.revokeObjectURL = vi.fn();
    fireEvent.click(btn);
    await waitFor(() =>
      expect(calls.some((c) => c.url === "/api/conversations/t1/team-files/x.xlsx")).toBe(true),
    );
  });
});
