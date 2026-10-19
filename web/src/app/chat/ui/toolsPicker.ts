// The Tools picker's rules, shared by the composer popover (ToolsPopover) and
// the library modal (ToolsLibrary): what status a connection is in, what
// counts as on, how search matches and ranks, and how rows group under the
// static category headings. Pure functions over the catalog rows so both
// surfaces agree and the rules are unit-tested in one place
// (toolsPicker.test.ts).

/** The catalog row shape the picker reads (a subset of MCPServerInfo). */
export type PickerConnector = {
  name: string;
  display_name?: string;
  description: string;
  tools: string[];
  tool_count: number;
  enabled: boolean;
  always_on?: boolean;
  beta?: boolean;
  remote?: boolean;
  accounts?: string[];
  default_account?: string;
  account?: string;
  /** A picker category ID; absent or unknown lands in Misc Tools. */
  category?: string;
  /** Server-side availability; absent = available. */
  status?: "available" | "needs_setup" | "unavailable";
};

export type PickerCategory = { id: string; name: string };

/**
 * The five statuses the design names. "on"/"off" have a switch; "always" has
 * a lock and counts as on; "setup" and "unavailable" have no switch and are
 * hidden from the popover until a search matches them.
 */
export type ConnectorStatus = "on" | "off" | "always" | "setup" | "unavailable";

export const MISC_CATEGORY = "misc";

// Used when an older server answers without a category list: the starting set
// from the design, Misc Tools last.
export const FALLBACK_CATEGORIES: PickerCategory[] = [
  { id: "ssps", name: "SSPs" },
  { id: "deal-management", name: "Deal Management" },
  { id: "data-storage", name: "Data & Storage" },
  { id: MISC_CATEGORY, name: "Misc Tools" },
];

export function connectorStatus(c: PickerConnector): ConnectorStatus {
  if (c.status === "needs_setup") return "setup";
  // An always-on row the server could not reach comes back enabled=false.
  if (c.status === "unavailable" || (c.always_on && !c.enabled)) return "unavailable";
  if (c.always_on) return "always";
  return c.enabled ? "on" : "off";
}

/** Has a switch in this chat. */
export function isToggleable(c: PickerConnector): boolean {
  const s = connectorStatus(c);
  return s === "on" || s === "off";
}

/** On in this chat, always-on included (the trigger badge and "4 on"). */
export function isOn(c: PickerConnector): boolean {
  const s = connectorStatus(c);
  return s === "on" || s === "always";
}

/** Listed while browsing the popover (needs setup / unavailable only in search). */
export function isUsable(c: PickerConnector): boolean {
  return isToggleable(c) || connectorStatus(c) === "always";
}

export function connectorLabel(c: PickerConnector): string {
  return c.display_name || c.name;
}

export function countOn(rows: readonly PickerConnector[]): number {
  return rows.filter(isOn).length;
}

export function toolCountLabel(n: number): string {
  return `${n} ${n === 1 ? "tool" : "tools"}`;
}

/** Resolve a row's category against the known list; unknown → Misc Tools. */
export function categoryOf(c: PickerConnector, cats: readonly PickerCategory[]): string {
  const id = c.category || MISC_CATEGORY;
  return cats.some((cat) => cat.id === id) ? id : MISC_CATEGORY;
}

export function categoryName(id: string, cats: readonly PickerCategory[]): string {
  return cats.find((c) => c.id === id)?.name ?? "Misc Tools";
}

/** The category list with Misc Tools forced last, whatever order it came in. */
export function orderedCategories(cats: readonly PickerCategory[] | undefined): PickerCategory[] {
  const list = cats && cats.length > 0 ? [...cats] : [...FALLBACK_CATEGORIES];
  const misc = list.find((c) => c.id === MISC_CATEGORY) ?? { id: MISC_CATEGORY, name: "Misc Tools" };
  return [...list.filter((c) => c.id !== MISC_CATEGORY), misc];
}

// ── Search ──────────────────────────────────────────────────────────────

export type ConnectorMatch = {
  /** Higher ranks first: name starts with (4) > name contains (3) >
   *  category (2) > description (1.5) > an inner tool name (1). */
  score: number;
  /** Tool names that matched, shown under the count ("↳ ix_clone_deal")
   *  when nothing above them matched. */
  toolHits: string[] | null;
};

