import { t as tr } from "@/app/lib/i18n";
// The sidebar's lists (docs/SIDEBAR.md), inside the shared NavRail frame: the
// search / new-chat / + row, then Pinned, Projects and Chats in the person's
// own order, all in the rail's one scroller. A row is its title with at most
// one quiet status at the right edge (RowStatus) and a ⋯ menu that appears on
// hover or focus; a touch screen always shows the menu button.
//
// It is presentational + event-forwarding: the lists and the mutation
// handlers are threaded in via props. Chat passes its own live state
// (ChatExperience); every other page passes what useSidebarLibrary reads, so
// the same sidebar is on every page (AppSidebar.tsx). Per-row accessible names
// ("Conversation options for <title>", "Open project <name>", …) are stable:
// the e2e suites reach rows and menus through them.

import { useEffect, useMemo, useRef, useState } from "react";
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { navigate } from "@/app/lib/navigation";
import { NavRail, type RailCollapse, type RailView } from "@/app/shared/ui/NavRail";
import { RAIL_HEADING_ACTION_CLASS, RAIL_HEADING_CLASS, railIconClass } from "@/app/shared/ui/railStyles";
import { Kbd } from "@/components/ui/kbd";
import { Icon } from "./Icon";
import {
  CHAT_SCOPES,
  MAX_CHAT_ORDER,
  PINNED_ORDER_SCOPE,
  dateGroupLabel,
  groupByDate,
  pinnedEntries,
  pinnedGroup,
  projectOrderScope,
  sortRecent,
  type ChatScope,
  type RecentSort,
  deriveLabels,
  filterConversations,
  isFiltering as computeIsFiltering,
  matchesSearch,
  searchTokens,
  pinnedUnfiled,
  projectGroups,
  railProjectEmptyState,
  recentUnfiled,
  type LabelSummary,
} from "./conversationOrganization";
import type { ConversationSummary, PendingDeleteConversation, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { previewNodes, type SearchResult } from "./SearchBar";
import { isTemporaryChat } from "./deleteAllUnpinnedScope";
import { expiresInDays, retentionPolicy, useNowSeconds } from "./chatRetention";
import { TeamGlyph } from "./ShareGlyphs";
import { ChatReorderList, ReorderDoneButton } from "./ChatReorderList";
import { RowStatus, type RowStatusInput } from "@/app/shared/sidebar/RowStatus";
import { openPatrolWizard } from "@/app/launchpad/patrolWizard";
import { sectionOf, topItems, updateSection, type SectionId } from "@/app/shared/sidebar/layout";
import { updateSidebarLayout, useSidebar } from "@/app/shared/sidebar/store";
import {
  SidebarTargetsContext,
  patrolsByConversation,
  usePatrols,
  type SidebarPins,
  type SidebarTargets,
} from "@/app/shared/sidebar/targets";
import { useCanCustomize } from "@/app/shared/sidebar/useCanCustomize";
import { BulkBar } from "./sidebar/BulkBar";
import { ChatRow } from "./sidebar/ChatRow";
import { RetentionNotices } from "./sidebar/RetentionNotices";
import {
  CONV_DRAG_MIME,
  RENAME_INPUT_CLASS,
  ROW_ACTIONS_REVEAL,
  ROW_CLEAR_FOR_ACTIONS,
  STATUS_YIELDS_TO_ACTIONS,
} from "./sidebar/classes";
import { ChatsMenu, NewMenu, SectionHeading } from "./sidebar/railMenus";
import { ConversationKebab, ProjectKebab, TeamChatKebab } from "./sidebar/rowMenus";
import { ResultTag, SearchGroup, markMatches } from "./sidebar/searchParts";

// How many projects the Projects section lists before "All N projects".
const PROJECTS_SHOWN = 8;

// What a platform's search shortcut says about its new-chat chord.
function newChatChord(searchShortcut: string): string {
  return searchShortcut.startsWith("⌘") ? "⌘⇧O" : "Ctrl+Shift+O";
}

// ── Main sidebar ─────────────────────────────────────────────────────────────
const NO_STREAMING: ReadonlySet<string> = new Set();
const NO_SELECTION: ReadonlySet<string> = new Set();
const noop = () => {};

export function ConversationSidebar({
  activeView = "chat",
  opsCount,
  sidebarOpen,
  setSidebarOpen,
  collapse,
  branding,
  serverConfig,
  userEmail,
  onSignOut,
  clearConversation,
  sidebarQuery,
  setSidebarQuery,
  searchRef,
  filterLabels,
  setFilterLabels,
  isLoadingHistory,
  conversations,
  filteredConversations,
  activeConversationId,
  focusedConversationId,
  renameSignal,
  loadConversation,
  streamingConvs = NO_STREAMING,
  togglePin,
  chatOrders,
  onReorderChats,
  recentSort = "activity",
  onRecentSortChange,
  toggleArchive,
  renameConversation,
  downloadConversation,
  promoteConversation,
  savePromptFromConversation,
  onOpenPatrols,
  setPendingDeleteConversation,
  setConversationLabels,
  openShareDialog,
  openTransferDialog,
  canTransferTeamChats,
  canPinTeamChats = false,
  readOnly = false,
  archivedConversations,
  showArchived,
  setShowArchived,
  setConfirmBulkDelete,
  selectMode = false,
  selectedIds = NO_SELECTION,
  onToggleSelection = noop,
  onEnterSelectMode,
  onExitSelectMode = noop,
  onBulkDelete = noop,
  onBulkPin = noop,
  onBulkAddLabel = noop,
  searchShortcut,
  newChatShortcut = true,
  onCreateProject,
  onNewTask,
  onOpenProjectHome,
  onPinProject,
  onNewChatInProject,
  onRenameProject,
  onDeleteProject,
  projects,
  teamSharedChatCounts,
  teamSharedConversations = [],
  onSelectTeamSharedConversation,
  onMoveToProject,
  activeProjectId = null,
  projectsLoaded = false,
  teamSharedLoaded = false,
  archivedLoaded = false,
  onSetDashboardView,
  scheduleKindByConv,
  notice,
}: {
  // Which page the sidebar is on: Chat by default. On any other page a chat
  // or a project is never "the open one".
  activeView?: RailView;
  // Background tasks running now, for that row's quiet count.
  opsCount?: number;
  sidebarOpen: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
  // Shared rail collapse state (owned by the page via useRailCollapse).
  // ≥sm the wide-only content below hides when collapsed; the <sm drawer
  // always shows everything.
  collapse: RailCollapse;
  branding: ClientBranding;
  serverConfig: ServerConfig;
  userEmail: string;
  onSignOut: () => void;
  clearConversation: (opts?: { lockdown?: boolean }) => void;
  sidebarQuery: string;
  setSidebarQuery: Dispatch<SetStateAction<string>>;
  searchRef: RefObject<HTMLInputElement | null>;
  filterLabels: string[];
  setFilterLabels: Dispatch<SetStateAction<string[]>>;
  isLoadingHistory: boolean;
  conversations: ConversationSummary[];
  filteredConversations: ConversationSummary[];
  activeConversationId: string | null;
  // Keyboard j/k cursor position (owned by ChatExperience). null when no row
  // is focused.
  focusedConversationId: string | null;
  // Rename trigger from the parent's `r` shortcut: a monotonically-bumped nonce
  // paired with the target id. The sidebar owns the inline-edit state
  // (editingId), so the parent asks for a rename via this signal rather than
  // reaching into that state. null before the first request.
  renameSignal: { id: string; nonce: number } | null;
  loadConversation: (
    conversationId: string,
    options?: { preserveScroll?: boolean },
  ) => Promise<void>;
  // The chats with a reply being written. Chat holds the live streams;
  // another page has none to give, so its rows show no working mark.
  streamingConvs?: ReadonlySet<string>;
  // Resolves to whether the pin stuck. The rail ignores it (a failed pin
  // reverts itself via a refresh); the "Pin it and remove" confirm does not,
  // because there the pin is what keeps the copy's promise.
  togglePin: (conversation: ConversationSummary) => Promise<unknown>;
  // The person's own orders by scope ("pinned", "project:<id>") and the save
  // for a new one (docs/CHAT-ORDER.md). The pinned order names chats by id
  // and pinned projects as "project:<id>". Optional: without them the rail
  // keeps the server's order and offers no reorder mode.
  chatOrders?: Readonly<Record<string, readonly string[]>>;
  onReorderChats?: (scope: string, ids: string[]) => void;
  // How the Chats list is sorted, and the change for it. Without the handler
  // the list stays most recently active first, with no sort control.
  recentSort?: RecentSort;
  onRecentSortChange?: (sort: RecentSort) => void;
  toggleArchive: (
    conversation: ConversationSummary,
    archived: boolean,
  ) => Promise<void>;
  renameConversation: (
    conversationId: string,
    nextTitle: string,
  ) => Promise<boolean>;
  downloadConversation: (conversation: ConversationSummary) => Promise<void>;
  promoteConversation: (conversation: ConversationSummary) => Promise<void>;
  savePromptFromConversation: (conversation: ConversationSummary) => void;
  // Opens the chat with its patrols dialog up ("Add a patrol…").
  onOpenPatrols?: (conversation: ConversationSummary) => void;
  setPendingDeleteConversation: Dispatch<
    SetStateAction<PendingDeleteConversation | null>
  >;
  setConversationLabels: (conversationId: string, labels: string[]) => void;
  // Opens the Share dialog for one conversation — link scope and team scope
  // in one place (ADR-0057).
  openShareDialog: (conversation: ConversationSummary) => void;
  // Opens the Transfer ownership dialog for one conversation. The owner's
  // row menu always offers it; a teammate's chat offers it to an admin.
  openTransferDialog: (conversation: ConversationSummary) => void;
  canTransferTeamChats: boolean;
  // Whether the person may pin a teammate's chat for themselves: true once
  // the account is known not to be a Viewer (whose writes the API refuses),
  // the rule reordering uses.
  canPinTeamChats?: boolean;
  // A read-only (Viewer) account, whose every change the API refuses
  // (rejectViewerWrites): the sidebar offers no change at all — no new chat,
  // patrol, task or project, no pin, rename, label, move, share, transfer,
  // archive or delete, no selecting, and nothing to drag. It still lists,
  // searches and opens everything the account can read.
  readOnly?: boolean;
  archivedConversations: ConversationSummary[];
  showArchived: boolean;
  setShowArchived: Dispatch<SetStateAction<boolean>>;
  setConfirmBulkDelete: Dispatch<SetStateAction<boolean>>;
  // Select mode (#279): entered from the Chats menu's "Select chats…" — that
  // is the only way checkboxes appear. Bulk actions live in the compact bar
  // above the account menu; Escape/Cancel exit. Select mode is Chat's: on
  // another page "Select chats…" hands over to Chat (onEnterSelectMode), so
  // the rest are left out there and the sidebar is never in select mode.
  selectMode?: boolean;
  selectedIds?: ReadonlySet<string>;
  onToggleSelection?: (id: string) => void;
  onEnterSelectMode: (id?: string) => void;
  onExitSelectMode?: () => void;
  onBulkDelete?: () => void;
  onBulkPin?: () => void;
  onBulkAddLabel?: (label: string) => void;
  // Platform-appropriate hint for the search field ("⌘K" / "Ctrl+K").
  searchShortcut: string;
  // Whether this page binds the new-chat chord (Chat does), so the + menu
  // may quote it.
  newChatShortcut?: boolean;
  // Opens the new-project form (the + menu's "New project").
  onCreateProject: () => void;
  // Starts a background task (the + menu). Defaults to the Background tasks
  // page with its new-task form open; null leaves the item out (a read-only
  // account on that page).
  onNewTask?: (() => void) | null;
  // Opens the project's HOME (title · chats · sources · instructions ·
  // per-project settings) in the main pane — a project row's name click and
  // the menu's "Project settings…" (settings=true opens the dialog too).
  onOpenProjectHome: (projectID: string, settings?: boolean) => void;
  // Pin/unpin a project (owner-only, from the project menu): a pinned
  // project is listed in Pinned, with the pinned chats.
  onPinProject: (projectID: string, pinned: boolean) => void;
  // Start a chat in this project (the project menu's "New chat").
  onNewChatInProject?: (projectID: string) => void;
  // Inline rename from the rail (project menu → Rename); the parent PATCHes
  // just the name.
  onRenameProject: (projectID: string, name: string) => void;
  // Delete from the rail (project menu); the parent confirms + DELETEs —
  // the server detaches the project's chats rather than deleting them.
  onDeleteProject: (projectID: string) => void;
  projects: Project[];
  // Per-project count of the team-shared chats OTHER members contributed —
  // i.e. the size of that project's home "Team" section. Keyed by project id;
  // a loaded map omits projects with none, so a missing key means zero.
  //
  // `undefined` for the whole map means "not known yet" (still loading, or the
  // read failed), and the rail then falls back to copy that asserts no number.
  // The shared chats themselves stay OUT of the rail (ADR-0057: the project
  // home is the one discovery surface) — this is a count, not a listing.
  teamSharedChatCounts?: Record<string, number>;
  // Shared and Multiplayer conversations from teammates (GET /conversations?scope=shared).
  teamSharedConversations?: ConversationSummary[];
  onSelectTeamSharedConversation?: (conversation: ConversationSummary) => void;
  // Re-files a conversation into a project ("" = unfile) — drag-and-drop and
  // the menu's "Move to project" both land here; the parent owns the
  // optimistic state + POST.
  onMoveToProject: (conversationId: string, projectID: string) => void;
  // The project whose home is open in the main pane.
  activeProjectId?: string | null;
  // The projects list has loaded once (an empty list is then a real "none").
  projectsLoaded?: boolean;
  // The shared/multiplayer conversation list has loaded once.
  teamSharedLoaded?: boolean;
  // The archived conversation list has loaded once.
  archivedLoaded?: boolean;
  // Puts the open chat in dashboard view (true) or chat view (false): a
  // shortcut clicked while its chat is already open applies the view it
  // names, whichever view the chat is in.
  onSetDashboardView?: (dashboard: boolean) => void;
  // What this page knows of each chat's schedules, for the chats it has read
  // them for: "patrol" (an armed or paused recurring patrol), "timer" (only
  // one-shot timers), or "none". A chat missing from the map is not known
  // here; the rail then goes by the person's patrols list.
  scheduleKindByConv?: ReadonlyMap<string, "patrol" | "timer" | "none">;
  // Something the sidebar has to say, above the account menu (a change made
  // on another page that did not save).
  notice?: ReactNode;
}) {
  const [editingId, setEditingId] = useState<string | null>(null);
  // The person's sidebar layout (docs/SIDEBAR.md): the order, names and
  // visibility of the Pinned, Projects, Chats and Team chats sections, what
  // is kept at the top, and how much a row says.
  const { layout: railLayout } = useSidebar();
  const pinnedSec = sectionOf(railLayout, "pinned");
  const projectsSec = sectionOf(railLayout, "projects");
  const chatsSec = sectionOf(railLayout, "chats");
  const teamSec = sectionOf(railLayout, "team");
  // Each section starts open unless the layout folds it; folding one in the
  // sidebar lasts for the page.
  const [pinnedOpen, setPinnedOpen] = useState(!pinnedSec.collapsed);
  const [projectsSectionOpen, setProjectsSectionOpen] = useState(!projectsSec.collapsed);
  const [chatsSectionOpen, setChatsSectionOpen] = useState(!chatsSec.collapsed);
  const [teamChatsOpen, setTeamChatsOpen] = useState(!teamSec.collapsed);
  // When the layout's "starts folded" choices change (the read landed, or a
  // preset changed them), the sections follow. Compared during render,
  // React's "reset state when a prop changes".
  const foldKey = `${Boolean(pinnedSec.collapsed)}|${Boolean(projectsSec.collapsed)}|${Boolean(chatsSec.collapsed)}|${Boolean(teamSec.collapsed)}`;
  const [seenFoldKey, setSeenFoldKey] = useState(foldKey);
  if (foldKey !== seenFoldKey) {
    setSeenFoldKey(foldKey);
    setPinnedOpen(!pinnedSec.collapsed);
    setProjectsSectionOpen(!projectsSec.collapsed);
    setChatsSectionOpen(!chatsSec.collapsed);
    setTeamChatsOpen(!teamSec.collapsed);
  }
  // Each project starts collapsed and remembers its expansion for the page.
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(new Set());
  // "All N projects": the section lists the most recently updated few until asked.
  const [showAllProjects, setShowAllProjects] = useState(false);
  // Whose chats the Chats list shows (the chips over it).
  const [scope, setScope] = useState<ChatScope>("all");
  // The project being renamed inline (menu → Rename), mirroring the chat
  // rows' editingId.
  const [renamingProjectId, setRenamingProjectId] = useState<string | null>(null);
  // Same story as the chat rows' rename field: it opens from the project menu
  // and must take focus. Only one renames at a time, so one ref is enough.
  const projectRenameInputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (renamingProjectId !== null) projectRenameInputRef.current?.focus();
  }, [renamingProjectId]);
  // Unified search (#308 merged into the rail): alongside the client-side
  // title filter, the same query hits GET /api/search (Postgres full-text
  // over message content, 300ms debounce). null = no query / search
  // disabled server-side / fetch failed — the "In messages" group simply
  // doesn't render; the title filter still works.
  // contentFor is the query the current answer belongs to: until it matches
  // what is typed, the message search is still on its way (the empty state
  // says "searching" rather than "nothing matches").
  const [contentResults, setContentResultsRaw] = useState<SearchResult[] | null>(null);
  const [contentFor, setContentFor] = useState("");
  useEffect(() => {
    const q = sidebarQuery.trim();
    let cancelled = false;
    const setContentResults = (results: SearchResult[] | null) => {
      setContentResultsRaw(results);
      setContentFor(q);
    };
    // Every setState happens inside the timer callback, never synchronously
    // in the effect body (the React compiler lint's cascading-render rule);
    // a too-short query clears immediately via a 0ms timer for the same
    // reason.
    const handle = setTimeout(
      () => {
        if (q.length < 2) {
          if (!cancelled) setContentResults(null);
          return;
        }
        void (async () => {
          try {
            const res = await fetch(`/api/search?q=${encodeURIComponent(q)}`, {
              cache: "no-store",
            });
            if (!res.ok) {
              if (!cancelled) setContentResults(null);
              return;
            }
            const data = (await res.json()) as {
              results: SearchResult[] | null;
            };
            if (!cancelled) setContentResults(data.results ?? []);
          } catch {
            if (!cancelled) setContentResults(null);
          }
        })();
      },
      q.length < 2 ? 0 : 300,
    );
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [sidebarQuery]);
  // The project row a conversation drag is currently hovering — drives the
  // drop-target highlight.
  const [dragOverProject, setDragOverProject] = useState<string | null>(null);

  // The parent's `r` shortcut asks for an inline rename by bumping renameSignal.
  // We open the inline editor for the requested id when the nonce changes,
  // comparing *during render* (React's "reset state when a prop changes"
  // pattern) so it fires once per request without a setState-in-effect cascade.
  const [seenRenameNonce, setSeenRenameNonce] = useState<number | null>(renameSignal?.nonce ?? null);
  if (renameSignal && renameSignal.nonce !== seenRenameNonce) {
    setSeenRenameNonce(renameSignal.nonce);
    if (!readOnly) setEditingId(renameSignal.id);
  }

  const onChat = activeView === "chat";
  const openConversationId = onChat ? activeConversationId : null;
  const openProjectId = onChat ? activeProjectId : null;
  const labelSummaries: LabelSummary[] = deriveLabels(conversations);
  const allLabelNames = labelSummaries.map((l) => l.name);
  // A chat whose project the viewer cannot see is treated as unfiled, so it
  // still appears somewhere. Only meaningful once the projects list has
  // loaded: an empty list on first paint would move every project chat into
  // Chats for a frame, so the set is withheld until there is one.
  const knownProjectIds = projects.length > 0 ? new Set(projects.map((p) => p.id)) : undefined;
  // What the person keeps at the top of the sidebar is listed there and
  // nowhere else: a thing has one place.
  const kept = topItems(railLayout);
  const topChatIds = new Set(kept.filter((i) => i.kind === "chat").map((i) => i.ref ?? ""));
  const topProjectIds = new Set(kept.filter((i) => i.kind === "project").map((i) => i.ref ?? ""));
  // One list for all chats, the default (docs/SIDEBAR.md): teammates' chats
  // are listed in Chats with the person's own. "Own list" gives them a Team
  // chats section instead; "Hidden" lists none of them (search finds them
  // all the same).
  const unified = !railLayout.separate_lists;
  const listedTeamChats = teamSec.hidden ? [] : teamSharedConversations.filter((c) => !topChatIds.has(c.id));
  const teamRowIds = new Set(listedTeamChats.map((c) => c.id));
  // The Pinned section: the pinned projects, the person's own pinned chats
  // and the teammates' chats they pinned for themselves (their own pin;
  // nothing changes for the owner), in one hand-set order. A chat is listed
  // once: a teammate's row wins over a copy in the own list.
  //
  // With Pinned hidden, a pinned project and a teammate's chat the person
  // pinned stay where they are listed otherwise (Projects; Chats or Team
  // chats): hiding the list must not take them out of the sidebar. A layout
  // saved before Pinned existed starts with it hidden when Chats was, and
  // its pinned projects were in Projects then.
  const pinnedTeam = pinnedSec.hidden ? [] : listedTeamChats.filter((c) => c.pinned);
  const pinnedTeamIds = new Set(pinnedTeam.map((c) => c.id));
  const pinnedChats = pinnedGroup(
    pinnedUnfiled(conversations, knownProjectIds).filter((c) => !pinnedTeamIds.has(c.id) && !topChatIds.has(c.id)),
    pinnedTeam,
  );
  const pinnedProjects = pinnedSec.hidden ? [] : projects.filter((p) => p.pinned && !topProjectIds.has(p.id));
  const pinned = pinnedEntries(pinnedChats, pinnedProjects, chatOrders?.[PINNED_ORDER_SCOPE]);
  // Reorder mode: the scope being ordered ("pinned" or "project:<id>"), whose
  // rows become a drag-and-arrow list until Done (or Escape). One at a time.
  const [reorderScope, setReorderScope] = useState<string | null>(null);
  const reorderable = (count: number) => Boolean(onReorderChats) && count > 1 && count <= MAX_CHAT_ORDER;
  const canReorderPinned = reorderable(pinned.length);
  // Leaving the mode hands the focus back to what opened it: the Pinned
  // section's Reorder button, or the project's options button (both are
  // unmounted or closed while the mode is on).
  const reorderToggleRef = useRef<HTMLButtonElement | null>(null);
  const returnFocusOnExit = useRef<(() => void) | null>(null);
  useEffect(() => {
    if (reorderScope !== null || !returnFocusOnExit.current) return;
    const restore = returnFocusOnExit.current;
    returnFocusOnExit.current = null;
    restore();
  }, [reorderScope]);
  // If the scope being ordered stops being orderable while the mode is open
  // (a chat unpinned or moved in another tab leaves fewer than two, or the
  // project disappears), the mode ends: otherwise its rows would fall back
  // to ordinary ones with no Done to clear it, and it would reappear,
  // unasked, when the count recovered.
  const reorderScopeCount =
    reorderScope === null
      ? 0
      : reorderScope === PINNED_ORDER_SCOPE
        ? pinned.length
        : (projectGroups(conversations, projects).find((g) => projectOrderScope(g.project.id) === reorderScope)?.chats
            .length ?? 0);
  useEffect(() => {
    if (reorderScope !== null && !reorderable(reorderScopeCount)) setReorderScope(null);
    // reorderable reads only props; the count is the trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reorderScope, reorderScopeCount]);
  const endReorder = (restore: () => void) => {
    returnFocusOnExit.current = restore;
    setReorderScope(null);
  };

  // Which chats check in on a schedule, and when next. Patrols come from one
  // read of the person's patrols per page; the page's own knowledge of a
  // chat's schedules (read when the chat is opened, updated as a patrol is
  // made, paused or ended) wins over it, so a patrol made a minute ago is
  // marked without a reload.
  const patrolInfo = patrolsByConversation(usePatrols(true));
  const scheduleKind = (id: string): "patrol" | "timer" | "none" =>
    scheduleKindByConv?.get(id) ?? (patrolInfo.has(id) ? "patrol" : "none");
  const isPatrol = (id: string) => scheduleKind(id) === "patrol";
  // Temporary is exactly what the server deletes — for inactivity, and by
  // "Delete temporary chats" (isTemporaryChat mirrors the server's one
  // definition). The server's listing says which chats a patrol or timer
  // keeps; the patrols list and what this page knows fill in what changed
  // since.
  const isTemporary = (c: ConversationSummary) =>
    isTemporaryChat({ ...c, has_schedule: c.has_schedule || patrolInfo.has(c.id) }, scheduleKindByConv);
  const retention = retentionPolicy(serverConfig);
  // A patrol's next check is quoted to the minute or the hour; days left and
  // the date groups ride the same clock.
  const nowSeconds = useNowSeconds(60_000);
  // Days left for a temporary chat inside the warning window (null before it).
  const expiresIn = (c: ConversationSummary) =>
    isTemporary(c) ? expiresInDays(c.updated_at, retention, nowSeconds) : null;
  // What a row says for itself, most pressing first (RowStatus).
  const showStatus = !railLayout.status_off;
  const rowStatus = (c: ConversationSummary, own: boolean): RowStatusInput => {
    const patrol = isPatrol(c.id) ? patrolInfo.get(c.id) : undefined;
    return {
      streaming: streamingConvs.has(c.id),
      expiresIn: own ? expiresIn(c) : null,
      patrol: isPatrol(c.id) ? { status: patrol?.status ?? "active", nextRunAt: patrol?.next_run_at } : null,
      live: Boolean(c.multiplayer),
      teamShared: own && Boolean(c.team_visible),
      linkShared: own && Boolean(c.share_token),
      sealed: Boolean(c.lockdown),
      owner: own ? undefined : c.user_email,
    };
  };

  // The Chats list: the person's unpinned, unfiled chats and (in the one
  // list) teammates' chats, under one sort. The chips narrow whose are
  // shown. A chat is listed once: a teammate's row wins over a copy in the
  // own list, and a teammate's chat the person pinned is in Pinned.
  const recentOwn = recentUnfiled(conversations, knownProjectIds).filter(
    (c) => !topChatIds.has(c.id) && !(unified && teamRowIds.has(c.id)),
  );
  const teamInChats = unified ? listedTeamChats.filter((c) => !pinnedTeamIds.has(c.id)) : [];
  const patrolsInChats = [...recentOwn, ...teamInChats].filter((c) => isPatrol(c.id));
  // A chip is offered only when it would change the list.
  const scopes = CHAT_SCOPES.filter((s) =>
    s.value === "patrols" ? patrolsInChats.length > 0 : s.value === "all" ? true : teamInChats.length > 0,
  );
  const activeScope: ChatScope = scopes.some((s) => s.value === scope) ? scope : "all";
  const scoped =
    activeScope === "mine"
      ? recentOwn
      : activeScope === "shared"
        ? teamInChats
        : activeScope === "patrols"
          ? patrolsInChats
          : [...recentOwn, ...teamInChats];
  const chatList = sortRecent(scoped, recentSort);
  const chatGroups = railLayout.dates_off
    ? null
    : groupByDate(chatList, recentSort, new Date(nowSeconds * 1000));
  // The person's chats that are about to be deleted, in the order the list
  // shows them, for the notice under the heading and its jump to the first.
  // A chat kept at the top is not in the list, but it is deleted like any
  // other temporary chat, so it counts here too (after the listed ones: the
  // jump goes to a row in the list when there is one).
  const keptAtTop = recentUnfiled(conversations, knownProjectIds).filter((c) => topChatIds.has(c.id));
  const expiring = [...sortRecent(recentOwn, recentSort), ...keptAtTop].filter((c) => expiresIn(c) !== null);
  const showFirstExpiring = () => {
    const first = expiring[0];
    if (!first) return;
    setScope("all");
    setChatsSectionOpen(true);
    requestAnimationFrame(() => {
      const row = document.querySelector<HTMLElement>(
        `[data-conversation-id="${CSS.escape(first.id)}"] [data-rail-row]`,
      );
      row?.scrollIntoView({ block: "center" });
      row?.focus();
    });
  };

  const filtering = computeIsFiltering({
    labels: filterLabels,
    query: sidebarQuery,
  });
  const searching = sidebarQuery.trim().length > 0;
  // Unified-search derivations: project names matching the query, and
  // content hits deduped against conversations the title filter already
  // shows (a chat matching both ways should appear once).
  const trimmedQuery = sidebarQuery.trim();
  const tokens = useMemo(() => searchTokens(trimmedQuery), [trimmedQuery]);
  const matchingProjects = useMemo(
    () => (searching ? projects.filter((p) => matchesSearch(tokens, p.name)) : []),
    [searching, projects, tokens],
  );
  // Search looks everywhere, whatever the sidebar shows: teammates' chats
  // (by title or by whose they are) and archived chats are matched here, the
  // person's own in filteredConversations. A hidden or folded section never
  // hides a result (docs/SIDEBAR-SEARCH.md).
  const teamMatches = useMemo(
    () => (searching ? teamSharedConversations.filter((c) => matchesSearch(tokens, c.title, c.user_email)) : []),
    [searching, teamSharedConversations, tokens],
  );
  const archivedMatches = useMemo(
    () => (searching ? filterConversations(archivedConversations, { labels: filterLabels, query: trimmedQuery }) : []),
    [searching, archivedConversations, filterLabels, trimmedQuery],
  );
  const canCustomize = useCanCustomize();
  const showSection = (id: SectionId) =>
    canCustomize ? () => updateSidebarLayout((l) => updateSection(l, id, { hidden: false })) : undefined;
  // The Projects section's tree: the projects that are not pinned or kept at
  // the top (those are listed there), most recently updated first. Built only
  // when its inputs change: the rail re-renders on every streamed event.
  const listedProjects = useMemo(
    () => projects.filter((p) => !(p.pinned && !pinnedSec.hidden) && !topProjectIds.has(p.id)),
    // topProjectIds is rebuilt each render from the layout.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [projects, railLayout],
  );
  const projectTree = useMemo(
    () => projectGroups(conversations, listedProjects, Number.POSITIVE_INFINITY, chatOrders),
    [conversations, listedProjects, chatOrders],
  );
  const pinnedProjectTree = useMemo(
    () => new Map(projectGroups(conversations, pinnedProjects, Number.POSITIVE_INFINITY, chatOrders).map((g) => [g.project.id, g])),
    // pinnedProjects is rebuilt each render from projects and the layout.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [conversations, projects, railLayout, chatOrders],
  );
  const matchingProjectTree = useMemo(
    () => (searching ? projectGroups(conversations, matchingProjects, Math.max(matchingProjects.length, 1), chatOrders) : []),
    [searching, conversations, matchingProjects, chatOrders],
  );
  // The open project (or the open chat's project) is never behind "All N
  // projects": the list opens up to show where the person is.
  const openChatProject = conversations.find((c) => c.id === openConversationId)?.project_id;
  const mustShowProject = projectTree.findIndex(
    (g) => g.project.id === openProjectId || g.project.id === openChatProject,
  );
  const allProjectsShown = showAllProjects || mustShowProject >= PROJECTS_SHOWN;
  const shownProjectTree = allProjectsShown ? projectTree : projectTree.slice(0, PROJECTS_SHOWN);

  const shownIds = new Set([
    ...filteredConversations.map((c) => c.id),
    ...archivedMatches.map((c) => c.id),
    ...teamMatches.map((c) => c.id),
  ]);
  // Only the answer to what is typed now: while a new query's answer is on
  // its way, the previous query's hits are not shown, counted or opened.
  const messageHits =
    searching && contentFor === sidebarQuery.trim()
      ? (contentResults ?? []).filter((r) => !shownIds.has(r.conversation_id))
      : [];

  const selecting = selectMode;
  const railCollapsed = collapse.collapsed;

  const toggleLabelFilter = (name: string) =>
    setFilterLabels((ls) => (ls.includes(name) ? ls.filter((l) => l !== name) : [...ls, name]));
  const clearFilters = () => {
    setFilterLabels([]);
    setSidebarQuery("");
  };

  const commitRename = (id: string, title: string) => {
    const trimmed = title.trim();
    if (trimmed) void renameConversation(id, trimmed);
    setEditingId(null);
  };

  const rowKebab = (conversation: ConversationSummary): ReactNode => (
    <ConversationKebab
      conversation={conversation}
      allLabelNames={allLabelNames}
      projects={projects}
      hasPatrol={isPatrol(conversation.id)}
      readOnly={readOnly}
      onPin={() => void togglePin(conversation)}
      onRename={() => setEditingId(conversation.id)}
      onDownload={() => void downloadConversation(conversation)}
      onPromote={() => void promoteConversation(conversation)}
      onSavePrompt={() => savePromptFromConversation(conversation)}
      onPatrol={
        onOpenPatrols
          ? () => {
              setSidebarOpen(false);
              onOpenPatrols(conversation);
            }
          : undefined
      }
      onSetLabels={(labels) => setConversationLabels(conversation.id, labels)}
      onSetProject={(projectID) => onMoveToProject(conversation.id, projectID ?? "")}
      isShared={Boolean(conversation.share_token)}
      onShare={() => openShareDialog(conversation)}
      onTransfer={() => openTransferDialog(conversation)}
      onArchive={() => void toggleArchive(conversation, true)}
      onDelete={() =>
        setPendingDeleteConversation({
          id: conversation.id,
          title: conversation.title,
        })
      }
    />
  );

  // One of the person's own chats.
  const renderOwnRow = (
    conversation: ConversationSummary,
    opts: { inResults?: boolean; icon?: string } = {},
  ) => (
    <ChatRow
      key={conversation.id}
      own
      conversation={conversation}
      icon={opts.icon}
      status={rowStatus(conversation, true)}
      showStatus={showStatus}
      now={nowSeconds}
      titleNode={opts.inResults ? markMatches(conversation.title, tokens) : undefined}
      context={
        opts.inResults && conversation.project_id
          ? projects.find((p) => p.id === conversation.project_id)?.name
          : undefined
      }
      active={openConversationId === conversation.id}
      focused={focusedConversationId === conversation.id}
      editing={editingId === conversation.id}
      selecting={selecting}
      checked={selectedIds.has(conversation.id)}
      onOpen={() => {
        setSidebarOpen(false);
        void loadConversation(conversation.id);
      }}
      onToggleSelect={() => onToggleSelection(conversation.id)}
      onCommitRename={(title) => commitRename(conversation.id, title)}
      onCancelRename={() => setEditingId(null)}
      kebab={rowKebab(conversation)}
      onDragStart={
        selecting || readOnly
          ? undefined
          : (e) => {
              e.dataTransfer.setData(CONV_DRAG_MIME, conversation.id);
              e.dataTransfer.effectAllowed = "move";
            }
      }
    />
  );
  const renderRow = (conversation: ConversationSummary) => renderOwnRow(conversation);
  const renderResultRow = (conversation: ConversationSummary) => renderOwnRow(conversation, { inResults: true });

  // A teammate's chat: the same row, with its owner's initial and a menu of
  // what a viewer can do with someone else's chat. Not selectable: it is not
  // the person's to delete, pin in bulk or label.
  const renderTeamRow = (c: ConversationSummary, opts: { inResults?: boolean; icon?: string } = {}) => (
    <ChatRow
      key={c.id}
      own={false}
      conversation={c}
      icon={opts.icon}
      testId="team-shared-conv-row"
      status={rowStatus(c, false)}
      showStatus={showStatus}
      now={nowSeconds}
      titleNode={opts.inResults ? markMatches(c.title, tokens) : undefined}
      active={openConversationId === c.id}
      onOpen={() => {
        setSidebarOpen(false);
        if (onSelectTeamSharedConversation) onSelectTeamSharedConversation(c);
        else void loadConversation(c.id);
      }}
      kebab={
        <TeamChatKebab
          conversation={c}
          onPin={canPinTeamChats ? () => void togglePin(c) : undefined}
          onDownload={c.multiplayer ? () => void downloadConversation(c) : undefined}
          onTransfer={canTransferTeamChats ? () => openTransferDialog(c) : undefined}
        />
      }
    />
  );
  const renderChat = (c: ConversationSummary, icon?: string) =>
    teamRowIds.has(c.id) && c.user_email && c.user_email.trim().toLowerCase() !== userEmail.trim().toLowerCase()
      ? renderTeamRow(c, { icon })
      : renderOwnRow(c, { icon });

  // ── A project's row, and its chats under it ───────────────────────────────
  // Three targets, each doing one thing and saying so: the NAME opens the
  // project's page (and leaves the fold as it is); the chevron at its left
  // folds and unfolds the project's chats here in the sidebar; the dots open
  // its menu. The chevron takes the icon's place on hover and keyboard focus,
  // and is always shown on a touch screen, where there is no hover to reveal
  // it. The whole group (row + expanded chats) is the drop zone for a dragged
  // chat, not just the row: dropping onto an expanded project's list is the
  // natural gesture.
  const expandProject = (id: string) => setExpandedProjects((s) => (s.has(id) ? s : new Set(s).add(id)));
  const renderProject = ({ project, chats }: { project: Project; chats: ConversationSummary[] }) => {
    const expanded = expandedProjects.has(project.id);
    const dropReady = dragOverProject === project.id;
    const active = openProjectId === project.id;
    // Team sharing, on the row itself, always labeled with its audience,
    // never a bare "shared" (docs/TEAM-SHARING.md vocabulary).
    const teamShared = Boolean(project.team_id);
    const teamSharedLabel = tr("Shared with {0}", [project.team_id]);
    // undefined = the counts haven't loaded (or the read failed); a loaded
    // map omits projects with none, so a missing key is a real zero. The
    // difference decides whether the empty state may quote a number at all.
    const sharedChats = teamSharedChatCounts ? (teamSharedChatCounts[project.id] ?? 0) : undefined;
    const emptyState = railProjectEmptyState({ teamShared, teamSharedChatCount: sharedChats });
    const openHome = () => {
      setSidebarOpen(false);
      onOpenProjectHome(project.id);
    };
    const newChatHere = onNewChatInProject && !readOnly ? (
      <>
        {" · "}
        <button
          type="button"
          aria-label={tr("New chat in {0}", [project.name])}
          className="rounded text-[var(--color-link)] transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={() => {
            setSidebarOpen(false);
            onNewChatInProject(project.id);
          }}
        >
          {tr("New chat here")}
        </button>
      </>
    ) : null;
    return (
      <div
        key={project.id}
        data-project-id={project.id}
        onDragOver={(e) => {
          if (readOnly || !e.dataTransfer.types.includes(CONV_DRAG_MIME)) return;
          e.preventDefault();
          e.dataTransfer.dropEffect = "move";
          setDragOverProject(project.id);
        }}
        onDragLeave={() => setDragOverProject((cur) => (cur === project.id ? null : cur))}
        onDrop={(e) => {
          setDragOverProject(null);
          const convID = e.dataTransfer.getData(CONV_DRAG_MIME);
          if (!convID) return;
          e.preventDefault();
          onMoveToProject(convID, project.id);
          // Reveal where the chat landed.
          expandProject(project.id);
        }}
      >
        {renamingProjectId === project.id ? (
          <input
            // Uncontrolled like the chat rows' rename input: mounts only
            // while renaming, so defaultValue tracks the live name.
            ref={projectRenameInputRef}
            aria-label={tr("Rename project {0}", [project.name])}
            className={RENAME_INPUT_CLASS}
            defaultValue={project.name}
            onFocus={(e) => e.currentTarget.select()}
            onBlur={(e) => {
              onRenameProject(project.id, e.currentTarget.value);
              setRenamingProjectId(null);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                onRenameProject(project.id, e.currentTarget.value);
                setRenamingProjectId(null);
              } else if (e.key === "Escape") {
                e.preventDefault();
                setRenamingProjectId(null);
              }
            }}
          />
        ) : (
          <div
            className={[
              "group relative flex min-h-[var(--rail-row-h)] items-center rounded-[var(--radius-md)] transition-colors any-pointer-coarse:min-h-11",
              dropReady
                ? "bg-[var(--rail-hover)] ring-1 ring-inset ring-[var(--color-accent)]"
                : active
                  ? "bg-[var(--rail-active)]"
                  : "hover:bg-[var(--rail-hover)]",
            ].join(" ")}
          >
            <button
              type="button"
              aria-expanded={expanded}
              aria-label={tr("Project {0} ({1} chats)", [project.name, chats.length])}
              title={expanded ? tr("Hide this project's chats") : tr("Show this project's chats")}
              data-testid="project-fold"
              className="hit-area-touch group/fold relative ml-1 inline-flex size-7 shrink-0 items-center justify-center rounded-md transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={() =>
                setExpandedProjects((s) => {
                  const next = new Set(s);
                  if (next.has(project.id)) next.delete(project.id);
                  else next.add(project.id);
                  return next;
                })
              }
            >
              <Icon
                name="briefcase"
                className={`${railIconClass(active)} transition group-hover:opacity-0 group-focus-visible/fold:opacity-0 any-pointer-coarse:hidden`}
              />
              <Icon
                name="chevron-right"
                className={[
                  "absolute size-3.5 text-[var(--color-text-secondary)] opacity-0 transition group-hover:opacity-100 group-focus-visible/fold:opacity-100 any-pointer-coarse:static any-pointer-coarse:size-4 any-pointer-coarse:opacity-100",
                  expanded ? "rotate-90" : "",
                ].join(" ")}
              />
            </button>
            <button
              type="button"
              aria-label={tr("Open project {0}", [project.name])}
              aria-current={active ? "page" : undefined}
              data-rail-row=""
              title={teamShared ? `${project.name} · ${teamSharedLabel}` : project.name}
              className={[
                "flex min-h-[var(--rail-row-h)] min-w-0 flex-1 items-center gap-2 rounded-[var(--radius-md)] pl-[0.3rem] pr-2.5 text-left text-[length:var(--rail-row-text)] leading-tight transition-colors focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11",
                ROW_CLEAR_FOR_ACTIONS,
                active || dropReady
                  ? "font-medium text-[var(--color-text-primary)]"
                  : "text-[var(--color-text-secondary)] group-hover:text-[var(--color-text-primary)]",
              ].join(" ")}
              onClick={openHome}
            >
              <span className="min-w-0 flex-1 truncate">{project.name}</span>
              {teamShared && showStatus ? (
                // Decorative HERE on purpose: this button carries its own
                // aria-label, which swallows any label nested inside it. The
                // audience text lives in the sr-only sibling just below,
                // where a screen reader actually reaches it.
                <span className={`ml-auto inline-flex shrink-0 ${STATUS_YIELDS_TO_ACTIONS}`}>
                  <TeamGlyph className="size-3.5 text-[var(--color-link)]" />
                </span>
              ) : null}
            </button>
            {teamShared ? <span className="sr-only">{teamSharedLabel}</span> : null}
            <div className="absolute inset-y-0 right-1 flex items-center">
              <ProjectKebab
                projectId={project.id}
                projectName={project.name}
                pinned={Boolean(project.pinned)}
                isOwner={project.owner_email === userEmail}
                readOnly={readOnly}
                onOpen={() => {
                  expandProject(project.id);
                  openHome();
                }}
                onNewChat={() => {
                  setSidebarOpen(false);
                  onNewChatInProject?.(project.id);
                }}
                onEdit={() => {
                  setSidebarOpen(false);
                  onOpenProjectHome(project.id, true);
                }}
                // Project sharing lives in settings ("Who can see it"), so
                // Share opens the same dialog.
                onShare={() => {
                  setSidebarOpen(false);
                  onOpenProjectHome(project.id, true);
                }}
                onPin={() => onPinProject(project.id, !project.pinned)}
                onRename={() => setRenamingProjectId(project.id)}
                onDelete={() => onDeleteProject(project.id)}
                onReorder={
                  reorderable(chats.length) && !selecting
                    ? () => {
                        expandProject(project.id);
                        setReorderScope(projectOrderScope(project.id));
                      }
                    : undefined
                }
              />
            </div>
          </div>
        )}
        {expanded ? (
          <div className="ml-[1.15rem] border-l border-[var(--color-border)] pl-1.5">
            {chats.length === 0 ? (
              <p className="m-0 flex min-h-[var(--rail-row-h)] flex-wrap items-center gap-x-1 px-2 py-1 text-[0.8rem] leading-[1.4] text-[var(--color-text-muted)]">
                {/* Three empty states, because "empty" means different things
                    to the owner and to a teammate. See railProjectEmptyState. */}
                {emptyState.kind === "team-has-chats" ? (
                  <>
                    {tr("No chats of yours yet ·")}{" "}
                    <button
                      type="button"
                      aria-label={tr(emptyState.count === 1 ? "Open {0} — {1} chat shared by your team" : "Open {0} — {1} chats shared by your team", [project.name, emptyState.count])}
                      className="rounded text-left text-[var(--color-link)] transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                      onClick={openHome}
                    >
                      {emptyState.count} {tr("shared by your team")} <span aria-hidden="true">&rarr;</span>
                    </button>
                  </>
                ) : emptyState.kind === "team-count-unknown" ? (
                  // Teammates may have shared chats here that the rail can't
                  // count yet, so this says nothing about theirs.
                  <>
                    {tr("No chats of yours yet")}
                    {newChatHere}
                  </>
                ) : (
                  <>
                    {tr("No chats yet")}
                    {newChatHere}
                  </>
                )}
              </p>
            ) : reorderScope === projectOrderScope(project.id) && onReorderChats && reorderable(chats.length) ? (
              <ChatReorderList
                chats={chats.map((c) => ({ id: c.id, title: c.title }))}
                label={tr("Chats in {0}, in order", [project.name])}
                hint={tr("Drag a row by its handle, or use the arrows.")}
                onReorder={(ids) => onReorderChats(projectOrderScope(project.id), ids)}
                onDone={() =>
                  // By project ID: two projects may share a name.
                  endReorder(() =>
                    Array.from(document.querySelectorAll<HTMLButtonElement>("button[data-project-options]"))
                      .find((b) => b.dataset.projectOptions === project.id)
                      ?.focus(),
                  )
                }
              />
            ) : (
              chats.map(renderRow)
            )}
          </div>
        ) : null}
      </div>
    );
  };

  // ── Pinned ────────────────────────────────────────────────────────────────
  const pinnedReordering = reorderScope === PINNED_ORDER_SCOPE && canReorderPinned && Boolean(onReorderChats);
  const pinnedSection =
    pinnedSec.hidden || pinned.length === 0 ? null : (
      <section key="pinned" aria-label={pinnedSec.label || tr("Pinned")} data-testid="pinned-section" className="mb-1">
        <SectionHeading
          label={pinnedSec.label || tr("Pinned")}
          open={pinnedOpen}
          onToggle={() => setPinnedOpen((o) => !o)}
        >
          {pinnedOpen && pinnedReordering ? (
            <ReorderDoneButton onDone={() => endReorder(() => reorderToggleRef.current?.focus())} />
          ) : pinnedOpen && canReorderPinned && !selecting ? (
            <button
              type="button"
              ref={reorderToggleRef}
              data-testid="pinned-reorder-toggle"
              aria-label={tr("Reorder pinned chats")}
              onClick={() => setReorderScope(PINNED_ORDER_SCOPE)}
              className={RAIL_HEADING_ACTION_CLASS}
            >
              {tr("Reorder")}
            </button>
          ) : null}
        </SectionHeading>
        {!pinnedOpen ? null : pinnedReordering && onReorderChats ? (
          <ChatReorderList
            chats={pinned.map((e) => ({ id: e.id, title: e.kind === "chat" ? e.chat.title : e.project.name }))}
            label={tr("Pinned chats, in order")}
            hint={tr("Drag a row by its handle, or use the arrows.")}
            onReorder={(ids) => onReorderChats(PINNED_ORDER_SCOPE, ids)}
            onDone={() => endReorder(() => reorderToggleRef.current?.focus())}
            doneInHeading
          />
        ) : (
          pinned.map((e) =>
            e.kind === "chat"
              ? renderChat(e.chat, "message")
              : renderProject(pinnedProjectTree.get(e.project.id) ?? { project: e.project, chats: [] }),
          )
        )}
      </section>
    );

  // ── Projects ──────────────────────────────────────────────────────────────
  const projectsSection = projectsSec.hidden ? null : (
    <section key="projects" aria-label={projectsSec.label || tr("Projects")} data-testid="projects-section" className="mb-1">
      <SectionHeading
        label={projectsSec.label || tr("Projects")}
        open={projectsSectionOpen}
        onToggle={() => setProjectsSectionOpen((o) => !o)}
        showChevron
      />
      {!projectsSectionOpen ? null : projectTree.length === 0 ? (
        readOnly ? (
          <p className="m-0 px-2.5 py-1 text-[length:var(--rail-row-text)] text-[var(--color-text-muted)]">{tr("Nothing here yet.")}</p>
        ) : (
        <button
          type="button"
          className="flex min-h-[var(--rail-row-h)] w-full items-center gap-2.5 rounded-[var(--radius-md)] px-2.5 text-left text-[length:var(--rail-row-text)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
          onClick={onCreateProject}
        >
          <Icon name="plus" className="size-[1.05rem] shrink-0" />
          {pinnedProjects.length + topProjectIds.size > 0 ? tr("New project") : tr("New project…")}
        </button>
        )
      ) : (
        <>
          {shownProjectTree.map(renderProject)}
          {projectTree.length > PROJECTS_SHOWN ? (
            <button
              type="button"
              data-testid="projects-show-all"
              aria-expanded={allProjectsShown}
              className="mt-0.5 inline-flex min-h-8 items-center rounded-md px-2.5 text-[0.8rem] text-[var(--color-link)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
              onClick={() => setShowAllProjects(!allProjectsShown)}
            >
              {allProjectsShown ? tr("Show fewer") : tr("All {0} projects", [projectTree.length])}
            </button>
          ) : null}
        </>
      )}
    </section>
  );

  // ── Archived: a quiet fold at the end of Chats ────────────────────────────
  const renderArchivedRow = (conversation: ConversationSummary, inResults: boolean) => (
    <div
      key={conversation.id}
      className={[
        "group relative rounded-[var(--radius-md)] transition-colors",
        openConversationId === conversation.id ? "bg-[var(--rail-active)]" : "hover:bg-[var(--rail-hover)]",
      ].join(" ")}
    >
      <button
        type="button"
        data-rail-row=""
        className="flex min-h-[var(--rail-row-h)] w-full min-w-0 items-center rounded-[var(--radius-md)] pl-2.5 pr-20 text-left text-[length:var(--rail-row-text)] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11 any-pointer-coarse:pr-28"
        onClick={() => {
          setSidebarOpen(false);
          void loadConversation(conversation.id);
        }}
      >
        <span className="block truncate italic">
          {inResults ? markMatches(conversation.title, tokens) : conversation.title}
        </span>
      </button>
      {readOnly ? null : (
        <div className={`absolute inset-y-0 right-1 flex items-center gap-1 transition any-pointer-coarse:gap-4 ${ROW_ACTIONS_REVEAL}`}>
          <button
            type="button"
            aria-label={tr("Unarchive {0}", [conversation.title])}
            data-tip-top="Unarchive"
            className="hit-area-touch inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
            onClick={() => void toggleArchive(conversation, false)}
          >
            <Icon name="unarchive" className="size-3.5" />
          </button>
          <button
            type="button"
            aria-label={tr("Delete {0}", [conversation.title])}
            data-tip-top="Delete"
            className="hit-area-touch inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)] sm:size-7"
            onClick={() =>
              setPendingDeleteConversation({
                id: conversation.id,
                title: conversation.title,
              })
            }
          >
            <Icon name="trash" className="size-3.5" />
          </button>
        </div>
      )}
    </div>
  );
  const archivedFold =
    archivedConversations.length > 0 ? (
      <div className="mt-2">
        <button
          type="button"
          aria-expanded={showArchived}
          aria-label={tr("Archived conversations ({0})", [archivedConversations.length])}
          className="flex min-h-8 w-full items-center gap-1 rounded-md px-2.5 text-[0.75rem] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
          onClick={() => setShowArchived((v) => !v)}
        >
          {tr("Archived ({0})", [archivedConversations.length])}
          <Icon name="chevron-down" className={["size-3 shrink-0 transition", showArchived ? "" : "-rotate-90"].join(" ")} />
        </button>
        {showArchived ? archivedConversations.map((conversation) => renderArchivedRow(conversation, false)) : null}
      </div>
    ) : null;

  // ── Chats ─────────────────────────────────────────────────────────────────
  const hasTemporary = conversations.some(isTemporary);
  const labelFilterChips =
    filterLabels.length > 0 ? (
      <div className="mb-1 mt-0.5 flex items-center gap-1.5 px-1.5 motion-safe:animate-[filter-in_var(--motion-fast)_ease_both]">
        <div className="flex min-w-0 flex-1 flex-wrap gap-1.5">
          {filterLabels.map((l) => (
            <span
              key={l}
              className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.78rem] text-[var(--color-text-primary)]"
            >
              <span className="text-[var(--color-text-muted)]">{tr("Label:")}</span> {l}
              <button
                type="button"
                aria-label={tr("Remove label filter {0}", [l])}
                className="hit-area inline-flex size-4 items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                onClick={() => toggleLabelFilter(l)}
              >
                <Icon name="close" className="size-2.5" />
              </button>
            </span>
          ))}
        </div>
        <button
          type="button"
          className="shrink-0 rounded px-1.5 py-0.5 text-[0.78rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
          onClick={clearFilters}
        >
          {tr("Clear")}
        </button>
      </div>
    ) : null;
  const chatsSection = chatsSec.hidden ? null : (
    <section key="chats" aria-label={chatsSec.label || tr("Chats")} data-testid="chats-section" className="mb-1">
      <SectionHeading
        label={chatsSec.label || tr("Chats")}
        open={chatsSectionOpen}
        onToggle={() => setChatsSectionOpen((o) => !o)}
      >
        {chatsSectionOpen && !selecting ? (
          <ChatsMenu
            sort={recentSort}
            onSort={onRecentSortChange}
            labels={labelSummaries}
            filterLabels={filterLabels}
            onToggleLabel={toggleLabelFilter}
            onSelect={!readOnly && recentOwn.length + pinnedChats.length > 0 ? () => onEnterSelectMode() : undefined}
            onDeleteTemporary={!readOnly && hasTemporary ? () => setConfirmBulkDelete(true) : undefined}
            ttlDays={retention.ttlDays}
          />
        ) : null}
      </SectionHeading>
      {!chatsSectionOpen ? null : (
        <>
          {labelFilterChips}
          {filtering ? (
            // A label filter alone: the person's own chats carrying it,
            // wherever they live.
            filteredConversations.length === 0 ? (
              <p className="m-0 px-2.5 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
                {tr("Nothing matches this filter.")}
              </p>
            ) : (
              filteredConversations.map(renderRow)
            )
          ) : (
            <>
              {scopes.length > 1 ? (
                <div role="group" aria-label={tr("Show")} data-testid="chat-scope" className="mb-1 flex flex-wrap gap-1 px-1.5 pt-0.5">
                  {scopes.map((s) => (
                    <button
                      key={s.value}
                      type="button"
                      aria-pressed={activeScope === s.value}
                      data-testid={`chat-scope-${s.value}`}
                      onClick={() => setScope(s.value)}
                      className={[
                        "inline-flex h-7 items-center rounded-full border px-2.5 text-[0.78rem] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-9 any-pointer-coarse:px-3.5",
                        activeScope === s.value
                          ? "border-[var(--color-selected-border)] bg-[var(--color-selected-bg)] text-[var(--color-text-primary)]"
                          : "border-[var(--color-pill-border)] text-[var(--color-text-secondary)] hover:bg-[var(--color-pill-hover-bg)] hover:text-[var(--color-text-primary)]",
                      ].join(" ")}
                    >
                      {s.label}
                    </button>
                  ))}
                </div>
              ) : null}
              <RetentionNotices
                expiringCount={expiring.length}
                temporaryCount={conversations.filter(isTemporary).length}
                cap={retention.cap}
                onShowFirst={showFirstExpiring}
              />
              {chatList.length === 0 ? (
                <p className="m-0 px-2.5 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
                  {activeScope === "all" ? tr("No saved chats yet.") : tr("Nothing here yet.")}
                </p>
              ) : (
                <div data-testid="unified-chat-list">
                  {chatGroups
                    ? chatGroups.map((g) => (
                        <div key={g.id} role="group" aria-label={dateGroupLabel(g.id)} data-date-group={g.id}>
                          <p aria-hidden="true" className="m-0 px-2.5 pb-0.5 pt-2 text-[0.72rem] text-[var(--color-text-muted)]">
                            {dateGroupLabel(g.id)}
                          </p>
                          {g.chats.map((c) => renderChat(c))}
                        </div>
                      ))
                    : chatList.map((c) => renderChat(c))}
                </div>
              )}
            </>
          )}
          {archivedFold}
        </>
      )}
    </section>
  );

  // ── Team chats, as a list of their own ("Own list") ───────────────────────
  const separateTeam = listedTeamChats.filter((c) => !pinnedTeamIds.has(c.id));
  const teamSection =
    !unified && !teamSec.hidden && separateTeam.length > 0 ? (
      <section key="team" aria-label={teamSec.label || tr("Team chats")} data-testid="team-chats-section" className="mb-1">
        <SectionHeading
          label={teamSec.label ? `${teamSec.label} (${separateTeam.length})` : tr("Team chats ({0})", [separateTeam.length])}
          open={teamChatsOpen}
          onToggle={() => setTeamChatsOpen((o) => !o)}
        />
        {teamChatsOpen ? separateTeam.map((c) => renderTeamRow(c)) : null}
      </section>
    ) : null;

  const sectionNodes: Record<SectionId, ReactNode> = {
    pinned: pinnedSection,
    projects: projectsSection,
    chats: chatsSection,
    team: teamSection,
  };

  // What the sidebar's shortcuts need from this page (sidebar/targets.tsx):
  // live names, the open chat or project, whether a target can still be
  // opened by this account, and opening it in place.
  // "Not available" needs every list a chat could be in to have answered:
  // own (the history load), archived and shared — each read on its own.
  const listsKnown = !isLoadingHistory && teamSharedLoaded && archivedLoaded;
  // The Pinned section, for Customize: what is in it, a new order for it,
  // taking one out, and putting a chat or a project in.
  const mineEmail = userEmail.trim().toLowerCase();
  // What Customize's Pin may pin, and what it then pins: a project the person
  // owns, or a chat of theirs (or, where they may, a teammate's) that is not
  // filed in a project (a project's chat lives under its project) and is not
  // pinned already. One rule for the picker's list and for the pin itself,
  // so the picker never offers a row the pin would refuse.
  const pinTarget = (
    kind: "chat" | "project",
    ref: string,
  ): { kind: "project" } | { kind: "chat"; chat: ConversationSummary } | null => {
    if (readOnly) return null;
    if (kind === "project") {
      const project = projects.find((p) => p.id === ref);
      if (!project || project.pinned || (project.owner_email ?? "").trim().toLowerCase() !== mineEmail) return null;
      return { kind: "project" };
    }
    const own = conversations.find((c) => c.id === ref);
    const chat = own ?? (canPinTeamChats ? teamSharedConversations.find((c) => c.id === ref) : undefined);
    if (!chat || chat.pinned || (own && own.project_id && knownProjectIds?.has(own.project_id))) return null;
    return { kind: "chat", chat };
  };
  const sidebarPins: SidebarPins = {
    entries: pinned.map((e) =>
      e.kind === "chat"
        ? { id: e.id, kind: "chat", ref: e.chat.id, title: e.chat.title, canUnpin: !readOnly && (!teamRowIds.has(e.chat.id) || canPinTeamChats) }
        : {
            id: e.id,
            kind: "project",
            ref: e.project.id,
            title: e.project.name,
            canUnpin: !readOnly && (e.project.owner_email ?? "").trim().toLowerCase() === mineEmail,
          },
    ),
    reorder: canReorderPinned && onReorderChats ? (ids) => onReorderChats(PINNED_ORDER_SCOPE, ids) : undefined,
    unpin: (entry) => {
      if (readOnly) return;
      if (entry.kind === "project") {
        onPinProject(entry.ref, false);
        return;
      }
      const chat = pinnedChats.find((c) => c.id === entry.ref);
      if (chat) void togglePin(chat);
    },
    canPin: (kind, ref) => pinTarget(kind, ref) !== null,
    pin: (kind, ref) => {
      const target = pinTarget(kind, ref);
      if (!target) return false;
      if (target.kind === "project") onPinProject(ref, true);
      else void togglePin(target.chat);
      return true;
    },
  };
  const sidebarTargets: SidebarTargets = {
    activeConversationId: openConversationId,
    activeProjectId: openProjectId,
    pins: sidebarPins,
    titleFor: (item) => {
      if (item.kind === "chat") {
        return (
          conversations.find((c) => c.id === item.ref) ??
          teamSharedConversations.find((c) => c.id === item.ref) ??
          archivedConversations.find((c) => c.id === item.ref)
        )?.title;
      }
      if (item.kind === "project") return projects.find((p) => p.id === item.ref)?.name;
      return undefined;
    },
    availability: (item) => {
      if (item.kind === "chat") {
        // Found in any list already loaded: openable, whatever the others
        // are still doing (a failed team read must not hide a chat that is
        // plainly here). "No" only once every list has answered.
        const found =
          conversations.some((c) => c.id === item.ref) ||
          archivedConversations.some((c) => c.id === item.ref) ||
          teamSharedConversations.some((c) => c.id === item.ref);
        if (found) return "yes";
        return listsKnown ? "no" : "unknown";
      }
      if (item.kind === "project") {
        if (projects.some((p) => p.id === item.ref)) return "yes";
        return projectsLoaded ? "no" : "unknown";
      }
      return "unknown";
    },
    open: (item) => {
      const ref = item.ref ?? "";
      if (item.kind === "chat") {
        if (openConversationId === ref && onSetDashboardView) {
          onSetDashboardView(item.view === "dashboard");
          return true;
        }
        const team = teamSharedConversations.find((c) => c.id === ref);
        if (team && onSelectTeamSharedConversation) onSelectTeamSharedConversation(team);
        else void loadConversation(ref);
        return true;
      }
      if (item.kind === "project") {
        onOpenProjectHome(ref);
        return true;
      }
      return false;
    },
  };

  // ── Search results ─────────────────────────────────────────────────────────
  // One list for the whole query, in place of the sections: the person's own
  // chats, teammates' chats, projects, archived chats and message hits, each
  // group under the name of the section it comes from. It ignores what the
  // sidebar is set to show: a hidden or folded section is exactly where a
  // chat "goes missing", and search is how people look for it.
  const sectionLabel = (id: SectionId, fallback: string) => sectionOf(railLayout, id).label || fallback;
  const messageSearchPending = trimmedQuery.length >= 2 && contentFor !== trimmedQuery;
  const resultCount =
    filteredConversations.length +
    teamMatches.length +
    matchingProjects.length +
    archivedMatches.length +
    messageHits.length;
  // The lists search could not look in: each is read on its own after the
  // boot, and a read still on its way (or one that failed) leaves its flag
  // down. Search then says which lists it did not see, instead of claiming
  // it looked everywhere.
  const unreadLists = [
    teamSharedLoaded ? null : sectionLabel("team", tr("Team chats")),
    projectsLoaded ? null : sectionLabel("projects", tr("Projects")),
    archivedLoaded ? null : tr("Archived"),
  ].filter((x): x is string => x !== null);
  // A hit in a chat someone shared with the person (it carries the owner's
  // email) opens through the shared door, as its row does. If the shared
  // list has not loaded, the hit itself stands in for the row: the page
  // tries it as a chat the person takes part in, then as a read-only team
  // chat.
  const openMessageHit = (r: SearchResult) => {
    const team = teamSharedConversations.find((c) => c.id === r.conversation_id);
    if (team && onSelectTeamSharedConversation) onSelectTeamSharedConversation(team);
    else if (r.user_email && onSelectTeamSharedConversation) {
      onSelectTeamSharedConversation({
        id: r.conversation_id,
        title: r.title,
        persona: "",
        model: "",
        pinned: false,
        updated_at: r.matched_at,
        user_email: r.user_email,
        multiplayer: true,
      });
    } else void loadConversation(r.conversation_id);
  };
  const searchResults = (
    <div data-testid="search-results" role="region" aria-label={tr("Search results")}>
      <p role="status" className="sr-only">
        {resultCount === 0
          ? messageSearchPending
            ? tr("Searching…")
            : tr("Nothing matches “{0}”.", [trimmedQuery])
          : resultCount === 1
            ? tr("1 result for “{0}”", [trimmedQuery])
            : tr("{0} results for “{1}”", [resultCount, trimmedQuery])}
      </p>
      <p aria-hidden="true" className={`m-0 px-2.5 pb-1 pt-1.5 ${RAIL_HEADING_CLASS}`}>
        {resultCount === 1 ? tr("1 result") : tr("{0} results", [resultCount])}
      </p>
      {resultCount === 0 ? (
        messageSearchPending ? (
          <p className="m-0 px-2.5 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">{tr("Searching…")}</p>
        ) : (
          <div data-testid="search-empty" className="px-2.5 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
            <p className="m-0 font-medium text-[var(--color-text-secondary)]">
              {tr("Nothing matches “{0}”.", [trimmedQuery])}
            </p>
            <p className="m-0 mt-1 text-[0.78rem] leading-[1.5]">
              {unreadLists.length > 0
                ? tr("Try fewer or different words.")
                : filterLabels.length > 0
                ? tr("Search looked through all your chats with the chosen labels, plus team chats and projects, including hidden and folded sections. Try fewer words, or clear the label filter.")
                : tr("Search looked through all your chats, team chats, projects and archived chats, including hidden and folded sections. Try fewer or different words.")}
            </p>
            <button
              type="button"
              className="mt-2 rounded-md border border-[var(--color-border)] px-2 py-1 text-[0.78rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
              onClick={() => {
                setSidebarQuery("");
                searchRef.current?.focus();
              }}
            >
              {tr("Clear search")}
            </button>
          </div>
        )
      ) : null}
      {filteredConversations.length > 0 ? (
        <SearchGroup
          testId="search-group-chats"
          title={sectionLabel("chats", tr("Chats"))}
          count={filteredConversations.length}
          hidden={chatsSec.hidden}
          onShow={showSection("chats")}
        >
          {filteredConversations.map(renderResultRow)}
        </SearchGroup>
      ) : null}
      {teamMatches.length > 0 ? (
        <SearchGroup
          testId="search-group-team"
          title={sectionLabel("team", tr("Team chats"))}
          count={teamMatches.length}
          // In the one list teammates' chats live in the Chats section, so
          // hiding either one hides them.
          hidden={teamSec.hidden || (unified && chatsSec.hidden)}
          onShow={showSection(teamSec.hidden ? "team" : "chats")}
        >
          {teamMatches.map((c) => renderTeamRow(c, { inResults: true }))}
        </SearchGroup>
      ) : null}
      {matchingProjects.length > 0 ? (
        <SearchGroup
          testId="search-group-projects"
          title={sectionLabel("projects", tr("Projects"))}
          count={matchingProjects.length}
          hidden={projectsSec.hidden}
          onShow={showSection("projects")}
        >
          {matchingProjectTree.map(renderProject)}
        </SearchGroup>
      ) : null}
      {archivedMatches.length > 0 ? (
        <SearchGroup testId="search-group-archived" title={tr("Archived")} count={archivedMatches.length}>
          {archivedMatches.map((c) => renderArchivedRow(c, true))}
        </SearchGroup>
      ) : null}
      {/* Full-text hits inside message content, deduped against the chats
          already listed above. Each hit carries its path (Project › chat
          title when the chat is filed) with the matched snippet beneath. */}
      {messageHits.length > 0 ? (
        <SearchGroup testId="search-group-messages" title={tr("In messages")} count={messageHits.length}>
          {messageHits.map((r) => {
            const conv = conversations.find((c) => c.id === r.conversation_id);
            const archived = !conv && archivedConversations.some((c) => c.id === r.conversation_id);
            const proj = conv?.project_id ? projects.find((p) => p.id === conv.project_id) : undefined;
            return (
              <button
                key={r.conversation_id}
                type="button"
                data-testid="search-result"
                data-rail-row=""
                className="block w-full rounded-[var(--radius-md)] px-2.5 py-1.5 text-left transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
                onClick={() => {
                  setSidebarOpen(false);
                  openMessageHit(r);
                }}
              >
                <span className="flex min-w-0 items-center gap-1.5 text-[0.85rem] text-[var(--color-text-secondary)]">
                  <span className="min-w-0 flex-1 truncate">
                    {proj ? <span className="text-[var(--color-text-muted)]">{proj.name} › </span> : null}
                    {r.title || tr("Untitled")}
                  </span>
                  {archived ? <ResultTag>{tr("Archived")}</ResultTag> : null}
                  <RowStatus
                    status={{
                      patrol: isPatrol(r.conversation_id)
                        ? { status: patrolInfo.get(r.conversation_id)?.status ?? "active", nextRunAt: patrolInfo.get(r.conversation_id)?.next_run_at }
                        : null,
                      // Whose chat it is, as on a teammate's row.
                      owner: r.user_email || undefined,
                    }}
                    now={nowSeconds}
                  />
                </span>
                <span className="block truncate text-[0.75rem] text-[var(--color-text-muted)] [&_mark]:rounded-[2px] [&_mark]:bg-[var(--color-accent)]/25 [&_mark]:text-[var(--color-text-primary)]">
                  {previewNodes(r.match_preview)}
                </span>
              </button>
            );
          })}
        </SearchGroup>
      ) : null}
      {unreadLists.length > 0 ? (
        <p
          data-testid="search-incomplete"
          className="mx-2 mt-1 rounded-md bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.72rem] leading-[1.5] text-[var(--color-text-secondary)]"
        >
          {tr("Not searched, because it has not loaded: {0}. If this stays, reload the page.", [unreadLists.join(", ")])}
        </p>
      ) : null}
    </div>
  );

  // Arrow keys walk the results from the search field and back (Enter in the
  // field opens the first one), so a search never needs the mouse. Rows are
  // the primary button of each result ([data-rail-row]); a row's ⋯ menu stays
  // reachable with Tab.
  const listRef = useRef<HTMLDivElement | null>(null);
  const resultRows = () =>
    Array.from(listRef.current?.querySelectorAll<HTMLElement>("[data-rail-row]") ?? []);
  const onSearchKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (!searching || e.nativeEvent.isComposing) return;
    if (e.key === "ArrowDown") {
      const first = resultRows()[0];
      if (!first) return;
      e.preventDefault();
      first.focus();
    } else if (e.key === "Enter") {
      const first = resultRows()[0];
      if (!first) return;
      e.preventDefault();
      first.click();
    }
  };
  const onResultsKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (!searching || (e.key !== "ArrowDown" && e.key !== "ArrowUp")) return;
    const target = e.target as HTMLElement;
    if (!target.matches("[data-rail-row]")) return;
    const rows = resultRows();
    const at = rows.indexOf(target);
    if (at === -1) return;
    e.preventDefault();
    if (e.key === "ArrowDown") rows[Math.min(rows.length - 1, at + 1)]?.focus();
    else if (at === 0) searchRef.current?.focus();
    else rows[at - 1]?.focus();
  };

  // ── Search, new chat and the + menu ───────────────────────────────────────
  const newChat = () => {
    setSidebarOpen(false);
    clearConversation(serverConfig.lockdownOnly ? { lockdown: true } : undefined);
  };
  const [searchFocused, setSearchFocused] = useState(false);
  const actions = (
    <div className={["flex items-center gap-1.5", railCollapsed ? "sm:flex-col sm:gap-1" : ""].join(" ")}>
      {/* One field for everything in the sidebar: the person's own chats,
          teammates' chats, projects and archived chats by name, and
          (debounced) full-text hits inside messages. While it holds text the
          sections step aside for one list of results, so a hidden or folded
          section never hides a match (docs/SIDEBAR-SEARCH.md). ⌘K focuses it. */}
      <div className={["relative flex min-w-0 flex-1 items-center", railCollapsed ? "sm:hidden" : ""].join(" ")}>
        <Icon
          name="search"
          className="pointer-events-none absolute left-[0.65rem] size-[0.95rem] text-[var(--color-text-muted)]"
        />
        <input
          ref={searchRef}
          type="search"
          value={sidebarQuery}
          onChange={(e) => setSidebarQuery(e.target.value)}
          onKeyDown={onSearchKeyDown}
          onFocus={() => setSearchFocused(true)}
          onBlur={() => setSearchFocused(false)}
          placeholder={tr("Search")}
          aria-label={tr("Search")}
          aria-keyshortcuts={searchShortcut.startsWith("⌘") ? "Meta+K" : "Control+K"}
          data-testid="search-input"
          className="search-input-no-native-clear h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] pl-[2.1rem] pr-8 text-[0.875rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-11"
        />
        {sidebarQuery ? (
          <button
            type="button"
            aria-label={tr("Clear search")}
            className="absolute right-[0.4rem] inline-flex size-6 items-center justify-center rounded-[var(--radius-pill)] text-[var(--color-text-muted)] transition before:absolute before:left-1/2 before:top-1/2 before:size-8 before:-translate-x-1/2 before:-translate-y-1/2 before:content-[''] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={() => setSidebarQuery("")}
          >
            <Icon name="close" className="size-[0.8rem]" />
          </button>
        ) : searchFocused ? null : (
          // The shortcut, where a keyboard is likely: a keycap inside the
          // field's right edge. Decorative (the field's aria-keyshortcuts
          // says it to a screen reader).
          <Kbd aria-hidden="true" className="pointer-events-none absolute right-2 any-pointer-coarse:hidden">
            {searchShortcut}
          </Kbd>
        )}
      </div>
      {/* The icon strip keeps only the + (its menu starts a chat too) and
          the top items: search and the pen come back with the sidebar. */}
      {readOnly ? null : (
        <>
          <button
            type="button"
            aria-label={tr("New chat")}
            data-testid="new-chat-button"
            title={tr("New chat")}
            className={[
              "inline-flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] transition hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:size-11",
              railCollapsed ? "sm:hidden" : "",
            ].join(" ")}
            onClick={newChat}
          >
            <Icon name={serverConfig.lockdownOnly ? "lock" : "edit"} className="size-[1.05rem]" />
          </button>
          <NewMenu
            collapsed={railCollapsed}
            newChatShortcut={newChatShortcut ? newChatChord(searchShortcut) : undefined}
            sealedAvailable={serverConfig.lockdownAvailable && !serverConfig.lockdownOnly}
            onNewChat={newChat}
            onNewPatrol={(returnFocusTo) => {
              setSidebarOpen(false);
              openPatrolWizard(returnFocusTo);
            }}
            onNewTask={
              onNewTask === null
                ? undefined
                : () => {
                    setSidebarOpen(false);
                    if (onNewTask) onNewTask();
                    else navigate("/orchestrator?new=1");
                  }
            }
            onNewProject={() => {
              setSidebarOpen(false);
              onCreateProject();
            }}
            onNewSealedChat={() => {
              setSidebarOpen(false);
              clearConversation({ lockdown: true });
            }}
          />
        </>
      )}
    </div>
  );

  // ── Select mode: the bar above the account menu (Chat's; BulkBar.tsx) ──
  const bulkBar = selecting ? (
    <BulkBar
      selectedIds={selectedIds}
      railCollapsed={railCollapsed}
      allLabelNames={allLabelNames}
      onBulkPin={onBulkPin}
      onBulkDelete={onBulkDelete}
      onExitSelectMode={onExitSelectMode}
      onBulkAddLabel={onBulkAddLabel}
    />
  ) : null;

  return (
    <SidebarTargetsContext.Provider value={sidebarTargets}>
      <NavRail
        activeView={activeView}
        opsCount={opsCount}
        brandName={branding.app_name}
        brandLogoSrc={branding.logo_url || undefined}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={collapse}
        account={{ email: userEmail, onSignOut }}
        actions={actions}
        footer={
          bulkBar || notice ? (
            <>
              {notice}
              {bulkBar}
            </>
          ) : null
        }
      >
        {/* The arrow-key handler is delegated from the rows (buttons) inside. */}
        {/* eslint-disable-next-line jsx-a11y/no-static-element-interactions */}
        <div
          ref={listRef}
          onKeyDown={onResultsKeyDown}
          data-testid="rail-lists"
          // Set once the lists are drawn (not the loading row): from then on
          // the rows on screen are the whole of what j/k may walk.
          data-lists-drawn={isLoadingHistory ? undefined : ""}
          className={railCollapsed ? "sm:hidden" : ""}
        >
          {isLoadingHistory ? (
            <p className="m-0 px-2.5 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
          ) : searching ? (
            searchResults
          ) : filtering ? (
            // A label filter narrows Chats to the person's chats carrying it;
            // the other sections step aside until it is cleared.
            chatsSection ?? (
              <>
                {labelFilterChips}
                {filteredConversations.map(renderRow)}
              </>
            )
          ) : (
            railLayout.sections.map((s) => sectionNodes[s.id])
          )}
        </div>
      </NavRail>
    </SidebarTargetsContext.Provider>
  );
}
