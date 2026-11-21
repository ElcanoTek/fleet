import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// The rail's hand-set orders (Pinned — chats and projects together — and each
// project's chats) and the Chats list's sort and menu (docs/CHAT-ORDER.md,
// docs/SIDEBAR.md): rows follow the saved order, Reorder swaps them for the
// drag-and-arrow list, a move hands the whole new order up, and Done brings
// the ordinary rows back.

afterEach(cleanup);

const CONV: ConversationSummary = {
  id: "c-1",
  title: "Quarterly review",
  persona: "default",
  model: "test/model",
  pinned: false,
  updated_at: 1_700_000_000,
};

const PROJECT: Project = {
  id: "p-1",
  owner_email: "me@example.com",
  name: "Roadmap",
  mcp_servers: [],
  created_at: 1_700_000_000,
  updated_at: 1_700_000_000,
};

const SERVER_CONFIG: ServerConfig = {
  lockdownAvailable: false,
  lockdownOnly: false,
  lockdownAllowedModels: [],
  uploadMaxBytes: 1_000_000,
};

const noop = () => {};
const asyncNoop = async () => {};

function renderSidebar(overrides: Record<string, unknown> = {}) {
  const props = {
    sidebarOpen: true,
    setSidebarOpen: noop,
    collapse: {
      collapsed: false,
      setCollapsed: noop,
      toggle: noop,
      hydrated: true,
    },
    branding: DEFAULT_BRANDING,
    serverConfig: SERVER_CONFIG,
    userEmail: "me@example.com",
    onSignOut: noop,
    clearConversation: noop,
    sidebarQuery: "",
    setSidebarQuery: noop,
    searchRef: { current: null },
    filterLabels: [],
    setFilterLabels: noop,
    isLoadingHistory: false,
    conversations: [CONV],
    filteredConversations: [CONV],
    activeConversationId: null,
    focusedConversationId: null,
    renameSignal: null,
    loadConversation: asyncNoop,
    streamingConvs: new Set<string>(),
    togglePin: asyncNoop,
    toggleArchive: asyncNoop,
    renameConversation: async () => true,
    downloadConversation: asyncNoop,
    promoteConversation: asyncNoop,
    savePromptFromConversation: noop,
    setPendingDeleteConversation: noop,
    setConversationLabels: noop,
    openShareDialog: noop,
    archivedConversations: [],
    showArchived: false,
    setShowArchived: noop,
    setConfirmBulkDelete: noop,
    selectMode: false,
    selectedIds: new Set<string>(),
    onToggleSelection: noop,
    onEnterSelectMode: noop,
    onExitSelectMode: noop,
    onBulkDelete: noop,
    onBulkPin: noop,
    onBulkAddLabel: noop,
    searchShortcut: "Ctrl+K",
    onCreateProject: noop,
    onOpenProjectHome: noop,
    onPinProject: noop,
    onNewChatInProject: noop,
    onRenameProject: noop,
    onDeleteProject: noop,
    projects: [PROJECT],
    onMoveToProject: noop,
    ...overrides,
  } as unknown as Parameters<typeof ConversationSidebar>[0];
  const utils = render(<ConversationSidebar {...props} />);
  return {
    ...utils,
    rerenderWith: (next: Record<string, unknown>) =>
      utils.rerender(<ConversationSidebar {...({ ...props, ...next } as typeof props)} />),
  };
}

const pin = (id: string, title: string): ConversationSummary => ({ ...CONV, id, title, pinned: true });

