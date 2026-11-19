import { t as tr } from "@/app/lib/i18n";
// Pure helpers for the rail's conversation organization — pinning, labels
// (#258 contract, served live by the #279 bulk API), and projects (#509).
// Kept free of React so the section-derivation and label-validation rules are
// unit-tested in isolation (see conversationOrganization.test.ts), the same
// way history.ts and protocolPills.ts are.
//
// Folders (the old flat, single-assignment buckets) were superseded by
// projects and are gone on BOTH sides now: the UI was removed here, and the
// server dropped the column with it (see internal/httpapi/labels.go). Chats
// that had been filed into one simply live in Pinned/Temporary (filing used to
// auto-pin, so most landed in Pinned). Labels are multi (max 10, 32 chars
// each) and colored by name-hash (see shared/lib/labelColors).

export const MAX_LABELS = 10;
export const MAX_LABEL_LEN = 32;

// OrganizableConversation is the structural slice these helpers read. The real
// ConversationSummary (chat-experience.tsx) is a superset; using a minimal shape
// keeps this module importable from tests without pulling in React/Next.
export type OrganizableConversation = {
  // The conversation's id; only the hand-set order (applyChatOrder) reads it.
  id?: string;
  title: string;
  // Activity and creation times (unix seconds); only sortRecent reads them.
  updated_at?: number;
  created_at?: number;
  pinned: boolean;
  labels?: string[];
  // project_id binds the conversation to a project/space (#509). A project
  // conversation lives ONLY under its project in the rail — it is excluded
  // from Pinned and Chats. (Label filters and search still reach it:
  // filters are explicit user actions.)
  project_id?: string;
};

export type LabelSummary = { name: string; count: number };

// normalizeLabel trims surrounding whitespace and clamps to the max length. The
// backend stores labels verbatim, so normalization is the frontend's job.
export function normalizeLabel(raw: string): string {
  return raw.trim().slice(0, MAX_LABEL_LEN);
}

// canAddLabel reports whether `raw` can be added to `existing`: it must be
// non-empty after normalization, not already present, and within the per-
// conversation cap.
export function canAddLabel(existing: readonly string[], raw: string): boolean {
  const label = normalizeLabel(raw);
  if (!label) return false;
  if (existing.includes(label)) return false;
  return existing.length < MAX_LABELS;
}

// addLabel returns the next label set with `raw` appended, or the original set
// unchanged when it can't be added (empty / duplicate / over cap). Pure: never
// mutates the input.
export function addLabel(existing: readonly string[], raw: string): string[] {
  if (!canAddLabel(existing, raw)) return [...existing];
  return [...existing, normalizeLabel(raw)];
}

// removeLabel returns the next label set with `label` removed.
export function removeLabel(existing: readonly string[], label: string): string[] {
  return existing.filter((l) => l !== label);
}

// deriveLabels materializes the label list — distinct label names across all
// conversations with counts, sorted alphabetically (case-insensitive).
export function deriveLabels(conversations: readonly OrganizableConversation[]): LabelSummary[] {
  const counts = new Map<string, number>();
  for (const c of conversations) {
    for (const label of c.labels ?? []) {
      counts.set(label, (counts.get(label) ?? 0) + 1);
    }
  }
  return [...counts.entries()]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
}

export type ConversationFilter = {
  labels?: readonly string[];
  query?: string;
};

// isFiltering reports whether any active filter is set, so callers can switch
// between the sectioned view (Pinned/Labels/Temporary) and a flat
// filtered-results view.
export function isFiltering(filter: ConversationFilter): boolean {
  return (filter.labels?.length ?? 0) > 0 || (filter.query?.trim().length ?? 0) > 0;
}

// ── Search matching (the rail's search) ──────────────────────────────────────
// One rule for everything the rail searches by name (own chats, team chats,
// archived chats, projects): every word of the query must appear somewhere in
// the text, in any order, ignoring case and accents. "renewal acme" finds
// "Acme renewal"; "resume" finds "Résumé review".

