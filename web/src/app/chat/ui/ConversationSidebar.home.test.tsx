import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { HomeConversation } from "./homeChat";

// The Home chat in the rail (docs/HOME-CHAT.md): the first row of the chat
// list, marked Home (never repeated in Pinned or Team chats), and the
// "Set as home" / "Remove as home" menu items on own and team rows.

afterEach(cleanup);

const conv = (id: string, title: string, extra: Partial<ConversationSummary> = {}): ConversationSummary => ({
  id,
  title,
  persona: "default",
  model: "test/model",
  pinned: false,
  updated_at: 1_700_000_000,
  ...extra,
});

const SERVER_CONFIG: ServerConfig = {
  lockdownAvailable: false,
  lockdownOnly: false,
  lockdownAllowedModels: [],
  uploadMaxBytes: 1_000_000,
};

const noop = () => {};
const asyncNoop = async () => {};

function renderSidebar(overrides: Record<string, unknown> = {}) {
  const conversations = [
    conv("mine-home", "Morning patrol", { pinned: true }),
    conv("mine-pinned", "Pinned one", { pinned: true }),
    conv("mine-temp", "Scratch"),
  ];
  const props = {
    sidebarOpen: true,
    setSidebarOpen: noop,
    collapse: { collapsed: false, setCollapsed: noop, toggle: noop, hydrated: true },
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
    conversations,
    filteredConversations: conversations,
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
    projects: [],
    onMoveToProject: noop,
    teamSharedConversations: [
      conv("team-dash", "Team dashboard", { user_email: "lead@example.com" }),
      conv("team-other", "Other team chat", { user_email: "lead@example.com" }),
    ],
    onSelectTeamSharedConversation: noop,
    ...overrides,
  } as unknown as Parameters<typeof ConversationSidebar>[0];
  return render(<ConversationSidebar {...props} />);
}

const OWN_HOME: HomeConversation = {
  conversation_id: "mine-home",
  access: "owner",
  title: "Morning patrol",
  owner_email: "me@example.com",
};
const TEAM_HOME: HomeConversation = {
  conversation_id: "team-dash",
  access: "team_view",
  title: "Team dashboard",
  owner_email: "lead@example.com",
};

const openMenu = (row: HTMLElement, title: string) =>
  fireEvent.click(within(row).getByRole("button", { name: `Conversation options for ${title}` }));

describe("Home in the rail", () => {
  it("shows an owned Home first, marked Home, and not again in Pinned", () => {
    renderSidebar({ home: OWN_HOME, onOpenHome: noop, onSetHome: noop, onClearHome: noop });
    const homeSection = screen.getByTestId("home-section");
    expect(within(homeSection).getByText("Morning patrol")).toBeTruthy();
    expect(within(homeSection).getByTestId("home-badge").textContent).toBe("Home");
    expect(screen.getAllByTestId("home-badge")).toHaveLength(1);
    expect(screen.queryByTestId("nav-home")).toBeNull();
    const pinned = screen.getByTestId("pinned-section");
    expect(within(pinned).queryByText("Morning patrol")).toBeNull();
    expect(within(pinned).getByText("Pinned one")).toBeTruthy();
  });

  it("shows a team Home with its owner, drops it from Team chats, and opens it", () => {
    const onOpenHome = vi.fn();
    renderSidebar({ home: TEAM_HOME, onOpenHome, onSetHome: noop, onClearHome: noop });
    const homeSection = screen.getByTestId("home-section");
    expect(within(homeSection).getByText("lead")).toBeTruthy();
    expect(within(homeSection).getByTestId("home-badge")).toBeTruthy();
    const teamRows = screen.getAllByTestId("team-shared-conv-row").map((r) => r.textContent);
    expect(teamRows.some((t) => t?.includes("Team dashboard"))).toBe(false);
    expect(screen.getByText("Team chats (1)")).toBeTruthy();
    fireEvent.click(screen.getByTestId("home-conv-row"));
    expect(onOpenHome).toHaveBeenCalledTimes(1);
  });

  it("offers Set as home on own and team rows, and Remove as home on Home", () => {
    const onSetHome = vi.fn();
    const onClearHome = vi.fn();
    renderSidebar({ home: OWN_HOME, onOpenHome: noop, onSetHome, onClearHome });

    const scratch = screen.getByText("Scratch").closest("[data-conversation-id]") as HTMLElement;
    openMenu(scratch, "Scratch");
    fireEvent.click(screen.getByRole("menuitem", { name: "Set as home" }));
    expect(onSetHome).toHaveBeenCalledWith(expect.objectContaining({ id: "mine-temp" }));

    const teamRow = screen.getByText("Other team chat").closest(".group") as HTMLElement;
    openMenu(teamRow, "Other team chat");
    fireEvent.click(screen.getByRole("menuitem", { name: "Set as home" }));
    expect(onSetHome).toHaveBeenCalledWith(expect.objectContaining({ id: "team-other" }));

    const homeRow = within(screen.getByTestId("home-section")).getByText("Morning patrol").closest("[data-conversation-id]") as HTMLElement;
    openMenu(homeRow, "Morning patrol");
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove as home" }));
    expect(onClearHome).toHaveBeenCalledTimes(1);
  });

  it("offers no Home items without the handlers", () => {
    renderSidebar();
    expect(screen.queryByTestId("home-section")).toBeNull();
    const scratch = screen.getByText("Scratch").closest("[data-conversation-id]") as HTMLElement;
    openMenu(scratch, "Scratch");
    expect(screen.queryByRole("menuitem", { name: "Set as home" })).toBeNull();
  });
});
