// Pure helpers for the rail's conversation organization — pinning, folders, and
// labels (#258 contract, served live by the #279 bulk API). Kept free of React
// so the section-derivation and label-validation rules are unit-tested in
// isolation (see conversationOrganization.test.ts), the same way history.ts and
// protocolPills.ts are.
//
// Folders are flat, single-assignment, and *derived* — there is no folders
// table; the set of folders is just the distinct non-empty `folder` values on
// the user's conversations. Labels are multi (max 10, 32 chars each) and colored
// by name-hash (see shared/lib/labelColors). Filing a conversation auto-pins it
// (handled by the caller), and a filed conversation lives in its folder only —
// it is intentionally excluded from the Pinned and Recent sections.

export const MAX_LABELS = 10;
export const MAX_LABEL_LEN = 32;

// OrganizableConversation is the structural slice these helpers read. The real
// ConversationSummary (chat-experience.tsx) is a superset; using a minimal shape
// keeps this module importable from tests without pulling in React/Next.
export type OrganizableConversation = {
  title: string;
  pinned: boolean;
  folder?: string;
  labels?: string[];
};

export type FolderSummary = { name: string; count: number };
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

// deriveFolders materializes the folder list from the conversations themselves —
// distinct non-empty folder names with their conversation counts, sorted
// alphabetically (case-insensitive) for a stable order.
export function deriveFolders(conversations: readonly OrganizableConversation[]): FolderSummary[] {
  const counts = new Map<string, number>();
  for (const c of conversations) {
    const folder = c.folder?.trim();
    if (!folder) continue;
    counts.set(folder, (counts.get(folder) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }));
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
  folder?: string | null;
  labels?: readonly string[];
  query?: string;
};

// isFiltering reports whether any active filter is set, so callers can switch
// between the sectioned view (Pinned/Folders/Labels/Recent) and a flat
// filtered-results view.
export function isFiltering(filter: ConversationFilter): boolean {
  return Boolean(filter.folder) || (filter.labels?.length ?? 0) > 0 || (filter.query?.trim().length ?? 0) > 0;
}

// filterConversations applies folder (exact), labels (AND — every selected label
// must be present), and a case-insensitive title substring query.
export function filterConversations<T extends OrganizableConversation>(
  conversations: readonly T[],
  filter: ConversationFilter,
): T[] {
  const q = filter.query?.trim().toLowerCase() ?? "";
  const labels = filter.labels ?? [];
  return conversations.filter((c) => {
    if (filter.folder && c.folder !== filter.folder) return false;
    if (labels.length > 0) {
      const own = c.labels ?? [];
      if (!labels.every((l) => own.includes(l))) return false;
    }
    if (q && !c.title.toLowerCase().includes(q)) return false;
    return true;
  });
}

// pinnedUnfiled / recentUnfiled split the unsectioned conversations. A filed
// conversation lives only in its folder, so both sections exclude any
// conversation with a folder; "Pinned" is the pinned remainder and "Recent" the
// rest.
export function pinnedUnfiled<T extends OrganizableConversation>(conversations: readonly T[]): T[] {
  return conversations.filter((c) => c.pinned && !c.folder);
}

export function recentUnfiled<T extends OrganizableConversation>(conversations: readonly T[]): T[] {
  return conversations.filter((c) => !c.pinned && !c.folder);
}
