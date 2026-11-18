import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { ConversationSidebar } from "./ConversationSidebar";
import { filterConversations } from "./conversationOrganization";
import { DEFAULT_BRANDING } from "@/app/lib/useClientConfig";
import type { ConversationSummary, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { BUILTIN_LAYOUT, updateSection, type SectionId } from "@/app/shared/sidebar/layout";
import { getSidebarState, loadSidebar, resetSidebarStoreForTests } from "@/app/shared/sidebar/store";
import { resetPatrolCacheForTests } from "@/app/shared/sidebar/targets";

// The rail's search looks everywhere, whatever the sidebar is set to show
// (docs/SIDEBAR-SEARCH.md). The reports this guards: a search that never
// looked at Team chats, and results that stayed out of sight because the
// section holding them was folded or hidden — "my chats are gone".

vi.mock("@/app/shared/lib/account", () => ({
  fetchMe: () => Promise.resolve({ email: "me@example.com", role: "member", team_id: "", admin: false }),
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

function stubFetch(layout: unknown, messageHits: unknown[] = []) {
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
        return new Response(JSON.stringify({ patrols: [{ conversation_id: "own-patrol" }] }), { status: 200 });
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
  opts: { query?: string; messageHits?: unknown[]; overrides?: Record<string, unknown> } = {},
) {
  const calls = stubFetch(layout, opts.messageHits);
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
    expect(team.getByText("dana")).toBeTruthy();

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
    await waitFor(() => expect(screen.getAllByTestId("patrol-mark")).toHaveLength(1));
    expect(screen.getByTestId("patrol-mark").closest("[data-rail-row]")?.getAttribute("title")).toBe(
      "Morning pacing patrol",
    );
    const mark = screen.getByTestId("patrol-mark");
    expect(mark.getAttribute("title")).toContain("schedule");
    // The mark does not rename the row: it is the row's description.
    const row = screen.getByRole("button", { name: "Morning pacing patrol" });
    expect(document.getElementById(row.getAttribute("aria-describedby") ?? "")?.textContent).toContain("schedule");
    type("morning");
    expect(within(group("chats")).getByTestId("patrol-mark")).toBeTruthy();
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
    await waitFor(() => expect(screen.getAllByTestId("patrol-mark")).toHaveLength(1));
    expect(screen.getByTestId("patrol-mark").closest("[data-rail-row]")?.getAttribute("title")).toBe(
      "Budget pacing review",
    );
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
      expect(hits.map((h) => h.textContent)).toEqual(["Launch checklistleethe rollback plan", "Postmortemkima rollback"]);

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

  it("marks the person's temporary chats, and only those", async () => {
    const mine = [
      ...own,
      // Kept by the server's sweep, so not temporary: shared by link, shared
      // with the team, Multiplayer.
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
    const list = screen.getByTestId("unified-chat-list");
    const titlesOf = (testId: string) =>
      Array.from(list.querySelectorAll(`[data-testid="${testId}"]`)).map((m) =>
        m.closest("[data-rail-row]")?.getAttribute("title"),
      );
    // A patrol chat is kept while its patrol is armed or paused: it carries
    // the patrol mark, never the hourglass.
    await waitFor(() => expect(titlesOf("patrol-mark")).toEqual(["Morning pacing patrol"]));
    expect(titlesOf("temporary-mark")).toEqual(["Budget pacing review"]);
    expect(within(list).getAllByTestId("temporary-mark")[0].getAttribute("title")).toContain("14 days");
    const row = screen.getByRole("button", { name: "Budget pacing review" });
    expect(document.getElementById(row.getAttribute("aria-describedby") ?? "")?.textContent).toContain("14 days");
    // Pinned chats are kept, so they carry no mark.
    expect(screen.getByTestId("pinned-section").querySelector('[data-testid="temporary-mark"]')).toBeNull();
    expect(screen.getByRole("button", { name: "About this list" })).toBeTruthy();
  });

  it("goes by the server's listing for schedules, and quotes this deployment's retention period", async () => {
    const mine = [
      conv({ id: "a", title: "Plain chat", updated_at: 30 }),
      // Never opened on this page: only the listing knows a timer keeps it.
      conv({ id: "b", title: "Timer from last week", updated_at: 20, has_schedule: true }),
    ];
    await renderRail(unifiedLayout(), {
      overrides: {
        ...overrides,
        conversations: mine,
        filteredConversations: mine,
        serverConfig: { ...SERVER_CONFIG, conversationTtlDays: 30 },
      },
    });
    const marks = screen.getAllByTestId("temporary-mark");
    expect(marks.map((m) => m.closest("[data-rail-row]")?.getAttribute("title"))).toEqual(["Plain chat"]);
    expect(marks[0].getAttribute("title")).toContain("30 days");
    expect(marks[0].getAttribute("title")).not.toContain("14 days");
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

  it("separate lists are one switch away: no marks, a Temporary group, a Team chats section", async () => {
    await renderRail(separateLayout(), { overrides });
    expect(screen.queryByTestId("unified-chat-list")).toBeNull();
    expect(screen.queryByTestId("temporary-mark")).toBeNull();
    expect(screen.getByTestId("team-chats-section")).toBeTruthy();
    expect(screen.getByRole("button", { name: "About temporary chats" })).toBeTruthy();
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
});