// foldChar lowercases one UTF-16 unit and strips its accents. It maps one
// unit to at most a few, and the callers below keep the index of the unit
// each folded character came from, so a match can be marked in the original.
function foldChar(ch: string): string {
  return ch.normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
}

/** foldSearchText is the form text is compared in: lowercase, no accents. */
export function foldSearchText(text: string): string {
  return text.normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();
}

/** searchTokens splits a query into its folded words ([] for a blank query). */
export function searchTokens(query: string | undefined): string[] {
  return foldSearchText(query ?? "").split(/\s+/).filter(Boolean);
}

/** matchesSearch: every token appears in at least one of the texts. */
export function matchesSearch(tokens: readonly string[], ...texts: readonly (string | undefined)[]): boolean {
  if (tokens.length === 0) return true;
  const folded = texts.map((x) => foldSearchText(x ?? ""));
  return tokens.every((tok) => folded.some((x) => x.includes(tok)));
}

/**
 * searchMatchRanges returns the [start, end) ranges of `text` (in its own,
 * unfolded indices) that match any token, merged and in order — what the
 * rail marks in a result's title.
 */
export function searchMatchRanges(text: string, tokens: readonly string[]): [number, number][] {
  if (tokens.length === 0 || !text) return [];
  // folded[i] came from text[origin[i]].
  let folded = "";
  const origin: number[] = [];
  for (let i = 0; i < text.length; i++) {
    const f = foldChar(text[i]);
    for (let k = 0; k < f.length; k++) origin.push(i);
    folded += f;
  }
  const ranges: [number, number][] = [];
  for (const tok of tokens) {
    for (let at = folded.indexOf(tok); at !== -1; at = folded.indexOf(tok, at + tok.length)) {
      ranges.push([origin[at], origin[at + tok.length - 1] + 1]);
    }
  }
  ranges.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
  const merged: [number, number][] = [];
  for (const r of ranges) {
    const last = merged[merged.length - 1];
    if (last && r[0] <= last[1]) last[1] = Math.max(last[1], r[1]);
    else merged.push([r[0], r[1]]);
  }
  return merged;
}

// filterConversations applies labels (AND — every selected label must be
// present) and the title query (matchesSearch: every word, any order,
// ignoring case and accents).
export function filterConversations<T extends OrganizableConversation>(
  conversations: readonly T[],
  filter: ConversationFilter,
): T[] {
  const tokens = searchTokens(filter.query);
  const labels = filter.labels ?? [];
  return conversations.filter((c) => {
    if (labels.length > 0) {
      const own = c.labels ?? [];
      if (!labels.every((l) => own.includes(l))) return false;
    }
    return matchesSearch(tokens, c.title);
  });
}

// pinnedUnfiled / recentUnfiled split the unsectioned conversations. A
// project conversation lives only under its project, so both sections
// exclude it; "Pinned" is the pinned remainder and "Temporary" the rest.
//
// `knownProjectIds`, when given, is the set of projects the viewer can
// actually see. A chat filed in a project OUTSIDE that set counts as unfiled
// and shows here, because otherwise it shows NOWHERE: the project section only
// iterates projects the viewer has, so losing access to a project (leaving the
// team, an admin move, the owner re-sharing it elsewhere) made the viewer's
// OWN chats vanish from the rail with nothing to explain it. Omitted, both
// functions behave exactly as before.
function isUnfiled(projectID: string | undefined, known?: ReadonlySet<string>): boolean {
  if (!projectID) return true;
  return known !== undefined && !known.has(projectID);
}

export function pinnedUnfiled<T extends OrganizableConversation>(
  conversations: readonly T[],
  knownProjectIds?: ReadonlySet<string>,
  pinOrder?: readonly string[],
): T[] {
  const pinned = conversations.filter((c) => c.pinned && isUnfiled(c.project_id, knownProjectIds));
  return pinOrder && pinOrder.length > 0 ? applyChatOrder(pinned, pinOrder) : pinned;
}

