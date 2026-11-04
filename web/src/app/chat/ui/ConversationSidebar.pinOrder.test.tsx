import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// The Pinned section's own order and its reorder mode
// (docs/CHAT-PIN-ORDER.md): rows follow the saved order, Reorder swaps them
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
  return render(<ConversationSidebar {...props} />);
}

const pin = (id: string, title: string): ConversationSummary => ({ ...CONV, id, title, pinned: true });

describe("ConversationSidebar pinned order", () => {
  const chats = [pin("a", "Alpha"), pin("b", "Bravo"), pin("c", "Charlie")];

  it("shows pinned chats in the saved order", () => {
    renderSidebar({ conversations: chats, filteredConversations: chats, pinOrder: ["c", "a", "b"], onReorderPinned: noop });
    const section = screen.getByTestId("pinned-section");
    const titles = ["Alpha", "Bravo", "Charlie"].map((t) => section.textContent!.indexOf(t));
    expect(titles[2]).toBeLessThan(titles[0]);
    expect(titles[0]).toBeLessThan(titles[1]);
  });

  it("reorders in reorder mode and returns to the rows on Done", () => {
    const onReorderPinned = vi.fn();
    renderSidebar({ conversations: chats, filteredConversations: chats, pinOrder: [], onReorderPinned });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    const list = screen.getByTestId("pinned-reorder-list");
    fireEvent.click(within(list).getByRole("button", { name: "Move Bravo up" }));
    expect(onReorderPinned).toHaveBeenCalledWith(["b", "a", "c"]);
    fireEvent.click(screen.getByTestId("pinned-reorder-done"));
    expect(screen.queryByTestId("pinned-reorder-list")).toBeNull();
    expect(screen.getByTestId("pinned-reorder-toggle")).toBeTruthy();
  });

  it("returns the focus to Reorder when the mode ends", () => {
    renderSidebar({ conversations: chats, filteredConversations: chats, onReorderPinned: noop });
    fireEvent.click(screen.getByTestId("pinned-reorder-toggle"));
    fireEvent.click(screen.getByTestId("pinned-reorder-done"));
    expect(document.activeElement).toBe(screen.getByTestId("pinned-reorder-toggle"));
  });

  it("offers no reorder above the server's 500-chat cap", () => {
    const many = Array.from({ length: 501 }, (_, i) => pin(`p${i}`, `Chat ${i}`));
    renderSidebar({ conversations: many, filteredConversations: many, onReorderPinned: noop });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
  });

  it("offers no reorder with a single pinned chat or without a save handler", () => {
    renderSidebar({ conversations: [chats[0]], filteredConversations: [chats[0]], onReorderPinned: noop });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
    cleanup();
    renderSidebar({ conversations: chats, filteredConversations: chats });
    expect(screen.queryByTestId("pinned-reorder-toggle")).toBeNull();
  });
});
