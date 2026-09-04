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
  title: string;
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

// filterConversations applies labels (AND — every selected label must be
// present) and a case-insensitive title substring query.
export function filterConversations<T extends OrganizableConversation>(
  conversations: readonly T[],
  filter: ConversationFilter,
): T[] {
  const q = filter.query?.trim().toLowerCase() ?? "";
  const labels = filter.labels ?? [];
  return conversations.filter((c) => {
    if (labels.length > 0) {
      const own = c.labels ?? [];
      if (!labels.every((l) => own.includes(l))) return false;
    }
    if (q && !c.title.toLowerCase().includes(q)) return false;
    return true;
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
): T[] {
  return conversations.filter((c) => c.pinned && isUnfiled(c.project_id, knownProjectIds));
}

export function recentUnfiled<T extends OrganizableConversation>(
  conversations: readonly T[],
  knownProjectIds?: ReadonlySet<string>,
): T[] {
  return conversations.filter((c) => !c.pinned && isUnfiled(c.project_id, knownProjectIds));
}

// ── Projects in the rail (#509 follow-up) ────────────────────────────────────

// MAX_RAIL_PROJECTS caps how many projects the rail section shows — the most
// recently updated ones. The rest stay reachable through the Projects modal.
export const MAX_RAIL_PROJECTS = 5;

// ProjectLike is the structural slice of a Project the rail grouping needs
// (the full type lives in ProjectsModal.tsx; a minimal shape keeps this
// module React-free and unit-testable, like OrganizableConversation above).
export type ProjectLike = { id: string; name: string; updated_at: number; pinned?: boolean };

export type ProjectGroup<T, P extends ProjectLike = ProjectLike> = { project: P; chats: T[] };

// projectGroups returns the rail's project tree: pinned projects first (in
// place, no separate section), then the rest by most-recent update, cut to
// the top `max`; each group's conversations keep the caller's list order
// (the server already sorts by updated_at DESC). Because pinned sorts before
// the cut, a pinned project always makes the rail. A project with no chats
// still gets a group — a fresh project must exist in the rail to be a drag
// target. Conversations whose project is NOT in the top slice stay hidden
// from the main list (they're project chats) and are reachable via search,
// filters, or the modal. Generic over the caller's project type so richer
// fields (owner_email for the kebab's owner gate) survive the grouping.
export function projectGroups<T extends OrganizableConversation, P extends ProjectLike>(
  conversations: readonly T[],
  projects: readonly P[],
  max: number = MAX_RAIL_PROJECTS,
): ProjectGroup<T, P>[] {
  return [...projects]
    .sort((a, b) => Number(Boolean(b.pinned)) - Number(Boolean(a.pinned)) || b.updated_at - a.updated_at)
    .slice(0, max)
    .map((project) => ({
      project,
      chats: conversations.filter((c) => c.project_id === project.id),
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
}): T[] {
  if (args.filtering) {
    return [...args.filtered];
  }
  return [
    ...pinnedUnfiled(args.all, args.knownProjectIds),
    ...recentUnfiled(args.all, args.knownProjectIds),
  ];
}