describe("ConversationSidebar chat order", () => {
  const chats = [pin("a", "Alpha"), pin("b", "Bravo"), pin("c", "Charlie")];

  it("shows pinned chats in the saved order", () => {
    renderSidebar({ conversations: chats, filteredConversations: chats, chatOrders: { pinned: ["c", "a", "b"] }, onReorderChats: noop });
    const section = screen.getByTestId("pinned-section");
    const titles = ["Alpha", "Bravo", "Charlie"].map((t) => section.textContent!.indexOf(t));
    expect(titles[2]).toBeLessThan(titles[0]);
    expect(titles[0]).toBeLessThan(titles[1]);
  });

  it("reorders in reorder mode and returns to the rows on Done", () => {
    const onReorderChats = vi.fn();
    renderSidebar({ conversations: chats, filteredConversations: chats, chatOrders: {}, onReorderChats });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    const list = screen.getByTestId("chat-reorder-list");
    fireEvent.click(within(list).getByRole("button", { name: "Move Bravo up" }));
    expect(onReorderChats).toHaveBeenCalledWith("pinned", ["b", "a", "c"]);
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    expect(screen.queryByTestId("chat-reorder-list")).toBeNull();
    expect(screen.getByTestId("pinned-reorder-toggle")).toBeTruthy();
  });

  it("returns the focus to Reorder when the mode ends", () => {
    renderSidebar({ conversations: chats, filteredConversations: chats, onReorderChats: noop });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    expect(document.activeElement).toBe(screen.getByTestId("pinned-reorder-toggle"));
  });

  it("offers no reorder above the server's 500-chat cap", () => {
    const many = Array.from({ length: 501 }, (_, i) => pin(`p${i}`, `Chat ${i}`));
    renderSidebar({ conversations: many, filteredConversations: many, onReorderChats: noop });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
  });

  it("offers no reorder with a single pinned chat or without a save handler", () => {
    renderSidebar({ conversations: [chats[0]], filteredConversations: [chats[0]], onReorderChats: noop });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
    cleanup();
    renderSidebar({ conversations: chats, filteredConversations: chats });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
  });

  it("reorders a project's chats from the project's menu, in that project's scope", () => {
    const filed = (id: string, title: string): ConversationSummary => ({ ...CONV, id, title, project_id: "p-1" });
    const inProject = [filed("x", "Xray"), filed("y", "Yankee")];
    const onReorderChats = vi.fn();
    renderSidebar({
      conversations: inProject,
      filteredConversations: inProject,
      chatOrders: { "project:p-1": ["y", "x"] },
      onReorderChats,
    });
    fireEvent.click(screen.getByRole("button", { name: "Project options for Roadmap" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Reorder chats" }));
    const list = screen.getByRole("list", { name: "Chats in Roadmap, in order" });
    // The saved order applies: Yankee first.
    expect(within(list).getAllByRole("listitem")[0].textContent).toContain("Yankee");
    fireEvent.click(within(list).getByRole("button", { name: "Move Xray up" }));
    expect(onReorderChats).toHaveBeenCalledWith("project:p-1", ["x", "y"]);
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Project options for Roadmap" }));
  });

  it("ends reorder mode when its scope stops being orderable, and does not come back unasked", () => {
    const { rerenderWith } = renderSidebar({ conversations: chats, filteredConversations: chats, onReorderChats: noop });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    expect(screen.getByTestId("chat-reorder-list")).toBeTruthy();
    // Another tab unpins two: one pinned chat left.
    rerenderWith({ conversations: [chats[0]], filteredConversations: [chats[0]] });
    expect(screen.queryByTestId("chat-reorder-list")).toBeNull();
    // They come back: the ordinary rows return, with Reorder offered again.
    rerenderWith({ conversations: chats, filteredConversations: chats });
    expect(screen.queryByTestId("chat-reorder-list")).toBeNull();
    expect(screen.getByTestId("pinned-reorder-toggle")).toBeTruthy();
  });

  it("returns the focus to the right project's menu when two projects share a name", () => {
    const twin = { ...PROJECT, id: "p-2" };
    const filed = (id: string, title: string): ConversationSummary => ({ ...CONV, id, title, project_id: "p-2" });
    const inTwin = [filed("x", "Xray"), filed("y", "Yankee")];
    renderSidebar({ conversations: inTwin, filteredConversations: inTwin, projects: [PROJECT, twin], onReorderChats: noop });
    const triggers = screen.getAllByRole("button", { name: "Project options for Roadmap" });
    const twinTrigger = triggers.find((b) => b.getAttribute("data-project-options") === "p-2")!;
    fireEvent.click(twinTrigger);
    fireEvent.click(screen.getByRole("menuitem", { name: "Reorder chats" }));
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    expect(document.activeElement).toBe(twinTrigger);
  });

  it("does not offer Reorder chats for a project with fewer than two of the viewer's chats", () => {
    const one = [{ ...CONV, id: "x", title: "Xray", project_id: "p-1" }];
    renderSidebar({ conversations: one, filteredConversations: one, onReorderChats: noop });
    fireEvent.click(screen.getByRole("button", { name: "Project options for Roadmap" }));
    expect(screen.queryByRole("menuitem", { name: "Reorder chats" })).toBeNull();
  });

  it("sorts the Chats list by the chosen sort and reports a change", () => {
    const temp = [
      { ...CONV, id: "t1", title: "Zulu", updated_at: 30, created_at: 1 },
      { ...CONV, id: "t2", title: "Alpha", updated_at: 20, created_at: 2 },
    ];
    const onRecentSortChange = vi.fn();
    renderSidebar({ conversations: temp, filteredConversations: temp, recentSort: "title", onRecentSortChange });
    const zulu = screen.getByText("Zulu");
    const alpha = screen.getByText("Alpha");
    expect(alpha.compareDocumentPosition(zulu) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // The sort lives in the Chats menu: a small button at the heading's edge
    // naming what it is and what the list is sorted by, opening a menu whose
    // first three choices are the sorts (not a native select, which a touch
    // screen draws at twice the heading's size).
    const sort = screen.getByRole("button", { name: "Chat list options. Sorted by: Title A–Z" });
    expect(sort).toBe(screen.getByTestId("recent-sort"));
    expect(sort.getAttribute("data-value")).toBe("title");
    expect(screen.queryByRole("combobox")).toBeNull();
    fireEvent.click(sort);
    const menu = screen.getByRole("menu", { name: "Chat list options" });
    const choices = within(menu).getAllByRole("menuitemradio");
    expect(choices.map((c) => [c.textContent, c.getAttribute("aria-checked")])).toEqual([
      ["Recent activity", "false"],
      ["Newest first", "false"],
      ["Title A–Z", "true"],
    ]);
    fireEvent.click(within(menu).getByRole("menuitemradio", { name: "Newest first" }));
    expect(onRecentSortChange).toHaveBeenCalledWith("created");
    expect(screen.queryByRole("menu", { name: "Chat list options" })).toBeNull();
    // Choosing the sort already on changes nothing.
    fireEvent.click(sort);
    fireEvent.click(screen.getByRole("menuitemradio", { name: "Title A–Z" }));
    expect(onRecentSortChange).toHaveBeenCalledTimes(1);
  });

  // A teammate's chat the person pinned for themselves (docs/SIDEBAR.md).
  const TEAM = (id: string, title: string, extra: Partial<ConversationSummary> = {}): ConversationSummary => ({
    ...CONV,
    id,
    title,
    user_email: "ana@example.com",
    team_visible: true,
    ...extra,
  });

  it("lists a pinned teammate's chat in Pinned, in one saved order with the person's own", () => {
    const own = [
      { ...CONV, id: "a", title: "Alpha", pinned: true, updated_at: 30 },
      { ...CONV, id: "b", title: "Bravo", pinned: false, updated_at: 20 },
    ];
    const team = [TEAM("t1", "Team pinned", { pinned: true, updated_at: 40 }), TEAM("t2", "Team loose", { updated_at: 10 })];
    renderSidebar({
      conversations: own,
      filteredConversations: own,
      teamSharedConversations: team,
      teamSharedLoaded: true,
      chatOrders: { pinned: ["a", "t1"] },
      onReorderChats: noop,
    });
    const pinned = screen.getByTestId("pinned-section");
    const titles = (root: HTMLElement) =>
      [...root.querySelectorAll("[data-rail-row]")].map((r) => r.querySelector(".truncate")?.textContent);
    expect(titles(pinned)).toEqual(["Alpha", "Team pinned"]);
    // It is not listed a second time below, and the heading already says
    // Pinned, so the row carries no pin of its own.
    const list = screen.getByTestId("unified-chat-list");
    expect(titles(list)).toEqual(["Bravo", "Team loose"]);
    expect(within(pinned).queryByTestId("team-chat-pinned-mark")).toBeNull();
    // Two pinned chats, one of them a teammate's: the group can be reordered.
    expect(screen.getByTestId("pinned-reorder-toggle")).toBeTruthy();
  });

  it("puts a newly pinned teammate's chat first until an order names it", () => {
    const own = [{ ...CONV, id: "a", title: "Alpha", pinned: true, updated_at: 30 }];
    const team = [TEAM("t1", "Older team", { pinned: true, updated_at: 5 }), TEAM("t2", "Newer team", { pinned: true, updated_at: 50 })];
    renderSidebar({ conversations: own, filteredConversations: own, teamSharedConversations: team, teamSharedLoaded: true });
    const rows = [...screen.getByTestId("pinned-section").querySelectorAll("[data-rail-row]")].map(
      (r) => r.querySelector(".truncate")?.textContent,
    );
    expect(rows).toEqual(["Newer team", "Alpha", "Older team"]);
  });

  it("reorders the Pinned group with a teammate's chat in it, saving every ID", () => {
    const own = [{ ...CONV, id: "a", title: "Alpha", pinned: true, updated_at: 30 }];
    const team = [TEAM("t1", "Team pinned", { pinned: true, updated_at: 20 })];
    const onReorderChats = vi.fn();
    renderSidebar({
      conversations: own,
      filteredConversations: own,
      teamSharedConversations: team,
      teamSharedLoaded: true,
      onReorderChats,
    });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    // Done sits in the Pinned heading, where Reorder was.
    expect(screen.getByTestId("pinned-section").querySelectorAll('[data-testid="chat-reorder-done"]').length).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "Move Team pinned up" }));
    expect(onReorderChats).toHaveBeenCalledWith("pinned", ["t1", "a"]);
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    expect(screen.queryByTestId("chat-reorder-list")).toBeNull();
  });

  it("offers Pin and Unpin in a teammate's chat menu only to an account that can save", () => {
    const team = [TEAM("t1", "Team pinned", { pinned: true }), TEAM("t2", "Team loose")];
    const togglePin = vi.fn(async () => true);
    const { unmount } = renderSidebar({ teamSharedConversations: team, teamSharedLoaded: true, togglePin, canPinTeamChats: true });
    fireEvent.click(screen.getByRole("button", { name: "Conversation options for Team loose" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Pin" }));
    expect(togglePin).toHaveBeenLastCalledWith(team[1]);
    fireEvent.click(screen.getByRole("button", { name: "Conversation options for Team pinned" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Unpin" }));
    expect(togglePin).toHaveBeenLastCalledWith(team[0]);
    unmount();
    // Download is for a Multiplayer chat the person takes part in only.
    const downloadConversation = vi.fn(async () => {});
    const mixed = [TEAM("t3", "Live room", { multiplayer: true }), TEAM("t4", "Read only")];
    const second = renderSidebar({ teamSharedConversations: mixed, teamSharedLoaded: true, downloadConversation });
    fireEvent.click(screen.getByRole("button", { name: "Conversation options for Read only" }));
    expect(screen.queryByRole("menuitem", { name: "Download chat…" })).toBeNull();
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    fireEvent.click(screen.getByRole("button", { name: "Conversation options for Live room" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Download chat…" }));
    expect(downloadConversation).toHaveBeenCalledWith(mixed[0]);
    second.unmount();
    // A Viewer (or a role not known yet): no pin in the menu.
    renderSidebar({ teamSharedConversations: team, teamSharedLoaded: true, togglePin });
    fireEvent.click(screen.getByRole("button", { name: "Conversation options for Team loose" }));
    expect(screen.queryByRole("menuitem", { name: "Pin" })).toBeNull();
  });
  // ── Pinned holds chats AND projects, in one order ─────────────────────────
  // The saved "pinned" order names a chat by its ID and a project as
  // "project:<id>" (docs/CHAT-ORDER.md).
  describe("pinned projects and chats together", () => {
    const LAUNCH: Project = { ...PROJECT, id: "p-9", name: "Launch", pinned: true };
    const pinnedTitles = () =>
      [...screen.getByTestId("pinned-section").querySelectorAll("[data-rail-row]")].map(
        (r) => r.querySelector(".truncate")?.textContent,
      );

    it("follows the one saved order across both kinds", () => {
      renderSidebar({
        conversations: chats,
        filteredConversations: chats,
        projects: [PROJECT, LAUNCH],
        chatOrders: { pinned: ["b", "project:p-9", "a", "c"] },
        onReorderChats: noop,
      });
      expect(pinnedTitles()).toEqual(["Bravo", "Launch", "Alpha", "Charlie"]);
      // A pinned project is a project row there (it opens the project)…
      const pinned = screen.getByTestId("pinned-section");
      expect(within(pinned).getByRole("button", { name: "Open project Launch" })).toBeTruthy();
      // …and is listed once: not in Projects as well.
      const projects = screen.getByTestId("projects-section");
      expect(within(projects).queryByRole("button", { name: "Open project Launch" })).toBeNull();
      expect(within(projects).getByRole("button", { name: "Open project Roadmap" })).toBeTruthy();
    });

    it("puts what the order does not name yet first: projects, then chats", () => {
      renderSidebar({
        conversations: chats,
        filteredConversations: chats,
        projects: [LAUNCH],
        chatOrders: { pinned: ["c", "a"] },
        onReorderChats: noop,
      });
      expect(pinnedTitles()).toEqual(["Launch", "Bravo", "Charlie", "Alpha"]);
    });

    it("ignores a project the order names that is no longer pinned", () => {
      renderSidebar({
        conversations: chats,
        filteredConversations: chats,
        projects: [PROJECT],
        chatOrders: { pinned: ["project:p-1", "c", "a", "b"] },
        onReorderChats: noop,
      });
      expect(pinnedTitles()).toEqual(["Charlie", "Alpha", "Bravo"]);
    });

    it("reorders them as one list, saving a project as “project:<id>”", () => {
      const onReorderChats = vi.fn();
      renderSidebar({
        conversations: chats,
        filteredConversations: chats,
        projects: [LAUNCH],
        chatOrders: {},
        onReorderChats,
      });
      fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
      const list = screen.getByTestId("chat-reorder-list");
      expect(within(list).getAllByRole("listitem").map((li) => li.textContent)).toEqual(
        ["Launch", "Alpha", "Bravo", "Charlie"].map((t) => expect.stringContaining(t)),
      );
      fireEvent.click(within(list).getByRole("button", { name: "Move Alpha up" }));
      expect(onReorderChats).toHaveBeenCalledWith("pinned", ["a", "project:p-9", "b", "c"]);
    });

    it("can be reordered with one chat and one project, and the pinned project opens from there", () => {
      const onOpenProjectHome = vi.fn();
      renderSidebar({
        conversations: [chats[0]],
        filteredConversations: [chats[0]],
        projects: [LAUNCH],
        onReorderChats: noop,
        onOpenProjectHome,
      });
      expect(screen.getByTestId("pinned-reorder-toggle")).toBeTruthy();
      fireEvent.click(screen.getByRole("button", { name: "Open project Launch" }));
      expect(onOpenProjectHome).toHaveBeenCalledWith("p-9");
    });

    it("shows no Pinned section with nothing pinned", () => {
      renderSidebar();
      expect(screen.queryByTestId("pinned-section")).toBeNull();
    });
  });

  // ── The Chats menu: sort, filter by label, select, tidy up ────────────────
  describe("the Chats menu", () => {
    const openMenu = () => fireEvent.click(screen.getByTestId("recent-sort"));
    const labelled = [
      { ...CONV, id: "l1", title: "Budget", labels: ["finance", "q4"] },
      { ...CONV, id: "l2", title: "Forecast", labels: ["finance"] },
      { ...CONV, id: "l3", title: "Standup" },
    ];

    it("filters by label when the person uses labels, and marks the button while a filter is on", () => {
      const setFilterLabels = vi.fn();
      const { rerenderWith } = renderSidebar({
        conversations: labelled,
        filteredConversations: labelled,
        setFilterLabels,
        onRecentSortChange: noop,
      });
      // No labels on the rows themselves: a row is its title.
      expect(screen.queryByText("finance")).toBeNull();
      openMenu();
      const entry = screen.getByTestId("chats-menu-labels");
      expect(entry.textContent).toContain("Filter by label");
      fireEvent.click(entry);
      const flyout = screen.getByRole("menu", { name: "Filter by label" });
      const choices = within(flyout).getAllByRole("menuitemcheckbox");
      // Each label with how many chats carry it.
      expect(choices.map((c) => [c.textContent, c.getAttribute("aria-checked")])).toEqual([
        ["finance2", "false"],
        ["q41", "false"],
      ]);
      fireEvent.click(choices[0]);
      const toggle = setFilterLabels.mock.calls[0][0] as (ls: string[]) => string[];
      expect(toggle([])).toEqual(["finance"]);
      expect(toggle(["finance", "q4"])).toEqual(["q4"]);

      // With the filter on: the entry counts it, and Chats lists what matches
      // under a chip that takes the filter off.
      const matching = labelled.slice(0, 2);
      rerenderWith({
        conversations: labelled,
        filteredConversations: matching,
        filterLabels: ["finance"],
        setFilterLabels,
        onRecentSortChange: noop,
      });
      expect(screen.getByTestId("chats-menu-labels").textContent).toContain("Filter by label (1)");
      const section = screen.getByTestId("chats-section");
      expect([...section.querySelectorAll("[data-rail-row]")].map((r) => r.getAttribute("title"))).toEqual([
        "Budget · finance, q4",
        "Forecast · finance",
      ]);
      expect(within(section).getByRole("button", { name: "Remove label filter finance" })).toBeTruthy();
    });

    it("has no label entry for a person with no labels", () => {
      renderSidebar({ onRecentSortChange: noop });
      openMenu();
      expect(screen.queryByTestId("chats-menu-labels")).toBeNull();
    });

    it("starts select mode with nothing ticked, and steps aside while selecting", () => {
      const onEnterSelectMode = vi.fn();
      const { rerenderWith } = renderSidebar({ onEnterSelectMode });
      openMenu();
      fireEvent.click(screen.getByTestId("chats-menu-select"));
      expect(onEnterSelectMode).toHaveBeenCalledWith();
      expect(screen.queryByRole("menu", { name: "Chat list options" })).toBeNull();
      rerenderWith({ onEnterSelectMode, selectMode: true });
      expect(screen.queryByTestId("recent-sort")).toBeNull();
      // Each row is a tick box now, with no ⋯.
      const row = screen.getByRole("button", { name: "Quarterly review" });
      expect(row.getAttribute("aria-pressed")).toBe("false");
      expect(screen.queryByRole("button", { name: "Conversation options for Quarterly review" })).toBeNull();
    });

    it("offers Select only when the person has chats of their own to select", () => {
      const team = [{ ...CONV, id: "t1", title: "Team room", user_email: "ana@example.com", team_visible: true }];
      renderSidebar({ conversations: [], filteredConversations: [], teamSharedConversations: team, teamSharedLoaded: true });
      openMenu();
      expect(screen.queryByTestId("chats-menu-select")).toBeNull();
      // Nor a bulk delete: none of these is theirs to delete.
      expect(screen.queryByTestId("delete-temporary-chats")).toBeNull();
    });

    it("asks before deleting temporary chats, says what it keeps, and is absent with none", () => {
      const setConfirmBulkDelete = vi.fn();
      const { unmount } = renderSidebar({ setConfirmBulkDelete });
      openMenu();
      const item = screen.getByTestId("delete-temporary-chats");
      expect(item.textContent).toContain("Delete temporary chats…");
      expect(item.textContent).toContain(
        "Keeps pinned, filed, shared and patrol chats. Temporary chats go after 90 days without activity anyway.",
      );
      fireEvent.click(item);
      expect(setConfirmBulkDelete).toHaveBeenCalledWith(true);
      unmount();
      // Every chat is kept by something (pinned, filed, shared): nothing to delete.
      const kept = [
        { ...CONV, id: "k1", title: "Pinned", pinned: true },
        { ...CONV, id: "k2", title: "Filed", project_id: "p-1" },
        { ...CONV, id: "k3", title: "Shared", team_visible: true },
      ];
      renderSidebar({ conversations: kept, filteredConversations: kept, setConfirmBulkDelete });
      openMenu();
      expect(screen.queryByTestId("delete-temporary-chats")).toBeNull();
    });

    it("has no sort choices where the page cannot change the sort", () => {
      renderSidebar();
      openMenu();
      expect(screen.queryByRole("menuitemradio")).toBeNull();
      expect(screen.getByTestId("chats-menu-select")).toBeTruthy();
    });
  });

  // ── The icon strip ────────────────────────────────────────────────────────
  // Collapsed (from sm up) the sidebar is the + and the top items only: the
  // search field, the pen and every list come back with the sidebar. The pen
  // stays rendered for the phone drawer, which always shows everything.
  describe("collapsed to the icon strip", () => {
    const strip = { collapsed: true, setCollapsed: noop, toggle: noop, hydrated: true };

    it("keeps the + and hides the pen, the search field and the lists", () => {
      const pinnedProject: Project = { ...PROJECT, id: "p-9", name: "Launch", pinned: true };
      renderSidebar({ collapse: strip, conversations: chats, filteredConversations: chats, projects: [PROJECT, pinnedProject] });
      expect(screen.getByTestId("new-menu-button")).toBeTruthy();
      expect(screen.getByTestId("new-chat-button").className.split(/\s+/)).toContain("sm:hidden");
      expect(screen.getByTestId("search-input").parentElement?.className.split(/\s+/)).toContain("sm:hidden");
      expect(screen.getByTestId("rail-lists").className.split(/\s+/)).toContain("sm:hidden");
      // No strip of pinned icons: pinned things are in the sidebar's lists.
      expect(screen.queryByTestId("rail-pinned-icons")).toBeNull();
    });

    it("shows the pen beside the search field when expanded", () => {
      renderSidebar({ conversations: chats, filteredConversations: chats });
      expect(screen.getByTestId("new-chat-button").className.split(/\s+/)).not.toContain("sm:hidden");
      expect(screen.getByTestId("rail-lists").className.split(/\s+/)).not.toContain("sm:hidden");
      expect(screen.queryByTestId("rail-pinned-icons")).toBeNull();
    });
  });
});
