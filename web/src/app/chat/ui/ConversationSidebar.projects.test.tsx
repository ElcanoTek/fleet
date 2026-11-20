import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { ConversationSidebar } from "./ConversationSidebar";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { BUILTIN_LAYOUT } from "@/app/shared/sidebar/layout";
import { loadSidebar, resetSidebarStoreForTests } from "@/app/shared/sidebar/store";

// The sidebar's quick pin is offered only once /api/me confirms a role that
// may save a layout (docs/SIDEBAR.md); these rows are a member's.
vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: "member", team_id: "", admin: false }),
}));

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

// The one line an expanded, empty project shows (its words and the action in
// it are separate nodes).
function emptyLine(projectId: string) {
  return document.querySelector(`[data-project-id="${projectId}"] p`)?.textContent;
}

const menuItems = () => screen.getAllByRole("menuitem").map((el) => el.textContent?.trim());

beforeEach(() => {
  resetSidebarStoreForTests();
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
    // The row's one status is the team mark at its right edge; its sentence
    // is the row's tooltip, after the project's name.
    const row = screen.getByRole("button", { name: "Open project Quant" });
    expect(row).toHaveAttribute("title", "Quant · Shared with quant");
    expect(row.querySelector("svg")).not.toBeNull();
  });

  it("leaves a personal project row unbadged", () => {
    renderSidebar({ projects: [PERSONAL_PROJECT] });
    expect(screen.queryByText(/Shared with/)).not.toBeInTheDocument();
    expect(document.querySelector("[title*='Shared with']")).toBeNull();
    const row = screen.getByRole("button", { name: "Open project Scratch" });
    expect(row).toHaveAttribute("title", "Scratch");
    expect(row.querySelector("svg")).toBeNull();
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

  it("says just “No chats yet” when a team-shared project really is empty", () => {
    // Counts loaded, and this project has none: nothing here for anyone.
    // Decision #23 replaced the filing-paths paragraph with the short line.
    const onNewChatInProject = vi.fn();
    renderSidebar({ teamSharedChatCounts: {}, onNewChatInProject });
    expandProject("Quant");
    expect(emptyLine("p-shared")).toBe("No chats yet · New chat here");
    expect(screen.queryByText(/drag one here/)).not.toBeInTheDocument();
    expect(screen.queryByText(/No chats of yours yet/)).not.toBeInTheDocument();
    // The line offers the next step, in that project.
    fireEvent.click(screen.getByRole("button", { name: "New chat in Quant" }));
    expect(onNewChatInProject).toHaveBeenCalledWith("p-shared");
  });

  it("says “No chats yet” for an empty personal project", () => {
    renderSidebar({ teamSharedChatCounts: { "p-mine": 7 } });
    expandProject("Scratch");
    // A personal project cannot hold a team-shared chat, so no count can
    // apply to it — and none is quoted even when the map bogusly carries one.
    expect(emptyLine("p-mine")).toBe("No chats yet · New chat here");
    expect(screen.queryByText(/shared by your team/)).not.toBeInTheDocument();
  });

  it("offers no “New chat here” where the page cannot start one", () => {
    renderSidebar({ teamSharedChatCounts: {}, onNewChatInProject: undefined });
    expandProject("Scratch");
    expect(emptyLine("p-mine")).toBe("No chats yet");
  });

  it("asserts no number when the counts have not loaded", () => {
    renderSidebar();
    expandProject("Quant");

    // Not "No chats yet": teammates may have shared chats the rail can't
    // count yet, so it only speaks for the viewer's own.
    expect(emptyLine("p-shared")).toBe("No chats of yours yet · New chat here");
    expect(screen.queryByText(/shared by your team/)).not.toBeInTheDocument();
  });
});

