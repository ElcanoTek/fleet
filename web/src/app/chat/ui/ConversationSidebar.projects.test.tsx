import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// The rail's Projects section, from a teammate's chair.
//
// Two things were wrong at a glance. A team-shared project and a personal one
// rendered identically — the only tell was the "Shared with team" chip on the
// project's home, so the tester renamed projects "- shared" / "- personal" by
// hand to keep them apart. And an expanded project told a teammate "No chats
// yet" while the same project's home listed two chats their colleagues had
// shared into it: the rail lists only the viewer's OWN chats on purpose
// (ADR-0057 keeps the home as the single discovery surface), which makes
// "empty" the wrong word rather than the shared chats the wrong place.

const SHARED_PROJECT: Project = {
  id: "p-shared",
  owner_email: "lead@example.com",
  name: "Quant",
  team_id: "quant",
  mcp_servers: [],
  created_at: 1_700_000_000,
  updated_at: 1_700_000_002,
};

const PERSONAL_PROJECT: Project = {
  id: "p-mine",
  owner_email: "me@example.com",
  name: "Scratch",
  mcp_servers: [],
  created_at: 1_700_000_000,
  updated_at: 1_700_000_001,
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
    conversations: [] as ConversationSummary[],
    filteredConversations: [] as ConversationSummary[],
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
    projects: [SHARED_PROJECT, PERSONAL_PROJECT],
    onMoveToProject: noop,
    ...overrides,
  } as unknown as Parameters<typeof ConversationSidebar>[0];
  return render(<ConversationSidebar {...props} />);
}

// Expanding a project row is what reveals its chat list (and so its empty
// state); the rows start collapsed.
function expandProject(name: string, chats = 0) {
  fireEvent.click(
    screen.getByRole("button", { name: `Project ${name} (${chats} chats)` }),
  );
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

describe("ConversationSidebar — team-shared project rows", () => {
  it("badges a team-shared project row with its audience", () => {
    renderSidebar();
    // Labeled with the team, not a bare "shared" (docs/TEAM-SHARING.md
    // vocabulary), and reachable by a screen reader rather than living only
    // in a title attribute.
    expect(screen.getByText("Shared with quant")).toBeInTheDocument();
    expect(
      document.querySelector('[title="Shared with quant"]'),
    ).toBeInTheDocument();
  });

  it("leaves a personal project row unbadged", () => {
    renderSidebar({ projects: [PERSONAL_PROJECT] });
    expect(screen.queryByText(/Shared with/)).not.toBeInTheDocument();
    expect(document.querySelector("[title^='Shared with']")).toBeNull();
  });
});

describe("ConversationSidebar — a project's empty state is viewer-aware", () => {
  it("points a teammate at the chats their team shared, with the count", () => {
    const onOpenProjectHome = vi.fn();
    renderSidebar({
      teamSharedChatCounts: { "p-shared": 2 },
      onOpenProjectHome,
    });
    expandProject("Quant");

    expect(screen.getByText(/No chats of yours yet/)).toBeInTheDocument();
    // The count is quoted, and the arrow opens the one surface that lists the
    // shared chats — they stay out of the rail (ADR-0057).
    const link = screen.getByRole("button", {
      name: "Open Quant — 2 chats shared by your team",
    });
    expect(link).toHaveTextContent("2 shared by your team");
    fireEvent.click(link);
    expect(onOpenProjectHome).toHaveBeenCalledWith("p-shared");
    // No second listing appeared in the rail.
    expect(screen.queryByText(/read-only/)).not.toBeInTheDocument();
  });

  it("says one chat in the singular", () => {
    renderSidebar({ teamSharedChatCounts: { "p-shared": 1 } });
    expandProject("Quant");
    expect(
      screen.getByRole("button", {
        name: "Open Quant — 1 chat shared by your team",
      }),
    ).toHaveTextContent("1 shared by your team");
  });

  it("keeps the filing copy when a team-shared project really is empty", () => {
    // Counts loaded, and this project has none: nothing here for anyone, so
    // the copy that teaches both filing paths is still the right one.
    renderSidebar({ teamSharedChatCounts: {} });
    expandProject("Quant");
    expect(screen.getByText(/No chats yet — drag one here/)).toBeInTheDocument();
    expect(screen.queryByText(/No chats of yours yet/)).not.toBeInTheDocument();
  });

  it("keeps the filing copy for a personal project", () => {
    renderSidebar({ teamSharedChatCounts: { "p-mine": 7 } });
    expandProject("Scratch");
    // A personal project cannot hold a team-shared chat, so no count can
    // apply to it — and none is quoted even when the map bogusly carries one.
    expect(screen.getByText(/No chats yet — drag one here/)).toBeInTheDocument();
    expect(screen.queryByText(/shared by your team/)).not.toBeInTheDocument();
  });

  it("asserts no number when the counts have not loaded", () => {
    const onOpenProjectHome = vi.fn();
    renderSidebar({ onOpenProjectHome });
    expandProject("Quant");

    expect(screen.getByText(/No chats of yours yet/)).toBeInTheDocument();
    expect(screen.queryByText(/shared by your team/)).not.toBeInTheDocument();
    const link = screen.getByRole("button", {
      name: "Open Quant — anything your team shared is on the project home",
    });
    fireEvent.click(link);
    expect(onOpenProjectHome).toHaveBeenCalledWith("p-shared");
  });
});
