import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { ConversationSidebar } from "./ConversationSidebar";
import { filterConversations } from "./conversationOrganization";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { BUILTIN_LAYOUT, keepAtTop, updateSection, type SectionId } from "@/app/shared/sidebar/layout";
import { getSidebarState, loadSidebar, resetSidebarStoreForTests } from "@/app/shared/sidebar/store";
import { resetPatrolCacheForTests } from "@/app/shared/sidebar/targets";

// The rail's search looks everywhere, whatever the sidebar is set to show
// (docs/SIDEBAR-SEARCH.md). The reports this guards: a search that never
// looked at Team chats, and results that stayed out of sight because the
// section holding them was folded or hidden — "my chats are gone".

vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: "member", team_id: "", admin: false }),
}));

// "New patrol" opens the patrol wizard through this call; the wizard itself
// (a lazy page of its own) stays shut here.
const openPatrolWizard = vi.hoisted(() => vi.fn());
vi.mock("@/app/launchpad/patrolWizard", async (original) => ({
  ...(await original<typeof import("@/app/launchpad/patrolWizard")>()),
  openPatrolWizard,
}));

const SERVER_CONFIG: ServerConfig = {
  lockdownAvailable: false,
  lockdownOnly: false,
  lockdownAllowedModels: [],
  uploadMaxBytes: 1_000_000,
};

const conv = (over: Partial<ConversationSummary>): ConversationSummary => ({
  id: "c",
  title: "Untitled",
  persona: "default",
  model: "",
  pinned: false,
  updated_at: 1_700_000_000,
  ...over,
});

const OWN = [
  conv({ id: "own-1", title: "Budget pacing review" }),
  conv({ id: "own-2", title: "Résumé notes", pinned: true }),
  conv({ id: "own-3", title: "Pacing in the project", project_id: "p-1" }),
  conv({ id: "own-patrol", title: "Morning pacing patrol" }),
];
const TEAM = [
  conv({ id: "team-1", title: "Quarterly pacing plan", user_email: "dana@example.com", team_visible: true }),
  conv({ id: "team-2", title: "Launch checklist", user_email: "lee@example.com", multiplayer: true }),
];
const ARCHIVED = [conv({ id: "arch-1", title: "Old pacing draft", archived_at: 1_700_000_000 })];
const PROJECTS: Project[] = [
  {
    id: "p-1",
    owner_email: "me@example.com",
    name: "Pacing playbook",
    mcp_servers: [],
    created_at: 1_700_000_000,
    updated_at: 1_700_000_001,
  },
  {
    id: "p-2",
    owner_email: "me@example.com",
    name: "Ops runbooks",
    mcp_servers: [],
    created_at: 1_700_000_000,
    updated_at: 1_700_000_000,
  },
];

const noop = () => {};
const asyncNoop = async () => {};

type Spies = {
  loadConversation: ReturnType<typeof vi.fn>;
  onSelectTeamSharedConversation: ReturnType<typeof vi.fn>;
};

// The parent owns the query and the filtered list; this stand-in does what
// ChatExperience does, so typing in the real field drives the real rail.
function Harness({
  spies,
  initialQuery = "",
  overrides = {},
}: {
  spies: Spies;
  initialQuery?: string;
  overrides?: Record<string, unknown>;
}) {
  const [query, setQuery] = useState(initialQuery);
  const searchRef = { current: null as HTMLInputElement | null };
  const props = {
    sidebarOpen: true,
    setSidebarOpen: noop,
    collapse: { collapsed: false, setCollapsed: noop, toggle: noop, hydrated: true },
    branding: DEFAULT_BRANDING,
    serverConfig: SERVER_CONFIG,
    userEmail: "me@example.com",
    onSignOut: noop,
    clearConversation: noop,
    sidebarQuery: query,
    setSidebarQuery: setQuery,
    searchRef,
    filterLabels: [],
    setFilterLabels: noop,
    isLoadingHistory: false,
    conversations: OWN,
    filteredConversations: filterConversations(OWN, { query }),
    activeConversationId: null,
    focusedConversationId: null,
    renameSignal: null,
    loadConversation: spies.loadConversation,
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
    archivedConversations: ARCHIVED,
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
    projects: PROJECTS,
    teamSharedConversations: TEAM,
    onSelectTeamSharedConversation: spies.onSelectTeamSharedConversation,
    onMoveToProject: noop,
    projectsLoaded: true,
    teamSharedLoaded: true,
    archivedLoaded: true,
    ...overrides,
  } as unknown as Parameters<typeof ConversationSidebar>[0];
  return <ConversationSidebar {...props} />;
}

function newSpies(): Spies {
  return { loadConversation: vi.fn(async () => {}), onSelectTeamSharedConversation: vi.fn() };
}

// The layout the server hands this account: the built-in one with `patch`
// applied to the named sections.
function layoutWith(patches: Partial<Record<SectionId, { hidden?: boolean; collapsed?: boolean }>>) {
  let layout = BUILTIN_LAYOUT;
  for (const [id, patch] of Object.entries(patches)) layout = updateSection(layout, id as SectionId, patch);
  return layout;
}

// The same, for a person who chose separate lists: Team chats is a section of
// its own, with its own fold.
function separateLayout(patches: Parameters<typeof layoutWith>[0] = {}) {
  return { ...layoutWith(patches), separate_lists: true };
}

// The patrols this account has, as GET /api/patrols lists them.
const DEFAULT_PATROLS: unknown[] = [{ conversation_id: "own-patrol" }];

function stubFetch(layout: unknown, messageHits: unknown[] = [], patrols: unknown[] = DEFAULT_PATROLS) {
  const calls: { url: string; method: string; body?: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, method: init?.method ?? "GET", body: typeof init?.body === "string" ? init.body : undefined });
      if (url.startsWith("/api/sidebar")) {
        // A save answers with what was saved, as the server does.
        if (init?.method === "PUT") return new Response(String(init.body), { status: 200 });
        return new Response(JSON.stringify({ layout, rev: 1 }), { status: 200 });
      }
      if (url.startsWith("/api/patrols")) {
        return new Response(JSON.stringify({ patrols }), { status: 200 });
      }
      if (url.startsWith("/api/search")) {
        return new Response(JSON.stringify({ results: messageHits, total: messageHits.length }), { status: 200 });
      }
      return new Response("{}", { status: 200 });
    }),
  );
  return calls;
}

async function renderRail(
  layout: unknown,
  opts: { query?: string; messageHits?: unknown[]; patrols?: unknown[]; overrides?: Record<string, unknown> } = {},
) {
  const calls = stubFetch(layout, opts.messageHits, opts.patrols);
  const spies = newSpies();
  await act(async () => {
    await loadSidebar();
  });
  render(<Harness spies={spies} initialQuery={opts.query} overrides={opts.overrides} />);
  return { calls, spies };
}

const search = () => screen.getByTestId("search-input");
const type = (q: string) => fireEvent.change(search(), { target: { value: q } });
const group = (name: string) => screen.getByTestId(`search-group-${name}`);