export function matchConnector(
  c: PickerConnector,
  query: string,
  cats: readonly PickerCategory[],
): ConnectorMatch | null {
  const q = query.trim().toLowerCase();
  if (!q) return { score: 0, toolHits: null };
  const name = connectorLabel(c).toLowerCase();
  if (name.startsWith(q)) return { score: 4, toolHits: null };
  if (name.includes(q)) return { score: 3, toolHits: null };
  if (categoryName(categoryOf(c, cats), cats).toLowerCase().includes(q)) {
    return { score: 2, toolHits: null };
  }
  const hits = (c.tools ?? []).filter((t) => t.toLowerCase().includes(q));
  if ((c.description ?? "").toLowerCase().includes(q)) {
    return { score: 1.5, toolHits: hits.length > 0 ? hits : null };
  }
  if (hits.length > 0) return { score: 1, toolHits: hits };
  return null;
}

export type TextSegment = { text: string; match: boolean };

/** Split text into matched / unmatched runs for highlighting, case-insensitive. */
export function highlightSegments(text: string, query: string): TextSegment[] {
  const q = query.trim();
  if (!q) return [{ text, match: false }];
  const lower = text.toLowerCase();
  const ql = q.toLowerCase();
  const out: TextSegment[] = [];
  let i = 0;
  let j = lower.indexOf(ql, i);
  while (j !== -1) {
    if (j > i) out.push({ text: text.slice(i, j), match: false });
    out.push({ text: text.slice(j, j + q.length), match: true });
    i = j + q.length;
    j = lower.indexOf(ql, i);
  }
  if (i < text.length) out.push({ text: text.slice(i), match: false });
  return out;
}

export type MatchedConnector<T extends PickerConnector = PickerConnector> = {
  connector: T;
  match: ConnectorMatch;
};

export type ConnectorGroup<T extends PickerConnector = PickerConnector> = {
  category: PickerCategory;
  rows: MatchedConnector<T>[];
};

const byLabel = (a: PickerConnector, b: PickerConnector) =>
  connectorLabel(a).localeCompare(connectorLabel(b));

/**
 * Group rows under the static category headings, empty groups dropped. With a
 * query the rows are ranked by score (then name) and the groups by their best
 * match, category order breaking ties (so Misc Tools stays last on a tie);
 * without one both keep category order and rows sort by name.
 */
export function groupConnectors<T extends PickerConnector>(
  rows: readonly MatchedConnector<T>[],
  cats: readonly PickerCategory[],
  ranked: boolean,
): ConnectorGroup<T>[] {
  const sorted = [...rows].sort(
    (a, b) => (ranked ? b.match.score - a.match.score : 0) || byLabel(a.connector, b.connector),
  );
  const groups = cats
    .map((category) => ({
      category,
      rows: sorted.filter((r) => categoryOf(r.connector, cats) === category.id),
    }))
    .filter((g) => g.rows.length > 0);
  if (!ranked) return groups;
  const index = (id: string) => cats.findIndex((c) => c.id === id);
  return groups.sort(
    (a, b) => b.rows[0].match.score - a.rows[0].match.score || index(a.category.id) - index(b.category.id),
  );
}

/** Every row matching the query (all rows when it is empty), with its match. */
export function matchAll<T extends PickerConnector>(
  rows: readonly T[],
  query: string,
  cats: readonly PickerCategory[],
): MatchedConnector<T>[] {
  const out: MatchedConnector<T>[] = [];
  for (const connector of rows) {
    const match = matchConnector(connector, query, cats);
    if (match) out.push({ connector, match });
  }
  return out;
}

// ── Popover ─────────────────────────────────────────────────────────────

export type PopoverSection<T extends PickerConnector = PickerConnector> = {
  key: string;
  label: string;
  /** Right-aligned header text: the on count, a match count, or nothing. */
  count: string;
  rows: MatchedConnector<T>[];
};

/**
 * The popover's sections. Browsing: an "On" section first (the rows that
 * were on when the popover opened — a snapshot, so a row does not jump while
 * you toggle it), then each category's remaining usable rows. Searching:
 * every matching row, needs-setup and unavailable included, under ranked
 * category headings with a match count each.
 */