// pinnedGroup is the rail's one Pinned group in the one-list view: the
// person's own pinned chats and the teammates' chats they have pinned for
// themselves (docs/SIDEBAR.md), in the person's saved order.
//
// `own` is the own pinned chats most recent first (pinnedUnfiled with no
// order) and `shared` the pinned teammates' chats. Before any order
// is saved the two interleave by recent activity; the merge never reorders
// `own` within itself, so the own rows of the result are exactly what
// pinnedUnfiled gives with the same order — which is what j/k walks
// (visibleConversationOrder), so the rendered rows and the walk cannot drift.
export function pinnedGroup<T extends { id?: string; updated_at?: number }>(
  own: readonly T[],
  shared: readonly T[],
  pinOrder?: readonly string[],
): T[] {
  // The shared list is not always in activity order (a chat opened by link
  // is put at its head), so it is sorted here; `own` is taken as it comes.
  const team = shared
    .map((c, at) => ({ c, at }))
    .sort((a, b) => (b.c.updated_at ?? 0) - (a.c.updated_at ?? 0) || a.at - b.at)
    .map((x) => x.c);
  const merged: T[] = [];
  let i = 0;
  let j = 0;
  while (i < own.length || j < team.length) {
    const takeTeam = i >= own.length || (j < team.length && (team[j].updated_at ?? 0) > (own[i].updated_at ?? 0));
    merged.push(takeTeam ? team[j++] : own[i++]);
  }
  return pinOrder && pinOrder.length > 0 ? applyChatOrder(merged, pinOrder) : merged;
}

// ── Hand-set chat order (docs/CHAT-ORDER.md) ─────────────────────────────

// The scopes a person can put in their own order: their pinned chats, and
// their chats in each project. Mirrors the server (store.ChatOrderPinned,
// store.ChatOrderProjectScope).
export const PINNED_ORDER_SCOPE = "pinned";
export const projectOrderScope = (projectId: string) => `project:${projectId}`;

// MAX_CHAT_ORDER mirrors the server's cap on one saved order
// (store.MaxChatOrder): the rail offers reordering only up to this many chats.
export const MAX_CHAT_ORDER = 500;

// applyChatOrder applies a saved order to a scope's chats. A chat the order
// does not name — pinned or filed since the last reorder — goes ABOVE the
// ordered ones, in the caller's (most recent first) order, so a new arrival
// lands where the person just looked rather than at the bottom of a long
// list. Names in the order that have left the scope are ignored. Stable and
// pure: the same inputs give the same rows.
export function applyChatOrder<T extends { id?: string }>(chats: readonly T[], order: readonly string[]): T[] {
  if (order.length === 0) return [...chats];
  const rank = new Map<string, number>();
  order.forEach((id, i) => {
    if (!rank.has(id)) rank.set(id, i);
  });
  const fresh: T[] = [];
  const ranked: { c: T; r: number }[] = [];
  for (const c of chats) {
    const r = c.id === undefined ? undefined : rank.get(c.id);
    if (r === undefined) fresh.push(c);
    else ranked.push({ c, r });
  }
  ranked.sort((a, b) => a.r - b.r);
  return [...fresh, ...ranked.map((x) => x.c)];
}

// ── Temporary chats: a sort, not an order ────────────────────────────────────

// Temporary chats expire with inactivity, so they get a SORT the person
// picks rather than a hand-set order that new activity would keep undoing.
//   • "activity" — most recently active first (the default; the server's order)
//   • "created"  — most recently started first
//   • "title"    — A to Z by title
export type RecentSort = "activity" | "created" | "title";
export const RECENT_SORTS: readonly { value: RecentSort; label: string }[] = [
  { value: "activity", label: tr("Recent activity") },
  { value: "created", label: tr("Newest first") },
  { value: "title", label: tr("Title A–Z") },
];

// Where the chosen sort is remembered (browser storage, per device).
export const RECENT_SORT_ITEM = "fleet.rail.recentSort";

export function isRecentSort(v: unknown): v is RecentSort {
  return v === "activity" || v === "created" || v === "title";
}

type Sortable = { title: string; updated_at?: number; created_at?: number };

