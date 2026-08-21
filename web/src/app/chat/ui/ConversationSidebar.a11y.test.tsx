import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// Focus behavior of the rail's three inline editors.
//
// All three used to rely on `autoFocus`, which the jsx-a11y pass removed. The
// point of these tests is that removing it did NOT quietly drop the focus:
// a rename field nobody focuses is a real regression for everyone, and the
// only way to keep the linter honest is to assert the behavior it was asked
// to change. Each editor now focuses from an effect tied to the state that
// opened it, so each test drives that state the way a user does.

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
    shareConversation: async () => true,
    unshareConversation: asyncNoop,
    copyShareLink: async () => true,
    archivedConversations: [],
    showArchived: false,
    setShowArchived: noop,
    updateAvailable: false,
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
    onShareProject: noop,
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
      utils.rerender(
        <ConversationSidebar {...({ ...props, ...next } as typeof props)} />,
      ),
  };
}

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response("{}", { status: 200 })),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("ConversationSidebar — inline editors take focus", () => {
  it("focuses the chat rename field when a rename is requested", async () => {
    const { rerenderWith } = renderSidebar();
    expect(
      screen.queryByRole("textbox", { name: `Rename ${CONV.title}` }),
    ).not.toBeInTheDocument();

    // The parent's `r` shortcut asks for the rename by bumping the nonce.
    rerenderWith({ renameSignal: { id: CONV.id, nonce: 1 } });

    const field = await screen.findByRole("textbox", {
      name: `Rename ${CONV.title}`,
    });
    await waitFor(() => expect(field).toHaveFocus());
    // Focusing still selects the whole title, so typing replaces it.
    expect((field as HTMLInputElement).selectionStart).toBe(0);
    expect((field as HTMLInputElement).selectionEnd).toBe(CONV.title.length);
  });

  it("focuses the chat rename field when Rename is picked from the row kebab", async () => {
    renderSidebar();
    fireEvent.click(
      screen.getByRole("button", {
        name: `Conversation options for ${CONV.title}`,
      }),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename" }));

    const field = await screen.findByRole("textbox", {
      name: `Rename ${CONV.title}`,
    });
    await waitFor(() => expect(field).toHaveFocus());
  });

  it("focuses the project rename field when Rename is picked from the kebab", async () => {
    renderSidebar();
    fireEvent.click(
      screen.getByRole("button", { name: `Project options for ${PROJECT.name}` }),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename" }));

    const field = await screen.findByRole("textbox", {
      name: `Rename project ${PROJECT.name}`,
    });
    await waitFor(() => expect(field).toHaveFocus());
  });

  it("focuses the label field when the Labels panel opens", async () => {
    renderSidebar();
    fireEvent.click(
      screen.getByRole("button", {
        name: `Conversation options for ${CONV.title}`,
      }),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: "Labels" }));

    const field = await screen.findByPlaceholderText("Add a label…");
    await waitFor(() => expect(field).toHaveFocus());
  });
});