describe("ConversationSidebar — project rows (decision #23)", () => {
  it("opens the project's home AND expands it when the name is clicked", () => {
    const onOpenProjectHome = vi.fn();
    renderSidebar({ onOpenProjectHome, teamSharedChatCounts: {} });
    const chevron = screen.getByRole("button", { name: "Project Scratch (0 chats)" });
    expect(chevron).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(screen.getByRole("button", { name: "Open project Scratch" }));
    expect(onOpenProjectHome).toHaveBeenCalledWith("p-mine");
    expect(chevron).toHaveAttribute("aria-expanded", "true");
    expect(emptyLine("p-mine")).toBe("No chats yet · New chat here");

    // Clicking the name again keeps it open — opening never folds.
    fireEvent.click(screen.getByRole("button", { name: "Open project Scratch" }));
    expect(chevron).toHaveAttribute("aria-expanded", "true");
  });

  it("only folds and unfolds from the chevron — it never opens the home", () => {
    const onOpenProjectHome = vi.fn();
    renderSidebar({ onOpenProjectHome });
    const chevron = screen.getByRole("button", { name: "Project Scratch (0 chats)" });
    fireEvent.click(chevron);
    expect(chevron).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(chevron);
    expect(chevron).toHaveAttribute("aria-expanded", "false");
    expect(onOpenProjectHome).not.toHaveBeenCalled();
  });

  // The ⋯ menu keeps one order on every row (docs/SIDEBAR.md): open and
  // start; where it lives (Pin, Move to the top, Rename) and where fleet
  // opens; pass it on; put it away. The two sidebar items appear once the
  // account is known to be able to save its sidebar.
  it("gives the owner the whole menu, in the one order", async () => {
    const onOpenProjectHome = vi.fn();
    const onNewChatInProject = vi.fn();
    const onPinProject = vi.fn();
    const onDeleteProject = vi.fn();
    renderSidebar({ onOpenProjectHome, onNewChatInProject, onPinProject, onDeleteProject });
    const open = () => fireEvent.click(screen.getByRole("button", { name: "Project options for Scratch" }));

    open();
    await waitFor(() =>
      expect(menuItems()).toEqual([
        "Open project",
        "New chat",
        "Pin",
        "Move to the top",
        "Rename",
        "Set as start page",
        "Share…",
        "Project settings…",
        "Delete project",
      ]),
    );
    // Share opens the project's sharing, which lives in its settings.
    fireEvent.click(screen.getByRole("menuitem", { name: "Share…" }));
    expect(onOpenProjectHome).toHaveBeenCalledWith("p-mine", true);

    open();
    fireEvent.click(screen.getByRole("menuitem", { name: "New chat" }));
    expect(onNewChatInProject).toHaveBeenCalledWith("p-mine");

    open();
    fireEvent.click(screen.getByRole("menuitem", { name: "Open project" }));
    expect(onOpenProjectHome).toHaveBeenLastCalledWith("p-mine");

    open();
    fireEvent.click(screen.getByRole("menuitem", { name: "Pin" }));
    expect(onPinProject).toHaveBeenCalledWith("p-mine", true);

    open();
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete project" }));
    expect(onDeleteProject).toHaveBeenCalledWith("p-mine");
  });

  it("renames a project in place from its menu", () => {
    const onRenameProject = vi.fn();
    renderSidebar({ onRenameProject });
    fireEvent.click(screen.getByRole("button", { name: "Project options for Scratch" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename" }));
    const field = screen.getByRole("textbox", { name: "Rename project Scratch" });
    expect(document.activeElement).toBe(field);
    fireEvent.change(field, { target: { value: "Scratchpad" } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(onRenameProject).toHaveBeenCalledWith("p-mine", "Scratchpad");
    expect(screen.queryByRole("textbox", { name: "Rename project Scratch" })).toBeNull();
  });

  it("gives a member Open, New chat and the two sidebar items only — the rest is the owner's", async () => {
    renderSidebar();
    fireEvent.click(
      screen.getByRole("button", { name: "Project options for Quant" }),
    );
    await waitFor(() =>
      expect(menuItems()).toEqual(["Open project", "New chat", "Move to the top", "Set as start page"]),
    );
  });
});

// The Projects section lists the most recently updated few; the rest are one
// click away, and a pinned project is listed in Pinned, not here.
describe("ConversationSidebar — the Projects section stays short", () => {
  const many: Project[] = Array.from({ length: 11 }, (_, i) => ({
    ...PERSONAL_PROJECT,
    id: `p-${i}`,
    name: `Project ${String.fromCharCode(65 + i)}`,
    // Project A is the most recently updated.
    updated_at: 1_700_000_100 - i,
  }));
  const listed = () =>
    within(screen.getByTestId("projects-section"))
      .getAllByRole("button", { name: /^Open project / })
      .map((b) => b.textContent);

  it("shows eight, then “All N projects”, which opens the rest in place", () => {
    renderSidebar({ projects: many });
    expect(listed()).toEqual(many.slice(0, 8).map((p) => p.name));
    const all = screen.getByTestId("projects-show-all");
    expect(all).toHaveTextContent("All 11 projects");
    expect(all).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(all);
    expect(listed()).toHaveLength(11);
    expect(all).toHaveTextContent("Show fewer");
    expect(all).toHaveAttribute("aria-expanded", "true");
    fireEvent.click(all);
    expect(listed()).toHaveLength(8);
  });

  it("offers no “All N projects” at eight or fewer", () => {
    renderSidebar({ projects: many.slice(0, 8) });
    expect(listed()).toHaveLength(8);
    expect(screen.queryByTestId("projects-show-all")).toBeNull();
  });

  it("never leaves the open project behind the fold", () => {
    renderSidebar({ projects: many, activeProjectId: "p-10" });
    expect(listed()).toHaveLength(11);
    expect(screen.getByRole("button", { name: "Open project Project K" })).toHaveAttribute("aria-current", "page");
  });

  it("lists a pinned project in Pinned and not in Projects", () => {
    const pinned = { ...PERSONAL_PROJECT, id: "p-pin", name: "Kept close", pinned: true };
    renderSidebar({ projects: [pinned, PERSONAL_PROJECT] });
    expect(within(screen.getByTestId("pinned-section")).getByRole("button", { name: "Open project Kept close" })).toBeTruthy();
    expect(listed()).toEqual(["Scratch"]);
  });

  it("offers “New project” when there is none to list", () => {
    const onCreateProject = vi.fn();
    renderSidebar({ projects: [], onCreateProject });
    fireEvent.click(within(screen.getByTestId("projects-section")).getByRole("button", { name: "New project…" }));
    expect(onCreateProject).toHaveBeenCalledTimes(1);
  });
});

// "Status on rows" off (docs/SIDEBAR.md): the mark is not drawn; what it
// says still reaches a screen reader.
describe("ConversationSidebar — project rows with row statuses turned off", () => {
  it("draws no team mark and keeps the audience for a screen reader", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) =>
        String(input).startsWith("/api/sidebar")
          ? new Response(JSON.stringify({ layout: { ...BUILTIN_LAYOUT, status_off: true }, rev: 1 }), { status: 200 })
          : new Response("{}", { status: 200 }),
      ),
    );
    await act(async () => {
      await loadSidebar();
    });
    renderSidebar();
    const row = screen.getByRole("button", { name: "Open project Quant" });
    expect(row.querySelector("svg")).toBeNull();
    expect(screen.getByText("Shared with quant")).toBeInTheDocument();
  });
});