export function popoverSections<T extends PickerConnector>(
  rows: readonly T[],
  query: string,
  cats: readonly PickerCategory[],
  pinned: ReadonlySet<string>,
): PopoverSection<T>[] {
  if (query.trim()) {
    return groupConnectors(matchAll(rows, query, cats), cats, true).map((g) => ({
      key: g.category.id,
      label: g.category.name,
      count: `${g.rows.length} ${g.rows.length === 1 ? "match" : "matches"}`,
      rows: g.rows,
    }));
  }
  const usable = matchAll(rows.filter(isUsable), "", cats);
  const onRows = usable.filter((r) => pinned.has(r.connector.name));
  const sections: PopoverSection<T>[] = [];
  if (onRows.length > 0) {
    sections.push({
      key: "on",
      label: "On",
      count: String(countOn(rows)),
      // Category order, then name — the same order they would have below.
      rows: groupConnectors(onRows, cats, false).flatMap((g) => g.rows),
    });
  }
  for (const g of groupConnectors(
    usable.filter((r) => !pinned.has(r.connector.name)),
    cats,
    false,
  )) {
    sections.push({ key: g.category.id, label: g.category.name, count: "", rows: g.rows });
  }
  return sections;
}

/** Names of the rows on right now — the "On" section's snapshot at open. */
export function onSnapshot(rows: readonly PickerConnector[]): Set<string> {
  return new Set(rows.filter(isOn).map((r) => r.name));
}

/**
 * The bulk buttons' scope: the visible, toggleable rows. Browsing that is
 * every toggleable connection; with a query, only the matching ones.
 */
export function bulkScope(sections: readonly PopoverSection[]): string[] {
  const names = new Set<string>();
  for (const s of sections) {
    for (const r of s.rows) if (isToggleable(r.connector)) names.add(r.connector.name);
  }
  return [...names];
}

// ── Library ─────────────────────────────────────────────────────────────

export type LibraryStatusFilter = "on" | "off" | "always" | "setup" | "unavailable";

export const LIBRARY_STATUSES: { id: LibraryStatusFilter; label: string }[] = [
  { id: "on", label: "On" },
  { id: "off", label: "Off" },
  { id: "always", label: "Always on" },
  { id: "setup", label: "Needs setup" },
  { id: "unavailable", label: "Unavailable" },
];

/** "On" includes always-on (it counts as on everywhere); the rest are exact. */
export function matchesStatus(c: PickerConnector, filter: LibraryStatusFilter | null): boolean {
  if (!filter) return true;
  if (filter === "on") return isOn(c);
  return connectorStatus(c) === filter;
}

export function inCategory(c: PickerConnector, cat: string, cats: readonly PickerCategory[]): boolean {
  return cat === "all" || categoryOf(c, cats) === cat;
}

export function statusText(s: ConnectorStatus): string {
  switch (s) {
    case "on":
      return "On for this chat";
    case "off":
      return "Off for this chat";
    case "always":
      return "Always on";
    case "setup":
      return "Needs setup";
    case "unavailable":
      return "Unavailable";
  }
}

/** The mono kicker beside a name; on/off rows have none. */
export function statusKicker(s: ConnectorStatus): string {
  switch (s) {
    case "always":
      return "Always on";
    case "setup":
      return "Needs setup";
    case "unavailable":
      return "Unavailable";
    default:
      return "";
  }
}

/**
 * The library's bulk-bar scope line. Only toggleable connections count.
 * "shown" when a search or status filter narrows the list; "in <category>"
 * when only a category does.
 */
export function libraryBulkScope(n: number, opts: { filtered: boolean; categoryName?: string }): string {
  const noun = `${n} ${n === 1 ? "connection" : "connections"}`;
  if (opts.filtered) return `Applies to ${noun} shown`;
  if (opts.categoryName) return `Applies to ${noun} in ${opts.categoryName}`;
  return `Applies to ${noun}`;
}

// ── Accounts ────────────────────────────────────────────────────────────

export type SeatOption = { value: string; label: string; isDefault: boolean };

/**
 * The account menu's options. "" is the default seat; it reads as the
 * connection's default account when it has a named one, else (bundled) the
 * deployment's name — "Elcano", "Reklaim" — since that is whose account it
 * is, or (hosted) the primary login. The default seat is not repeated below.
 */
export function seatOptions(c: PickerConnector, appName: string): SeatOption[] {
  const def = c.default_account ?? "";
  const defaultLabel = def || (c.remote ? "Primary login" : appName);
  return [
    { value: "", label: defaultLabel, isDefault: true },
    ...(c.accounts ?? []).filter((s) => s !== def).map((s) => ({ value: s, label: s, isDefault: false })),
  ];
}

export function seatLabel(c: PickerConnector, appName: string): string {
  const opts = seatOptions(c, appName);
  return (opts.find((o) => o.value === (c.account ?? "")) ?? opts[0]).label;
}
