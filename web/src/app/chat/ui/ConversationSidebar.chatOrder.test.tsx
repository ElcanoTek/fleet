import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// The rail's hand-set chat orders (Pinned, and each project) and the
// Temporary sort
// (docs/CHAT-ORDER.md): rows follow the saved order, Reorder swaps them
// for the drag-and-arrow list, a move hands the whole new order up, and Done
// brings the ordinary rows back.

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

  it("sorts Temporary chats by the chosen sort and reports a change", () => {
    const temp = [
      { ...CONV, id: "t1", title: "Zulu", updated_at: 30, created_at: 1 },
      { ...CONV, id: "t2", title: "Alpha", updated_at: 20, created_at: 2 },
    ];
    const onRecentSortChange = vi.fn();
    renderSidebar({ conversations: temp, filteredConversations: temp, recentSort: "title", onRecentSortChange });
    const zulu = screen.getByText("Zulu");
    const alpha = screen.getByText("Alpha");
    expect(alpha.compareDocumentPosition(zulu) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const select = screen.getByRole("combobox", { name: "Sort temporary chats" }) as HTMLSelectElement;
    expect(select.value).toBe("title");
    fireEvent.change(select, { target: { value: "created" } });
    expect(onRecentSortChange).toHaveBeenCalledWith("created");
  });
});