// A row's one status (RowStatus, docs/SIDEBAR.md): the marks of one kind, the
// row a mark sits on, the kind a row shows (null: it says nothing), and the
// sentence the mark stands for, which is the row's description.
const statuses = (key: string, root: ParentNode = document) =>
  Array.from(root.querySelectorAll<HTMLElement>(`[data-testid="row-status"][data-status="${key}"]`));
const rowTitle = (el: Element) => el.closest("[data-rail-row]")?.getAttribute("title");
const railRow = (title: string) => document.querySelector<HTMLElement>(`[data-rail-row][title="${title}"]`)!;
const statusOf = (title: string) =>
  railRow(title).querySelector('[data-testid="row-status"]')?.getAttribute("data-status") ?? null;
const describedBy = (row: Element) =>
  document.getElementById(row.getAttribute("aria-describedby") ?? "")?.textContent ?? "";
const listTitles = () =>
  Array.from(screen.getByTestId("unified-chat-list").querySelectorAll("[data-rail-row]")).map((r) => r.getAttribute("title"));

beforeEach(() => {
  resetSidebarStoreForTests();
  resetPatrolCacheForTests();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("ConversationSidebar — search looks everywhere", () => {
  it("finds own, project, team, archived chats and projects in one list", async () => {
    await renderRail(null);
    type("pacing");

    const chats = within(group("chats"));
    expect(chats.getByTitle("Budget pacing review")).toBeTruthy();
    expect(chats.getByTitle("Morning pacing patrol")).toBeTruthy();
    // A chat filed in a project says which one.
    expect(chats.getByTitle("Pacing in the project").textContent).toContain("Pacing playbook ›");
    expect(chats.queryByTitle("Résumé notes")).toBeNull();

    // Team chats were never searched before: only the match is listed.
    const team = within(group("team"));
    expect(team.getAllByTestId("team-shared-conv-row")).toHaveLength(1);
    // Whose it is: the owner's initial on the row, their address in the
    // row's tooltip and in its description.
    const teamRow = team.getByTestId("team-shared-conv-row");
    expect(within(teamRow).getByTestId("row-owner").textContent).toBe("D");
    expect(teamRow.getAttribute("title")).toBe("Quarterly pacing plan (dana@example.com)");
    expect(describedBy(teamRow)).toBe("Shared by dana@example.com");

    expect(within(group("projects")).getByRole("button", { name: "Open project Pacing playbook" })).toBeTruthy();
    expect(within(group("projects")).queryByRole("button", { name: "Open project Ops runbooks" })).toBeNull();
    expect(within(group("archived")).getByText("Old", { exact: false })).toBeTruthy();

    // The sections step aside: no folded heading can hide a result.
    expect(screen.queryByTestId("team-chats-section")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("6 results for “pacing”");
  });

  it("marks the matched words, and matches every word in any order, ignoring accents", async () => {
    await renderRail(null);
    type("review budget");
    const row = within(group("chats")).getByTitle("Budget pacing review");
    expect(Array.from(row.querySelectorAll("mark")).map((m) => m.textContent)).toEqual(["Budget", "review"]);

    type("resume");
    expect(within(group("chats")).getByTitle("Résumé notes").querySelector("mark")?.textContent).toBe("Résumé");
  });

  it("finds a teammate's chat by whose it is", async () => {
    await renderRail(null);
    type("lee");
    expect(within(group("team")).getByText("Launch checklist")).toBeTruthy();
  });

  it("marks a patrol chat, in the lists and in the results, once the patrols are known", async () => {
    await renderRail(null);
    // In the ordinary list, before any search.
    await waitFor(() => expect(statuses("patrol")).toHaveLength(1));
    const [mark] = statuses("patrol");
    expect(rowTitle(mark)).toBe("Morning pacing patrol");
    expect(mark.getAttribute("title")).toContain("schedule");
    // The mark does not rename the row: it is the row's description.
    const row = screen.getByRole("button", { name: "Morning pacing patrol" });
    expect(describedBy(row)).toContain("schedule");
    type("morning");
    expect(statuses("patrol", group("chats")).map(rowTitle)).toEqual(["Morning pacing patrol"]);
  });

  it("what the page knows of a chat's schedules wins over the patrols list", async () => {
    await renderRail(null, {
      overrides: {
        // own-patrol's patrol was just ended here; own-1 just got one.
        scheduleKindByConv: new Map([
          ["own-patrol", "none"],
          ["own-1", "patrol"],
        ]),
      },
    });
    await waitFor(() => expect(statuses("patrol").map(rowTitle)).toEqual(["Budget pacing review"]));
    // …and stays that way once the patrols list has answered.
    await act(async () => {
      await Promise.resolve();
    });
    expect(statuses("patrol").map(rowTitle)).toEqual(["Budget pacing review"]);
  });

  it("shows results when every section is folded", async () => {
    await renderRail(
      separateLayout({ chats: { collapsed: true }, team: { collapsed: true }, projects: { collapsed: true } }),
    );
    // Folded: nothing listed until the search.
    expect(screen.queryByTestId("team-shared-conv-row")).toBeNull();
    type("pacing");
    expect(within(group("team")).getByText("pacing", { exact: false })).toBeTruthy();
    expect(within(group("chats")).getByTitle("Budget pacing review")).toBeTruthy();
    expect(screen.queryByTestId("search-hidden-note")).toBeNull();
    // The list is not inside a hidden container.
    expect(screen.getByTestId("search-results").closest(".hidden")).toBeNull();
  });

  it("with Pinned hidden, a pinned project is still listed under Projects", async () => {
    // A layout saved before Pinned existed starts with it hidden when Chats
    // was; its pinned projects were in Projects then and must stay findable.
    const projects = [{ ...PROJECTS[0], pinned: true }];
    await renderRail(layoutWith({ pinned: { hidden: true }, chats: { hidden: true } }), { overrides: { projects } });
    expect(screen.queryByTestId("pinned-section")).toBeNull();
    expect(within(screen.getByTestId("projects-section")).getByText("Pacing playbook")).toBeTruthy();
  });

  it("with Pinned shown, a pinned project is in Pinned and not again under Projects", async () => {
    const projects = [{ ...PROJECTS[0], pinned: true }];
    await renderRail(layoutWith({}), { overrides: { projects } });
    expect(within(screen.getByTestId("pinned-section")).getByText("Pacing playbook")).toBeTruthy();
    expect(within(screen.getByTestId("projects-section")).queryByText("Pacing playbook")).toBeNull();
  });

  it("finds chats in hidden sections, says they are hidden, and brings a section back", async () => {
    const { calls } = await renderRail(
      separateLayout({ chats: { hidden: true }, team: { hidden: true }, projects: { hidden: true } }),
    );
    expect(screen.queryByTestId("team-chats-section")).toBeNull();
    type("pacing");

    for (const name of ["chats", "team", "projects"]) {
      expect(within(group(name)).getByTestId("search-hidden-note")).toBeTruthy();
    }
    // Archived is not a section of its own: no note there.
    expect(within(group("archived")).queryByTestId("search-hidden-note")).toBeNull();

    const show = await within(group("team")).findByRole("button", { name: "Show Team chats in the sidebar" });
    fireEvent.click(show);
    expect(getSidebarState().layout.sections.find((x) => x.id === "team")?.hidden).toBeFalsy();
    expect(within(group("team")).queryByTestId("search-hidden-note")).toBeNull();
    // The other sections stay as the person set them.
    expect(getSidebarState().layout.sections.find((x) => x.id === "projects")?.hidden).toBe(true);
    await waitFor(() => expect(calls.some((c) => c.url === "/api/sidebar" && c.method === "PUT")).toBe(true));

    // Clearing the search returns the rail, now with Team chats in it.
    type("");
    expect(screen.getByTestId("team-chats-section")).toBeTruthy();
  });

  it("opens a team result as a team chat and an own result as a chat", async () => {
    const { spies } = await renderRail(layoutWith({ team: { hidden: true } }));
    type("pacing");
    fireEvent.click(within(group("team")).getByTestId("team-shared-conv-row"));
    expect(spies.onSelectTeamSharedConversation).toHaveBeenCalledWith(TEAM[0]);
    fireEvent.click(within(group("archived")).getByText("Old", { exact: false }));
    expect(spies.loadConversation).toHaveBeenCalledWith("arch-1");
  });

  it("walks the results with the arrow keys and opens the first with Enter", async () => {
    const { spies } = await renderRail(null);
    type("pacing");
    fireEvent.keyDown(search(), { key: "ArrowDown" });
    const first = within(group("chats")).getByTitle("Budget pacing review");
    expect(document.activeElement).toBe(first);
    fireEvent.keyDown(first, { key: "ArrowDown" });
    expect(document.activeElement).toBe(within(group("chats")).getByTitle("Pacing in the project"));
    fireEvent.keyDown(document.activeElement as Element, { key: "ArrowUp" });
    fireEvent.keyDown(document.activeElement as Element, { key: "ArrowUp" });
    expect(document.activeElement).toBe(search());

    fireEvent.keyDown(search(), { key: "Enter" });
    expect(spies.loadConversation).toHaveBeenCalledWith("own-1");
  });

  it("lists message hits once, tags an archived one, and says when nothing matches", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await renderRail(null, {
        messageHits: [
          { conversation_id: "own-1", title: "Budget pacing review", match_preview: "x", matched_at: 1 },
          { conversation_id: "own-2", title: "Résumé notes", match_preview: "the <mark>budget</mark> line", matched_at: 1 },
          { conversation_id: "arch-9", title: "Elsewhere", match_preview: "a <mark>budget</mark>", matched_at: 1 },
        ],
      });
      type("budget");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(350);
      });
      const hits = within(group("messages")).getAllByTestId("search-result");
      // own-1 is already listed by title, so it is not repeated.
      expect(hits.map((h) => h.textContent)).toEqual(["Résumé notesthe budget line", "Elsewherea budget"]);
      expect(hits[0].querySelector("mark")?.textContent).toBe("budget");
    } finally {
      vi.useRealTimers();
    }
  });

  it("drops the previous query's message hits until the new answer arrives", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await renderRail(null, {
        messageHits: [{ conversation_id: "x-1", title: "Elsewhere", match_preview: "a hit", matched_at: 1 }],
      });
      type("first");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(350);
      });
      expect(screen.getAllByTestId("search-result")).toHaveLength(1);
      // A new query: the old hit is not shown, counted or opened by Enter.
      type("second");
      expect(screen.queryByTestId("search-result")).toBeNull();
      expect(screen.getByRole("status").textContent).toBe("Searching…");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(350);
      });
      expect(screen.getAllByTestId("search-result")).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("says which lists it could not search while they have not loaded", async () => {
    await renderRail(null, { overrides: { teamSharedConversations: [], teamSharedLoaded: false, archivedLoaded: false } });
    type("zzzz");
    const empty = await screen.findByTestId("search-empty", undefined, { timeout: 2000 });
    // No claim to have looked everywhere.
    expect(empty.textContent).not.toContain("including hidden and folded sections");
    expect(screen.getByTestId("search-incomplete").textContent).toContain("Team chats, Archived");
    // With results too: a match may still be missing from the list.
    type("pacing");
    expect(within(group("chats")).getByTitle("Budget pacing review")).toBeTruthy();
    expect(screen.getByTestId("search-incomplete")).toBeTruthy();
  });

  it("explains an empty search and clears it", async () => {
    await renderRail(null);
    type("zzzz");
    // The message search is on its way: no verdict yet.
    expect(screen.queryByTestId("search-empty")).toBeNull();
    const empty = await screen.findByTestId("search-empty", undefined, { timeout: 2000 });
    expect(empty.textContent).toContain("Nothing matches “zzzz”.");
    expect(empty.textContent).toContain("including hidden and folded sections");
    fireEvent.click(within(empty).getByRole("button", { name: "Clear search" }));
    expect((search() as HTMLInputElement).value).toBe("");
    expect(screen.queryByTestId("search-results")).toBeNull();
    expect(screen.getByTestId("unified-chat-list")).toBeTruthy();
  });
});