// sortRecent orders the Temporary list. Ties (and a missing created_at, from
// an older server) fall back to recent activity, then to the caller's order,
// so the result is stable.
export function sortRecent<T extends Sortable>(chats: readonly T[], sort: RecentSort = "activity"): T[] {
  const indexed = chats.map((c, i) => ({ c, i }));
  const byActivity = (a: { c: T; i: number }, b: { c: T; i: number }) =>
    (b.c.updated_at ?? 0) - (a.c.updated_at ?? 0) || a.i - b.i;
  const cmp =
    sort === "created"
      ? (a: { c: T; i: number }, b: { c: T; i: number }) =>
          (b.c.created_at ?? b.c.updated_at ?? 0) - (a.c.created_at ?? a.c.updated_at ?? 0) || byActivity(a, b)
      : sort === "title"
        ? (a: { c: T; i: number }, b: { c: T; i: number }) =>
            (a.c.title || "").localeCompare(b.c.title || "", undefined, { sensitivity: "base", numeric: true }) ||
            byActivity(a, b)
        : byActivity;
  return indexed.sort(cmp).map((x) => x.c);
}

// dragTargetIndex is where a row dragged `dy` pixels from position `from`
// lands in a list of `count` rows spaced `step` pixels apart: the nearest
// slot, never outside the list. The reorder list's pointer drag uses it for
// both the preview and the drop, so what is shown is what is saved.
export function dragTargetIndex(from: number, dy: number, step: number, count: number): number {
  if (count <= 0) return 0;
  if (!(step > 0) || !Number.isFinite(dy)) return Math.max(0, Math.min(count - 1, from));
  return Math.max(0, Math.min(count - 1, from + Math.round(dy / step)));
}

// movePinned returns the IDs with the one at `from` moved to `to`
// (both indexes into `ids`, clamped), for the rail's reorder mode — a drag
// drop and the Move up / Move down buttons both come through here.
export function movePinned(ids: readonly string[], from: number, to: number): string[] {
  const next = [...ids];
  if (from < 0 || from >= next.length) return next;
  const target = Math.max(0, Math.min(next.length - 1, to));
  const [moved] = next.splice(from, 1);
  next.splice(target, 0, moved);
  return next;
}

export function recentUnfiled<T extends OrganizableConversation>(
  conversations: readonly T[],
  knownProjectIds?: ReadonlySet<string>,
): T[] {
  return conversations.filter((c) => !c.pinned && isUnfiled(c.project_id, knownProjectIds));
}

// ── Projects in the rail (#509 follow-up) ────────────────────────────────────

// ProjectLike is the structural slice of a Project the rail grouping needs
// (the full type lives in ProjectsModal.tsx; a minimal shape keeps this
// module React-free and unit-testable, like OrganizableConversation above).
export type ProjectLike = { id: string; name: string; updated_at: number; pinned?: boolean };

export type ProjectGroup<T, P extends ProjectLike = ProjectLike> = { project: P; chats: T[] };

// projectGroups returns the rail's project tree: pinned projects first (in
// place, no separate section), then the rest by the project's own most recent
// update (its updated_at — a settings change, not chat activity); each
// group's conversations keep the caller's list order (the server already sorts
// by updated_at DESC). Every project is returned unless the caller passes
// `max` (search caps the tree to its matches). The rail used to cut this to
// the five freshest projects, with "the rest reachable through the Projects
// modal" — but nothing in the rail opened that modal once a project existed,
// so a sixth project, and every chat filed in it (project chats live only
// under their project), simply vanished. The rail's Projects section is its
// own scroller (max 30vh), so a long list scrolls there like Chats does. A
// project with no chats still gets a group — a fresh project must exist in the
// rail to be a drag target. Generic over the caller's project type so richer
// fields (owner_email for the kebab's owner gate) survive the grouping.
export function projectGroups<T extends OrganizableConversation, P extends ProjectLike>(
  conversations: readonly T[],
  projects: readonly P[],
  max: number = Number.POSITIVE_INFINITY,
  // Saved chat orders by scope; a project's own ("project:<id>") orders its
  // chats (applyChatOrder). Absent: the caller's list order.
  orders?: Readonly<Record<string, readonly string[]>>,
): ProjectGroup<T, P>[] {
  // One pass over the conversations, not one per project: with every project
  // listed, a per-project filter is O(projects × conversations) on each render.
  const byProject = new Map<string, T[]>();
  for (const c of conversations) {
    if (!c.project_id) continue;
    const list = byProject.get(c.project_id);
    if (list) list.push(c);
    else byProject.set(c.project_id, [c]);
  }
  return [...projects]
    .sort((a, b) => Number(Boolean(b.pinned)) - Number(Boolean(a.pinned)) || b.updated_at - a.updated_at)
    .slice(0, max)
    .map((project) => ({
      project,
      chats: applyChatOrder(byProject.get(project.id) ?? [], orders?.[projectOrderScope(project.id)] ?? []),
    }));
}