describe("ConversationSidebar — shared chats in message search", () => {
  it("shows whose chat a shared hit is and opens it through the shared door", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { spies } = await renderRail(null, {
        messageHits: [
          // In the loaded Team chats list, but matched by its text only.
          { conversation_id: "team-2", title: "Launch checklist", match_preview: "the <mark>rollback</mark> plan", matched_at: 1, user_email: "lee@example.com" },
          // Shared, but not in the list this page holds.
          { conversation_id: "team-9", title: "Postmortem", match_preview: "a <mark>rollback</mark>", matched_at: 7, user_email: "kim@example.com" },
        ],
      });
      type("rollback");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(350);
      });
      const hits = within(group("messages")).getAllByTestId("search-result");
      // Title, the owner's initial, then the matched text.
      expect(hits.map((h) => h.textContent)).toEqual(["Launch checklistLthe rollback plan", "PostmortemKa rollback"]);
      expect(hits.map((h) => within(h).getByTestId("row-status").getAttribute("title"))).toEqual([
        "Shared by lee@example.com",
        "Shared by kim@example.com",
      ]);

      fireEvent.click(hits[0]);
      expect(spies.onSelectTeamSharedConversation).toHaveBeenLastCalledWith(TEAM[1]);
      fireEvent.click(hits[1]);
      expect(spies.onSelectTeamSharedConversation).toHaveBeenLastCalledWith(
        expect.objectContaining({ id: "team-9", title: "Postmortem", user_email: "kim@example.com" }),
      );
      expect(spies.loadConversation).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not repeat a shared chat already listed by its title", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      await renderRail(null, {
        messageHits: [
          { conversation_id: "team-1", title: "Quarterly pacing plan", match_preview: "<mark>pacing</mark>", matched_at: 1, user_email: "dana@example.com" },
        ],
      });
      type("pacing");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(350);
      });
      expect(within(group("team")).getAllByTestId("team-shared-conv-row")).toHaveLength(1);
      expect(screen.queryByTestId("search-group-messages")).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("ConversationSidebar — one list for all chats", () => {
  // One list is the default: a layout that says nothing about it.
  const unifiedLayout = (patches: Parameters<typeof layoutWith>[0] = {}) => layoutWith(patches);
  // Activity order across own and team chats: team-2, own-1, team-1, own-patrol.
  const own = [
    conv({ id: "own-1", title: "Budget pacing review", updated_at: 300 }),
    conv({ id: "own-2", title: "Résumé notes", pinned: true, updated_at: 900 }),
    conv({ id: "own-3", title: "Pacing in the project", project_id: "p-1", updated_at: 800 }),
    conv({ id: "own-patrol", title: "Morning pacing patrol", updated_at: 100 }),
  ];
  const team = [
    conv({ id: "team-1", title: "Quarterly pacing plan", user_email: "dana@example.com", team_visible: true, updated_at: 200 }),
    conv({ id: "team-2", title: "Launch checklist", user_email: "lee@example.com", multiplayer: true, updated_at: 400 }),
  ];
  const overrides = { conversations: own, filteredConversations: own, teamSharedConversations: team };
  const team_ = team;

  it("lists own and teammates' chats together, by activity, with no Team chats section", async () => {
    const { spies } = await renderRail(unifiedLayout(), { overrides });
    expect(screen.queryByTestId("team-chats-section")).toBeNull();
    const list = screen.getByTestId("unified-chat-list");
    const titles = Array.from(list.querySelectorAll("[data-rail-row]")).map((b) => b.getAttribute("title"));
    expect(titles).toEqual([
      "Launch checklist (lee@example.com)",
      "Budget pacing review",
      "Quarterly pacing plan (dana@example.com)",
      "Morning pacing patrol",
    ]);
    // Pinned chats keep their own group; a project's chats stay in the project.
    expect(within(screen.getByTestId("pinned-section")).getByTitle("Résumé notes")).toBeTruthy();
    expect(within(list).queryByTitle("Pacing in the project")).toBeNull();
    // A teammate's row still opens as a team chat.
    fireEvent.click(within(list).getByTitle("Launch checklist (lee@example.com)"));
    expect(spies.onSelectTeamSharedConversation).toHaveBeenCalledWith(team[1]);
  });

  it("says what keeps a chat, and nothing at all for a plain temporary one", async () => {
    const mine = [
      ...own,
      // Kept by the server's sweep, so not temporary: shared by link, shared
      // with the team, Multiplayer, a timer.
      conv({ id: "own-link", title: "Shared by link", share_token: "tok", updated_at: 90 }),
      conv({ id: "own-team", title: "Shared with team", team_visible: true, updated_at: 80 }),
      conv({ id: "own-live", title: "My multiplayer", multiplayer: true, updated_at: 70 }),
      conv({ id: "own-timer", title: "Has a timer", updated_at: 60 }),
    ];
    await renderRail(unifiedLayout(), {
      overrides: {
        ...overrides,
        conversations: mine,
        filteredConversations: mine,
        scheduleKindByConv: new Map([["own-timer", "timer"]]),
      },
    });
    // A patrol chat is kept while its patrol is armed or paused: it carries
    // the patrol status.
    await waitFor(() => expect(statusOf("Morning pacing patrol")).toBe("patrol"));
    expect(statusOf("Shared by link")).toBe("link");
    expect(statusOf("Shared with team")).toBe("team");
    expect(statusOf("My multiplayer")).toBe("live");
    // A teammate's row: its state (Live), then whose it is.
    expect(statusOf("Launch checklist (lee@example.com)")).toBe("live");
    expect(within(railRow("Launch checklist (lee@example.com)")).getByTestId("row-owner").textContent).toBe("L");
    expect(describedBy(railRow("Launch checklist (lee@example.com)"))).toBe(
      "Multiplayer — teammates can chat here · Shared by lee@example.com",
    );
    // A chat a teammate shared with the team: only whose it is.
    expect(statusOf("Quarterly pacing plan (dana@example.com)")).toBeNull();
    expect(within(railRow("Quarterly pacing plan (dana@example.com)")).getByTestId("row-owner").textContent).toBe("D");
    // A plain temporary chat, with months left, says nothing: no mark and no
    // description. Neither does a pinned chat, nor one a timer keeps.
    for (const title of ["Budget pacing review", "Résumé notes", "Has a timer"]) {
      expect(railRow(title).querySelector('[data-testid="row-status"]')).toBeNull();
      expect(railRow(title).getAttribute("aria-describedby")).toBeNull();
    }
    // The list carries no standing explainer of temporary chats.
    expect(screen.queryByRole("button", { name: "About this list" })).toBeNull();
    expect(screen.queryByRole("button", { name: "About temporary chats" })).toBeNull();
  });

  it("goes by the server's listing for schedules, and quotes this deployment's retention period", async () => {
    const idle = Math.floor(Date.now() / 1000) - 25 * 86_400 - 3600;
    const mine = [
      conv({ id: "a", title: "Plain chat", updated_at: idle }),
      // Never opened on this page: only the listing knows a timer keeps it.
      conv({ id: "b", title: "Timer from last week", updated_at: idle, has_schedule: true }),
    ];
    const setConfirmBulkDelete = vi.fn();
    await renderRail(unifiedLayout(), {
      patrols: [],
      overrides: {
        ...overrides,
        conversations: mine,
        filteredConversations: mine,
        teamSharedConversations: [],
        serverConfig: { ...SERVER_CONFIG, conversationTtlDays: 30, conversationExpiryWarningDays: 14 },
        setConfirmBulkDelete,
      },
    });
    // 25 days idle of this deployment's 30: the plain chat is counted down,
    // the one a timer keeps is not.
    expect(statuses("expiring").map(rowTitle)).toEqual(["Plain chat"]);
    expect(statuses("expiring")[0].textContent).toBe("5d");
    expect(statusOf("Timer from last week")).toBeNull();
    // The Chats menu's bulk delete quotes the same period, and asks first.
    fireEvent.click(screen.getByTestId("recent-sort"));
    const item = screen.getByTestId("delete-temporary-chats");
    expect(item.textContent).toContain("30 days");
    expect(item.textContent).not.toContain("90 days");
    fireEvent.click(item);
    expect(setConfirmBulkDelete).toHaveBeenCalledWith(true);
  });

  // docs/CHAT-RETENTION.md: a temporary chat says how long it has left only
  // near the end of its window, in either layout, and the list says how many.
  describe("the warning before a temporary chat is deleted", () => {
    const DAY = 86_400;
    const idleFor = (days: number) => Math.floor(Date.now() / 1000) - days * DAY - 3600;
    const retention = { ...SERVER_CONFIG, conversationTtlDays: 90, conversationExpiryWarningDays: 14 };
    const mine = [
      conv({ id: "fresh", title: "Started today", updated_at: idleFor(0) }),
      conv({ id: "soon", title: "Idle for months", updated_at: idleFor(85) }),
      conv({ id: "last-day", title: "Nearly gone", updated_at: idleFor(90) }),
      // Just as idle, but kept: a pin and a timer both take a chat out.
      conv({ id: "kept-pin", title: "Pinned and idle", pinned: true, updated_at: idleFor(88) }),
      conv({ id: "kept-timer", title: "Timer and idle", has_schedule: true, updated_at: idleFor(88) }),
    ];
    const withRetention = (serverConfig: ServerConfig = retention) => ({
      overrides: { ...overrides, conversations: mine, filteredConversations: mine, serverConfig },
    });
    const expiringTitles = () => statuses("expiring").map(rowTitle);

    it("marks only the temporary chats inside the window, with the days left", async () => {
      await renderRail(unifiedLayout(), withRetention());
      expect(expiringTitles().sort()).toEqual(["Idle for months", "Nearly gone"]);
      const mark = statuses("expiring").find((m) => m.closest('[title="Idle for months"]'));
      expect(mark?.textContent).toBe("5d");
      expect(mark?.getAttribute("title")).toContain("deleted in 5 days");
      // The row keeps its name; the sentence is its description.
      const row = screen.getByRole("button", { name: "Idle for months" });
      expect(describedBy(row)).toContain("deleted in 5 days");
      const lastDay = statuses("expiring").find((m) => m.closest('[title="Nearly gone"]'));
      expect(lastDay?.textContent).toBe("1d");
      expect(lastDay?.getAttribute("title")).toContain("within a day");
      // A chat with months left says nothing yet; a pin or a timer keeps a
      // chat, however idle.
      for (const title of ["Started today", "Pinned and idle", "Timer and idle"]) {
        expect(railRow(title).querySelector('[data-testid="row-status"]')).toBeNull();
      }
    });

    it("counts them under the heading, and the count leads to the first", async () => {
      // jsdom has no layout: the jump scrolls the row into view, then focuses it.
      Element.prototype.scrollIntoView = vi.fn();
      await renderRail(unifiedLayout(), withRetention());
      const notice = screen.getByTestId("expiring-chats-notice");
      expect(notice.textContent).toContain("2 chats will be deleted soon.");
      fireEvent.click(notice);
      // The first of them as the list shows them (most recently active first).
      await waitFor(() => expect(document.activeElement?.getAttribute("title")).toBe("Idle for months"));
    });

    it("says “1 chat” in the singular, and nothing once none is close", async () => {
      const one = mine.filter((c) => c.id !== "last-day");
      await renderRail(unifiedLayout(), {
        overrides: { ...overrides, conversations: one, filteredConversations: one, serverConfig: retention },
      });
      expect(screen.getByTestId("expiring-chats-notice").textContent).toContain("1 chat will be deleted soon.");
      cleanup();
      const none = mine.filter((c) => c.id !== "last-day" && c.id !== "soon");
      await renderRail(unifiedLayout(), {
        overrides: { ...overrides, conversations: none, filteredConversations: none, serverConfig: retention },
      });
      expect(screen.queryByTestId("expiring-chats-notice")).toBeNull();
    });

    it("marks them in separate lists too", async () => {
      await renderRail(separateLayout(), withRetention());
      expect(expiringTitles().sort()).toEqual(["Idle for months", "Nearly gone"]);
      expect(screen.getByTestId("expiring-chats-notice")).toBeTruthy();
    });

    it("says nothing when the deployment turned the warning off, or nothing is close", async () => {
      await renderRail(unifiedLayout(), withRetention({ ...retention, conversationExpiryWarningDays: 0 }));
      expect(statuses("expiring")).toHaveLength(0);
      expect(screen.queryByTestId("expiring-chats-notice")).toBeNull();
      cleanup();
      await renderRail(unifiedLayout(), withRetention({ ...retention, conversationTtlDays: 365 }));
      expect(statuses("expiring")).toHaveLength(0);
      expect(screen.queryByTestId("expiring-chats-notice")).toBeNull();
    });

    it("still counts a temporary chat the person keeps at the top", async () => {
      // Kept at the top is not pinned: the chat is deleted like the rest, so
      // the notice must not go quiet about it.
      const layout = keepAtTop(unifiedLayout(), { kind: "chat", ref: "soon", title: "Idle for months" });
      await renderRail(layout, withRetention());
      expect(expiringTitles()).toEqual(["Nearly gone"]);
      expect(screen.getByTestId("expiring-chats-notice").textContent).toContain("2 chats will be deleted soon.");
    });

    it("says so when the person is close to the most temporary chats they keep", async () => {
      await renderRail(unifiedLayout(), withRetention({ ...retention, conversationUnpinnedCap: 4 }));
      expect(screen.getByTestId("temporary-cap-notice").textContent).toContain("You have 3 temporary chats. Past 4");
      cleanup();
      await renderRail(unifiedLayout(), withRetention({ ...retention, conversationUnpinnedCap: 500 }));
      expect(screen.queryByTestId("temporary-cap-notice")).toBeNull();
    });
  });

  it("is the default, with no saved layout at all", async () => {
    await renderRail(null, { overrides });
    expect(screen.getByTestId("unified-chat-list")).toBeTruthy();
    expect(screen.queryByTestId("team-chats-section")).toBeNull();
  });

  it("a person who hid Team chats sees none of them in the list, and search still finds them", async () => {
    await renderRail(unifiedLayout({ team: { hidden: true } }), { overrides });
    const list = screen.getByTestId("unified-chat-list");
    expect(within(list).queryByTestId("team-shared-conv-row")).toBeNull();
    expect(within(list).getByTitle("Budget pacing review")).toBeTruthy();
    type("launch");
    fireEvent.click(await within(group("team")).findByRole("button", { name: "Show Team chats in the sidebar" }));
    expect(getSidebarState().layout.sections.find((x) => x.id === "team")?.hidden).toBeFalsy();
    type("");
    expect(within(screen.getByTestId("unified-chat-list")).getAllByTestId("team-shared-conv-row")).toHaveLength(2);
  });

  it("“Own list” is one switch away: teammates' chats leave Chats for a Team chats section", async () => {
    const { spies } = await renderRail(separateLayout(), { overrides });
    // Chats holds the person's own chats only…
    expect(listTitles()).toEqual(["Budget pacing review", "Morning pacing patrol"]);
    // …and Team chats lists the teammates', counted in its heading.
    const team = screen.getByTestId("team-chats-section");
    expect(within(team).getByRole("button", { name: "Team chats (2)" })).toBeTruthy();
    expect(within(team).getAllByTestId("team-shared-conv-row").map((r) => r.getAttribute("title"))).toEqual([
      "Quarterly pacing plan (dana@example.com)",
      "Launch checklist (lee@example.com)",
    ]);
    // With no teammates' chats in Chats there is nothing for Mine or Shared to narrow.
    expect(screen.queryByTestId("chat-scope-shared")).toBeNull();
    expect(screen.queryByTestId("chat-scope-mine")).toBeNull();
    // A teammate's row opens as a team chat from there too.
    fireEvent.click(within(team).getByTitle("Launch checklist (lee@example.com)"));
    expect(spies.onSelectTeamSharedConversation).toHaveBeenCalledWith(team_[1]);
    // The heading folds the section.
    fireEvent.click(within(team).getByRole("button", { name: "Team chats (2)" }));
    expect(within(team).queryByTestId("team-shared-conv-row")).toBeNull();
  });

  it("a hidden Chats list is what hides teammates' chats, and search says so", async () => {
    await renderRail(unifiedLayout({ chats: { hidden: true } }), { overrides });
    expect(screen.queryByTestId("unified-chat-list")).toBeNull();
    type("launch");
    expect(within(group("team")).getByText("Launch", { exact: false })).toBeTruthy();
    expect(await within(group("team")).findByRole("button", { name: "Show Team chats in the sidebar" })).toBeTruthy();
    fireEvent.click(within(group("team")).getByRole("button", { name: "Show Team chats in the sidebar" }));
    expect(getSidebarState().layout.sections.find((x) => x.id === "chats")?.hidden).toBeFalsy();
    expect(getSidebarState().layout.separate_lists).toBeUndefined();
  });
  // ── The chips over Chats ──────────────────────────────────────────────────
  it("offers All, Mine, Shared and Patrols, and each narrows the list", async () => {
    await renderRail(unifiedLayout(), { overrides });
    const chips = () =>
      Array.from(screen.getByTestId("chat-scope").querySelectorAll("button")).map((b) => [
        b.textContent,
        b.getAttribute("aria-pressed"),
      ]);
    // Patrols arrives with the person's patrols.
    await waitFor(() =>
      expect(chips()).toEqual([
        ["All", "true"],
        ["Mine", "false"],
        ["Shared", "false"],
        ["Patrols", "false"],
      ]),
    );
    fireEvent.click(screen.getByTestId("chat-scope-mine"));
    expect(listTitles()).toEqual(["Budget pacing review", "Morning pacing patrol"]);
    fireEvent.click(screen.getByTestId("chat-scope-shared"));
    expect(listTitles()).toEqual(["Launch checklist (lee@example.com)", "Quarterly pacing plan (dana@example.com)"]);
    fireEvent.click(screen.getByTestId("chat-scope-patrols"));
    expect(listTitles()).toEqual(["Morning pacing patrol"]);
    expect(screen.getByTestId("chat-scope-patrols").getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(screen.getByTestId("chat-scope-all"));
    expect(listTitles()).toHaveLength(4);
  });

  it("shows a chip only when it would change the list", async () => {
    // Own chats only, none a patrol: nothing to narrow, so no chips at all.
    await renderRail(unifiedLayout(), { patrols: [], overrides: { ...overrides, teamSharedConversations: [] } });
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByTestId("chat-scope")).toBeNull();
    expect(listTitles()).toEqual(["Budget pacing review", "Morning pacing patrol"]);
    cleanup();
    resetPatrolCacheForTests();
    // A patrol among them, and no teammates' chats: All and Patrols.
    await renderRail(unifiedLayout(), { overrides: { ...overrides, teamSharedConversations: [] } });
    await screen.findByTestId("chat-scope-patrols");
    expect(screen.queryByTestId("chat-scope-mine")).toBeNull();
    expect(screen.queryByTestId("chat-scope-shared")).toBeNull();
    cleanup();
    resetPatrolCacheForTests();
    // Teammates' chats and no patrol: All, Mine and Shared.
    await renderRail(unifiedLayout(), { patrols: [], overrides });
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByTestId("chat-scope-mine")).toBeTruthy();
    expect(screen.getByTestId("chat-scope-shared")).toBeTruthy();
    expect(screen.queryByTestId("chat-scope-patrols")).toBeNull();
  });

  // ── Date groups ───────────────────────────────────────────────────────────
  describe("the dates Chats is grouped under", () => {
    const dateGroups = () =>
      Array.from(screen.getByTestId("unified-chat-list").querySelectorAll("[data-date-group]")).map((g) => [
        g.getAttribute("aria-label"),
        Array.from(g.querySelectorAll("[data-rail-row]")).map((r) => r.getAttribute("title")),
      ]);
    // Noon on a fixed day, so "today" never straddles midnight.
    const NOON = new Date(2026, 5, 17, 12, 0, 0);
    const at = (daysAgo: number, hour: number) =>
      Math.floor(new Date(2026, 5, 17 - daysAgo, hour).getTime() / 1000);
    const dated = [
      conv({ id: "d0", title: "This morning", updated_at: at(0, 9), created_at: at(40, 9) }),
      conv({ id: "d1", title: "Last night", updated_at: at(1, 22), created_at: at(1, 21) }),
      conv({ id: "d3", title: "Three days back", updated_at: at(3, 9), created_at: at(3, 8) }),
      // The first hour of the sixth day back is still "this week"; the last
      // hour of the seventh is not.
      conv({ id: "d6", title: "Six days back", updated_at: at(6, 1), created_at: at(6, 0) }),
      conv({ id: "d7", title: "A week back", updated_at: at(7, 23), created_at: at(7, 22) }),
      conv({ id: "d40", title: "Last month", updated_at: at(40, 9), created_at: at(41, 9) }),
    ];
    const datedOverrides = { conversations: dated, filteredConversations: dated, teamSharedConversations: [] };

    beforeEach(() => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      vi.setSystemTime(NOON);
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    it("Today, Yesterday, This week and Older, by when each chat was last active", async () => {
      await renderRail(unifiedLayout(), { patrols: [], overrides: datedOverrides });
      expect(dateGroups()).toEqual([
        ["Today", ["This morning"]],
        ["Yesterday", ["Last night"]],
        ["This week", ["Three days back", "Six days back"]],
        ["Older", ["A week back", "Last month"]],
      ]);
      // A date nobody has a chat under is not listed.
      cleanup();
      const two = [dated[0], dated[5]];
      await renderRail(unifiedLayout(), { patrols: [], overrides: { ...datedOverrides, conversations: two, filteredConversations: two } });
      expect(dateGroups().map(([label]) => label)).toEqual(["Today", "Older"]);
    });

    it("go by when each chat was started under “Newest first”, and step aside for A–Z", async () => {
      await renderRail(unifiedLayout(), { patrols: [], overrides: { ...datedOverrides, recentSort: "created" } });
      // "This morning" was started 40 days ago: under Newest first it is Older.
      expect(dateGroups()).toEqual([
        ["Yesterday", ["Last night"]],
        ["This week", ["Three days back", "Six days back"]],
        ["Older", ["A week back", "This morning", "Last month"]],
      ]);
      cleanup();
      await renderRail(unifiedLayout(), { patrols: [], overrides: { ...datedOverrides, recentSort: "title" } });
      expect(dateGroups()).toEqual([]);
      expect(listTitles()).toEqual([
        "A week back",
        "Last month",
        "Last night",
        "Six days back",
        "This morning",
        "Three days back",
      ]);
    });

    it("are off for a person who turned “Group chats by date” off: one run, same order", async () => {
      await renderRail({ ...BUILTIN_LAYOUT, dates_off: true }, { patrols: [], overrides: datedOverrides });
      expect(dateGroups()).toEqual([]);
      expect(listTitles()).toEqual([
        "This morning",
        "Last night",
        "Three days back",
        "Six days back",
        "A week back",
        "Last month",
      ]);
    });
  });

  // ── One status per row ────────────────────────────────────────────────────
  describe("one status per row", () => {
    const DAY = 86_400;
    const now = () => Math.floor(Date.now() / 1000);
    // Each chat is several things at once; its row says the most pressing.
    const mine = () => [
      conv({ id: "s-work", title: "Being answered", multiplayer: true, team_visible: true, updated_at: now() }),
      conv({ id: "s-exp", title: "Nearly deleted", lockdown: true, updated_at: now() - 88 * DAY - 3600 }),
      conv({ id: "s-patrol", title: "On patrol", multiplayer: true, team_visible: true, share_token: "t", updated_at: now() }),
      conv({ id: "s-paused", title: "Patrol on hold", team_visible: true, share_token: "t", updated_at: now() }),
      conv({ id: "s-live", title: "Live room", multiplayer: true, team_visible: true, share_token: "t", lockdown: true, updated_at: now() }),
      conv({ id: "s-team", title: "With the team", team_visible: true, share_token: "t", lockdown: true, updated_at: now() }),
      conv({ id: "s-link", title: "By link", share_token: "t", lockdown: true, updated_at: now() }),
      conv({ id: "s-sealed", title: "Sealed", lockdown: true, updated_at: now() }),
      conv({ id: "s-plain", title: "Plain", updated_at: now() }),
    ];
    const render = async (layout: unknown) => {
      const list = mine();
      await renderRail(layout, {
        patrols: [
          { conversation_id: "s-work", status: "active", next_run_at: now() + 3600 },
          { conversation_id: "s-patrol", status: "active", next_run_at: now() + 19 * 3600 },
          { conversation_id: "s-paused", status: "paused" },
        ],
        overrides: {
          conversations: list,
          filteredConversations: list,
          teamSharedConversations: [],
          streamingConvs: new Set(["s-work"]),
          serverConfig: { ...SERVER_CONFIG, conversationTtlDays: 90, conversationExpiryWarningDays: 14 },
        },
      });
      // The patrols have answered.
      await waitFor(() => expect(describedBy(railRow("On patrol"))).toContain("Patrol"));
    };

    it("shows the most pressing: working, days left, patrol, Live, team, link, sealed", async () => {
      await render(null);
      expect(
        ["Being answered", "Nearly deleted", "On patrol", "Patrol on hold", "Live room", "With the team", "By link", "Sealed", "Plain"].map(
          (title) => [title, statusOf(title)],
        ),
      ).toEqual([
        ["Being answered", "working"],
        ["Nearly deleted", "expiring"],
        ["On patrol", "patrol"],
        ["Patrol on hold", "patrol-paused"],
        ["Live room", "live"],
        ["With the team", "team"],
        ["By link", "link"],
        ["Sealed", "sealed"],
        ["Plain", null],
      ]);
      // One mark each, never two.
      for (const row of document.querySelectorAll('[data-testid="unified-chat-list"] [data-rail-row]')) {
        expect(row.querySelectorAll('[data-testid="row-status"]').length).toBeLessThanOrEqual(1);
      }
      // A patrol says when it checks next, to one unit, or that it is paused.
      expect(railRow("On patrol").querySelector('[data-testid="row-status"]')?.textContent).toBe("19h");
      expect(describedBy(railRow("On patrol"))).toBe("Patrol — next check in 19h");
      expect(railRow("Patrol on hold").querySelector('[data-testid="row-status"]')?.textContent).toBe("Paused");
      expect(describedBy(railRow("Patrol on hold"))).toBe("Patrol — paused");
      expect(railRow("Nearly deleted").querySelector('[data-testid="row-status"]')?.textContent).toBe("2d");
      expect(describedBy(railRow("Being answered"))).toBe("Working…");
      // The status never renames the row.
      expect(screen.getByRole("button", { name: "On patrol" })).toBe(railRow("On patrol"));
    });

    it("draws none with “Status on rows” off, and still tells a screen reader", async () => {
      await render({ ...BUILTIN_LAYOUT, status_off: true });
      expect(screen.getByTestId("rail-lists").querySelector('[data-testid="row-status"]')).toBeNull();
      expect(describedBy(railRow("On patrol"))).toBe("Patrol — next check in 19h");
      expect(describedBy(railRow("Live room"))).toBe("Multiplayer — teammates can chat here");
      expect(railRow("Plain").getAttribute("aria-describedby")).toBeNull();
    });
  });
});

// ── The + menu ──────────────────────────────────────────────────────────────
describe("ConversationSidebar — the pen and the + menu", () => {
  const openNew = () => fireEvent.click(screen.getByTestId("new-menu-button"));
  const newItems = () =>
    within(screen.getByRole("menu", { name: "New" }))
      .getAllByRole("menuitem")
      .map((i) => i.getAttribute("data-testid"));

  beforeEach(() => openPatrolWizard.mockClear());

  it("starts a chat from the pen", async () => {
    const clearConversation = vi.fn();
    await renderRail(null, { overrides: { clearConversation } });
    fireEvent.click(screen.getByTestId("new-chat-button"));
    expect(clearConversation).toHaveBeenCalledWith(undefined);
  });

  it("lists a chat, a patrol and a background task, then a project, and starts each", async () => {
    const clearConversation = vi.fn();
    const onNewTask = vi.fn();
    const onCreateProject = vi.fn();
    await renderRail(null, { overrides: { clearConversation, onNewTask, onCreateProject } });
    openNew();
    expect(newItems()).toEqual(["new-menu-chat", "new-menu-patrol", "new-menu-task", "new-menu-project"]);
    // The shortcut for a new chat rides on its item.
    expect(screen.getByTestId("new-menu-chat").textContent).toContain("Ctrl+Shift+O");

    fireEvent.click(screen.getByTestId("new-menu-chat"));
    expect(clearConversation).toHaveBeenCalledWith(undefined);
    expect(screen.queryByRole("menu", { name: "New" })).toBeNull();

    openNew();
    fireEvent.click(screen.getByTestId("new-menu-patrol"));
    // The wizard is handed the + button, to give the focus back to.
    expect(openPatrolWizard).toHaveBeenCalledWith(screen.getByTestId("new-menu-button"));

    openNew();
    fireEvent.click(screen.getByTestId("new-menu-task"));
    expect(onNewTask).toHaveBeenCalledTimes(1);

    openNew();
    fireEvent.click(screen.getByTestId("new-menu-project"));
    expect(onCreateProject).toHaveBeenCalledTimes(1);
  });

  it("leaves out the background task for an account that cannot make one", async () => {
    await renderRail(null, { overrides: { onNewTask: null } });
    openNew();
    expect(newItems()).toEqual(["new-menu-chat", "new-menu-patrol", "new-menu-project"]);
  });

  it("offers a sealed chat where the deployment has them", async () => {
    const clearConversation = vi.fn();
    await renderRail(null, {
      overrides: { clearConversation, serverConfig: { ...SERVER_CONFIG, lockdownAvailable: true } },
    });
    openNew();
    expect(newItems()).toEqual(["new-menu-chat", "new-menu-patrol", "new-menu-task", "new-menu-project", "new-menu-sealed"]);
    fireEvent.click(screen.getByTestId("new-menu-sealed"));
    expect(clearConversation).toHaveBeenCalledWith({ lockdown: true });
  });

  it("where every chat is sealed, the pen starts one and the menu has no second way", async () => {
    const clearConversation = vi.fn();
    await renderRail(null, {
      overrides: { clearConversation, serverConfig: { ...SERVER_CONFIG, lockdownAvailable: true, lockdownOnly: true } },
    });
    fireEvent.click(screen.getByTestId("new-chat-button"));
    expect(clearConversation).toHaveBeenCalledWith({ lockdown: true });
    openNew();
    expect(screen.queryByTestId("new-menu-sealed")).toBeNull();
  });
});