// visibleConversationOrder is the SINGLE source of truth for the flat, top-to-
// bottom order the sidebar shows — so keyboard j/k navigation (in the parent)
// and the rendered rows (in ConversationSidebar) can never drift. When a filter
// is active the sidebar shows filteredConversations verbatim; otherwise it shows
// pinned-unfiled then recent-unfiled (project conversations live only under
// their project and are not keyboard-navigable from the main list).
// Archived rows are a separate collapsible section and are intentionally
// excluded.
export function visibleConversationOrder<T extends OrganizableConversation>(args: {
  all: readonly T[];
  filtered: readonly T[];
  filtering: boolean;
  // The projects the viewer can see — see pinnedUnfiled. Must match what the
  // sidebar passes, or j/k navigation drifts from the rendered rows.
  knownProjectIds?: ReadonlySet<string>;
  // The person's own order for their pinned chats (applyChatOrder) and the
  // Temporary sort (sortRecent). Must match what the sidebar passes, for the
  // same reason.
  pinOrder?: readonly string[];
  recentSort?: RecentSort;
}): T[] {
  if (args.filtering) {
    return [...args.filtered];
  }
  return [
    ...pinnedUnfiled(args.all, args.knownProjectIds, args.pinOrder),
    ...sortRecent(recentUnfiled(args.all, args.knownProjectIds), args.recentSort),
  ];
}

// railProjectEmptyState picks which empty-state copy a project group in the
// rail shows when it holds none of the VIEWER'S OWN chats.
//
// The rail lists only the viewer's own chats, deliberately: a team-shared
// chat's one discovery surface is the project home's Team section (ADR-0057),
// and duplicating it here would be the second organizing axis that ADR
// rejected. But "No chats yet" was then a false statement to a teammate whose
// colleagues had filed two shared chats into the project — the rail said the
// project was empty while its home listed content. So the copy is
// viewer-aware: it says the project holds nothing OF THEIRS and points at the
// home, without moving the shared chats into the rail.
//
//   • "no-chats"           — nothing here for anyone. The filing-paths copy.
//   • "team-has-chats"     — teammates have shared `teamSharedChatCount` chats
//                            into it; the copy quotes the number and links to
//                            the home.
//   • "team-count-unknown" — a team-shared project whose count has not loaded
//                            (or failed to). Point at the home, assert NO
//                            number: a count we don't have must not be
//                            invented, and "empty" is exactly the claim that
//                            was wrong.
//
// A PERSONAL project resolves to "no-chats" without consulting any count:
// team sharing is refused outside a team-shared project and every way of
// removing that home clears the flag (ADR-0057), so zero there is certain
// rather than merely unknown.
export type RailProjectEmptyState =
  | { kind: "no-chats" }
  // count is > 0 by construction, so the copy can quote it without a fallback.
  | { kind: "team-has-chats"; count: number }
  | { kind: "team-count-unknown" };

export function railProjectEmptyState(args: {
  // Whether the project is shared with a team (Project.team_id is set).
  teamShared: boolean;
  // How many team-shared chats OTHER members contributed to this project.
  // undefined = not known (never loaded, or the read failed).
  teamSharedChatCount?: number;
}): RailProjectEmptyState {
  if (!args.teamShared) return { kind: "no-chats" };
  const count = args.teamSharedChatCount;
  if (count === undefined) return { kind: "team-count-unknown" };
  return count > 0 ? { kind: "team-has-chats", count } : { kind: "no-chats" };
}