// ── The ⋯ menus ─────────────────────────────────────────────────────────────
// One order on every row (docs/SIDEBAR.md): where it lives, where fleet
// opens, make something, pass it on, put it away. A group with nothing to
// offer is left out.
describe("ConversationSidebar — the ⋯ menus keep one order", () => {
  const menuItems = () => screen.getAllByRole("menuitem").map((el) => el.textContent?.trim());
  const openMenu = (title: string) =>
    fireEvent.click(screen.getByRole("button", { name: `Conversation options for ${title}` }));
  const closeMenu = () => fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });

  it("the person's own chat: every group, and each item does its one thing", async () => {
    const h = {
      togglePin: vi.fn(async () => {}),
      toggleArchive: vi.fn(async () => {}),
      downloadConversation: vi.fn(async () => {}),
      promoteConversation: vi.fn(async () => {}),
      savePromptFromConversation: vi.fn(),
      setPendingDeleteConversation: vi.fn(),
      openShareDialog: vi.fn(),
      openTransferDialog: vi.fn(),
      onOpenPatrols: vi.fn(),
    };
    await renderRail(null, { overrides: h });
    const chat = OWN[0];
    openMenu(chat.title);
    // The two sidebar items arrive once the account is known to be able to
    // save its sidebar.
    await waitFor(() =>
      expect(menuItems()).toEqual([
        "Pin",
        "Move to the top",
        "Rename",
        "Move to project",
        "Labels",
        "Set as start page",
        "Add a patrol…",
        "Make a background task…",
        "Save as workflow…",
        "Share…",
        "Download chat…",
        "Transfer ownership…",
        "Archive",
        "Delete…",
      ]),
    );
    const pick = (name: string) => {
      if (!screen.queryByRole("menu")) openMenu(chat.title);
      fireEvent.click(screen.getByRole("menuitem", { name }));
      // Choosing closes the menu.
      expect(screen.queryByRole("menu")).toBeNull();
    };
    pick("Pin");
    expect(h.togglePin).toHaveBeenCalledWith(chat);
    pick("Add a patrol…");
    expect(h.onOpenPatrols).toHaveBeenCalledWith(chat);
    pick("Make a background task…");
    expect(h.promoteConversation).toHaveBeenCalledWith(chat);
    pick("Save as workflow…");
    expect(h.savePromptFromConversation).toHaveBeenCalledWith(chat);
    pick("Share…");
    expect(h.openShareDialog).toHaveBeenCalledWith(chat);
    pick("Download chat…");
    expect(h.downloadConversation).toHaveBeenCalledWith(chat);
    pick("Transfer ownership…");
    expect(h.openTransferDialog).toHaveBeenCalledWith(chat);
    pick("Archive");
    expect(h.toggleArchive).toHaveBeenCalledWith(chat, true);
    pick("Delete…");
    expect(h.setPendingDeleteConversation).toHaveBeenCalledWith({ id: chat.id, title: chat.title });
  });

  it("renames in place from the menu", async () => {
    const renameConversation = vi.fn(async () => true);
    await renderRail(null, { overrides: { renameConversation } });
    openMenu("Budget pacing review");
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename" }));
    const field = screen.getByRole("textbox", { name: "Rename Budget pacing review" });
    expect(document.activeElement).toBe(field);
    fireEvent.change(field, { target: { value: "  Budget review  " } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(renameConversation).toHaveBeenCalledWith("own-1", "Budget review");
  });

  it("a patrol chat opens its patrols; a sealed chat is offered neither a patrol nor a transfer", async () => {
    const sealed = conv({ id: "own-sealed", title: "Board numbers", lockdown: true });
    const list = [...OWN, sealed];
    await renderRail(null, {
      overrides: { conversations: list, filteredConversations: list, onOpenPatrols: vi.fn(), openTransferDialog: vi.fn() },
    });
    await waitFor(() => expect(statusOf("Morning pacing patrol")).toBe("patrol"));
    openMenu("Morning pacing patrol");
    expect(screen.getByTestId("chat-menu-patrol").textContent).toContain("Patrols…");
    closeMenu();
    openMenu("Board numbers");
    expect(screen.queryByTestId("chat-menu-patrol")).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Transfer ownership…" })).toBeNull();
    expect(screen.getByRole("menuitem", { name: "Share…" })).toBeTruthy();
  });

  it("offers no patrol item where the page cannot open a chat's patrols", async () => {
    await renderRail(null);
    openMenu("Budget pacing review");
    expect(screen.getByRole("menuitem", { name: "Make a background task…" })).toBeTruthy();
    expect(screen.queryByTestId("chat-menu-patrol")).toBeNull();
  });

  it("Move to the top keeps a chat above the lists, where it is listed once", async () => {
    await renderRail(null, { overrides: { teamSharedConversations: [] } });
    openMenu("Budget pacing review");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Move to the top" }));
    const kept = getSidebarState().layout.items.find((i) => i.kind === "chat" && i.ref === "own-1");
    expect(kept).toBeTruthy();
    expect(kept?.hidden).toBeFalsy();
    // It is among the top items now, and no longer in Chats.
    expect(screen.getAllByTestId("rail-shortcut").map((r) => r.textContent)).toEqual(
      expect.arrayContaining([expect.stringContaining("Budget pacing review")]),
    );
    expect(listTitles()).toEqual(["Morning pacing patrol"]);
  });

  it("Set as start page makes fleet open to a chat without moving it, and takes it back", async () => {
    await renderRail(null, { overrides: { teamSharedConversations: [] } });
    openMenu("Budget pacing review");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Set as start page" }));
    const layout = getSidebarState().layout;
    const home = layout.items.find((i) => i.id === layout.home);
    expect(home).toMatchObject({ kind: "chat", ref: "own-1", hidden: true });
    // Not kept at the top: it stays where it was.
    expect(listTitles()).toContain("Budget pacing review");
    expect(screen.queryByTestId("rail-shortcut")).toBeNull();

    openMenu("Budget pacing review");
    fireEvent.click(await screen.findByRole("menuitem", { name: "Remove as start page" }));
    expect(getSidebarState().layout.home ?? "").toBe("");
  });

  it("a teammate's chat: only what a viewer can do with someone else's", async () => {
    await renderRail(null, { overrides: { canPinTeamChats: true, canTransferTeamChats: true } });
    // A Multiplayer chat the person takes part in.
    openMenu("Launch checklist");
    await waitFor(() =>
      expect(menuItems()).toEqual([
        "Pin",
        "Move to the top",
        "Set as start page",
        "Copy link",
        "Download chat…",
        "Transfer ownership…",
      ]),
    );
    // Nothing of the owner's: no rename, filing, labels, archive or delete.
    for (const name of ["Rename", "Move to project", "Labels", "Archive", "Delete…", "Share…"]) {
      expect(screen.queryByRole("menuitem", { name })).toBeNull();
    }
  });

  it("a teammate's read-only chat, for a member who is no admin: the sidebar items and its link", async () => {
    await renderRail(null);
    openMenu("Quarterly pacing plan");
    await waitFor(() => expect(menuItems()).toEqual(["Move to the top", "Set as start page", "Copy link"]));
  });
});
