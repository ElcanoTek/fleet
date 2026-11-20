import { t as tr } from "@/app/lib/i18n";

// The sidebar layout (docs/SIDEBAR.md): what the left rail shows, in what
// order, under what names. One JSON document per person, saved on the server
// (GET/PUT/DELETE /api/sidebar), with a workspace default an admin can set
// (/api/admin/sidebar). The effective layout is the person's own, else the
// workspace default, else BUILTIN_LAYOUT below — which is exactly the rail as
// it was before layouts existed, so nobody's rail changes until they (or an
// admin) change it.
//
// The server validates the same rules (internal/sidebar); this module keeps a
// mirror so the editor can refuse a bad value before a round trip and so a
// layout from an older or newer server still renders: normalizeLayout never
// throws, it repairs.

export const LAYOUT_VERSION = 1;

/** The rail's fixed destinations. Always present; they can be hidden, renamed and moved. */
export const BUILTIN_IDS = ["chat", "patrols", "tasks"] as const;
export type BuiltinId = (typeof BUILTIN_IDS)[number];

/**
 * The sidebar's lists, in their default order. "team" is teammates' shared
 * chats as a list of their own, used only with `separate_lists`.
 */
export const SECTION_IDS = ["pinned", "projects", "chats", "team"] as const;
export type SectionId = (typeof SECTION_IDS)[number];

export const PAGE_REFS = ["analytics", "help", "settings"] as const;
export type PageRef = (typeof PAGE_REFS)[number];

export type ItemKind = "builtin" | "chat" | "project" | "task" | "page";

export type SidebarItem = {
  id: string;
  kind: ItemKind;
  /** The target: a conversation, project or task id, or a PageRef. */
  ref?: string;
  /** The target's own name when it was added, shown when the live one is not known. */
  title?: string;
  /** The person's own name for it; empty means the default. */
  label?: string;
  /** A sprite icon name from ICON_CHOICES, or an emoji. Empty means the kind's default. */
  icon?: string;
  /** Chats only: open it as a dashboard (docs/DASHBOARD-VIEW.md). */
  view?: "dashboard";
  hidden?: boolean;
};

export type SidebarSection = {
  id: SectionId;
  label?: string;
  hidden?: boolean;
  collapsed?: boolean;
};

export type SidebarLayout = {
  version: number;
  items: SidebarItem[];
  sections: SidebarSection[];
  /** The item opened when fleet is opened at /chat; "" for the usual most recent chat. */
  home?: string;
  /** Hide the one-line descriptions under the rail's destinations. */
  compact?: boolean;
  /**
   * Separate lists: teammates' shared chats get a Team chats section of
   * their own. Unset — the default, and every layout saved before this
   * existed — is one list for all chats: they are listed inside the Chats
   * section with the person's own (docs/SIDEBAR.md).
   */
  separate_lists?: boolean;
  /**
   * Rows say nothing but their title: no patrol next check, sharing, Live or
   * days-left mark. Unset — the default — shows one status when it matters.
   */
  status_off?: boolean;
  /** The Chats list is one run, not grouped under Today, Yesterday, This week, Older. */
  dates_off?: boolean;
};

export const MAX_ITEMS = 60;
export const MAX_LABEL = 60;
export const MAX_SECTION_LABEL = 40;
export const MAX_TITLE = 120;

/** The icons a person can choose (all in Flag's core sprite). */
export const ICON_CHOICES = [
  "message", "bar-chart", "activity", "grid", "target", "sparkles", "zap", "briefcase",
  "database", "globe", "mail", "megaphone", "shield", "book", "folder", "layers",
  "dollar-sign", "shopping-cart", "brain", "calendar", "clock", "pin", "tag", "headphones",
  "file-text", "map-pin", "cloud", "bug", "persona", "tools", "plug", "puzzle",
] as const;

const ICON_SET = new Set<string>(ICON_CHOICES);
const ITEM_ID = /^[a-z0-9][a-z0-9_-]{0,39}$/;
const REF = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/;
// Control and format characters never belong in a name shown in the rail:
// C0/C1 controls, zero-width space and the line/paragraph separators.
function isControl(cp: number): boolean {
  return cp < 0x20 || (cp >= 0x7f && cp <= 0x9f) || cp === 0x200b || cp === 0x2028 || cp === 0x2029;
}

function stripControls(s: string): string {
  return Array.from(s)
    .filter((c) => !isControl(c.codePointAt(0) ?? 0))
    .join("");
}

export const BUILTIN_LAYOUT: SidebarLayout = {
  version: LAYOUT_VERSION,
  items: BUILTIN_IDS.map((id) => ({ id, kind: "builtin" as const })),
  sections: SECTION_IDS.map((id) => ({ id })),
  home: "",
};

/**
 * isEmojiIcon: a short run of non-ASCII characters (an emoji, possibly with
 * modifiers). The same rules as the server's (internal/sidebar validIcon), so
 * the editor never accepts an icon a save would then refuse: no ASCII, no
 * control, separator (any space) or format character except the zero-width
 * joiner that joins emoji and the tag characters of a subdivision flag.
 */
const SEPARATOR = /\p{Z}/u;
const FORMAT = /\p{Cf}/u;
const CONTROL_CHAR = /\p{Cc}/u;
export function isEmojiIcon(icon: string): boolean {
  const chars = Array.from(icon);
  if (chars.length < 1 || chars.length > 8) return false;
  return chars.every((c) => {
    const cp = c.codePointAt(0) ?? 0;
    if (cp < 0x80 || cp === 0xfffd || CONTROL_CHAR.test(c) || SEPARATOR.test(c)) return false;
    if (FORMAT.test(c)) return cp === 0x200d || (cp >= 0xe0020 && cp <= 0xe007f);
    return true;
  });
}

export function isValidIcon(icon: string | undefined): boolean {
  if (!icon) return true;
  return ICON_SET.has(icon) || isEmojiIcon(icon);
}

export function cleanText(s: string | undefined, max: number): string {
  const cleaned = stripControls(s ?? "").trim();
  return Array.from(cleaned).slice(0, max).join("");
}

export function isBuiltinId(id: string): id is BuiltinId {
  return (BUILTIN_IDS as readonly string[]).includes(id);
}

function isSectionId(id: string): id is SectionId {
  return (SECTION_IDS as readonly string[]).includes(id);
}

function isPageRef(ref: string): ref is PageRef {
  return (PAGE_REFS as readonly string[]).includes(ref);
}

function normalizeItem(raw: unknown): SidebarItem | null {
  if (!raw || typeof raw !== "object") return null;
  const r = raw as Record<string, unknown>;
  const id = typeof r.id === "string" ? r.id : "";
  const kind = r.kind;
  if (!ITEM_ID.test(id)) return null;
  const item: SidebarItem = { id, kind: "builtin" };
  if (kind === "builtin") {
    if (!isBuiltinId(id)) return null;
  } else if (kind === "chat" || kind === "project" || kind === "task" || kind === "page") {
    if (isBuiltinId(id)) return null;
    const ref = typeof r.ref === "string" ? r.ref : "";
    if (kind === "page" ? !isPageRef(ref) : !REF.test(ref)) return null;
    item.kind = kind;
    item.ref = ref;
    const title = cleanText(typeof r.title === "string" ? r.title : "", MAX_TITLE);
    if (title) item.title = title;
  } else {
    return null;
  }
  const label = cleanText(typeof r.label === "string" ? r.label : "", MAX_LABEL);
  if (label) item.label = label;
  const icon = typeof r.icon === "string" ? r.icon : "";
  if (icon && isValidIcon(icon)) item.icon = icon;
  if (r.view === "dashboard" && item.kind === "chat") item.view = "dashboard";
  if (r.hidden === true) item.hidden = true;
  return item;
}

/**
 * normalizeLayout turns anything (a stored layout, a server answer, an older
 * or newer version) into a layout the rail can render: invalid items are
 * dropped, duplicates removed, every builtin and section present exactly
 * once, and `home` cleared if it names nothing usable. It never throws.
 */
export function normalizeLayout(raw: unknown): SidebarLayout {
  if (!raw || typeof raw !== "object") return cloneLayout(BUILTIN_LAYOUT);
  const r = raw as Record<string, unknown>;
  const items: SidebarItem[] = [];
  const seen = new Set<string>();
  for (const one of Array.isArray(r.items) ? r.items : []) {
    const item = normalizeItem(one);
    if (!item || seen.has(item.id) || items.length >= MAX_ITEMS) continue;
    seen.add(item.id);
    items.push(item);
  }
  for (const id of BUILTIN_IDS) {
    if (!seen.has(id)) items.push({ id, kind: "builtin" });
  }
  // "chat" is in every layout and never a row (topItems): it rides first, so
  // the top of the sidebar is exactly items[1:], in order.
  const chatAt = items.findIndex((i) => i.id === "chat");
  if (chatAt > 0) items.unshift(...items.splice(chatAt, 1));
  const sections: SidebarSection[] = [];
  const seenSections = new Set<string>();
  for (const one of Array.isArray(r.sections) ? r.sections : []) {
    if (!one || typeof one !== "object") continue;
    const s = one as Record<string, unknown>;
    const id = typeof s.id === "string" ? s.id : "";
    if (!isSectionId(id) || seenSections.has(id)) continue;
    seenSections.add(id);
    const section: SidebarSection = { id };
    const label = cleanText(typeof s.label === "string" ? s.label : "", MAX_SECTION_LABEL);
    if (label) section.label = label;
    if (s.hidden === true) section.hidden = true;
    if (s.collapsed === true) section.collapsed = true;
    sections.push(section);
  }
  // A layout saved before Pinned was a section of its own gets it where it
  // has always been drawn: first.
  if (!seenSections.has("pinned")) sections.unshift({ id: "pinned" });
  for (const id of SECTION_IDS) {
    if (id !== "pinned" && !seenSections.has(id)) sections.push({ id });
  }
  const layout: SidebarLayout = { version: LAYOUT_VERSION, items, sections, home: "" };
  if (r.compact === true) layout.compact = true;
  if (r.separate_lists === true) layout.separate_lists = true;
  if (r.status_off === true) layout.status_off = true;
  if (r.dates_off === true) layout.dates_off = true;
  layout.home = validHome(layout, typeof r.home === "string" ? r.home : "");
  return layout;
}

// The start page names an item. It need not be at the top of the sidebar: a
// chat, project, task or page the person set as their start page from its
// menu is kept in the layout hidden (not a top item) and is still where fleet
// opens. A hidden Background tasks is not: hiding a destination says "I do
// not use this". Patrols is never a start page.
function validHome(layout: SidebarLayout, home: string): string {
  if (!home) return "";
  const item = layout.items.find((i) => i.id === home);
  if (!item || item.id === "patrols") return "";
  if (item.hidden && item.kind === "builtin") return "";
  return home;
}

export function cloneLayout(layout: SidebarLayout): SidebarLayout {
  return JSON.parse(JSON.stringify(layout)) as SidebarLayout;
}

/** sameLayout: structural equality after normalizing both sides. */
export function sameLayout(a: SidebarLayout, b: SidebarLayout): boolean {
  return JSON.stringify(normalizeLayout(a)) === JSON.stringify(normalizeLayout(b));
}

/** isBuiltinLayout: the layout renders exactly as the rail did before layouts. */
export function isBuiltinLayout(layout: SidebarLayout): boolean {
  return sameLayout(layout, BUILTIN_LAYOUT);
}

// ── Edits. Each returns a new, normalized layout. ──────────────────────────

/** A fresh shortcut id, unique within the layout. */
export function newItemId(layout: SidebarLayout): string {
  const taken = new Set(layout.items.map((i) => i.id));
  for (;;) {
    const id = `s-${Math.random().toString(36).slice(2, 9)}`;
    if (!taken.has(id)) return id;
  }
}

export type ShortcutTarget = {
  kind: Exclude<ItemKind, "builtin">;
  ref: string;
  title?: string;
  view?: "dashboard";
  icon?: string;
};

/** findShortcut: the item already pointing at this target, if any. */
export function findShortcut(layout: SidebarLayout, kind: ItemKind, ref: string): SidebarItem | undefined {
  return layout.items.find((i) => i.kind === kind && i.ref === ref);
}

/**
 * addShortcut puts a target in the rail, right after the last visible item
 * so it lands where the person is looking. Adding one already there
 * un-hides it (and updates its view) instead of making a second copy.
 */
export function addShortcut(layout: SidebarLayout, target: ShortcutTarget): SidebarLayout {
  const next = cloneLayout(layout);
  const existing = findShortcut(next, target.kind, target.ref);
  if (existing) {
    existing.hidden = undefined;
    if (target.kind === "chat") existing.view = target.view;
    if (target.title) existing.title = cleanText(target.title, MAX_TITLE);
    return normalizeLayout(next);
  }
  if (next.items.length >= MAX_ITEMS) return normalizeLayout(next);
  const item: SidebarItem = {
    id: newItemId(next),
    kind: target.kind,
    ref: target.ref,
    title: cleanText(target.title, MAX_TITLE) || undefined,
    view: target.kind === "chat" ? target.view : undefined,
    icon: target.icon,
  };
  next.items.push(item);
  return normalizeLayout(next);
}

/** isKeptAtTop: this target is one of the sidebar's top items. */
export function isKeptAtTop(layout: SidebarLayout, kind: ItemKind, ref: string): boolean {
  const item = findShortcut(layout, kind, ref);
  return Boolean(item && !item.hidden);
}

/** keepAtTop puts a target among the top items ("Move to the top"). */
export function keepAtTop(layout: SidebarLayout, target: ShortcutTarget): SidebarLayout {
  return addShortcut(layout, target);
}

/**
 * removeFromTop takes a target out of the top items. One that is the start
 * page stays in the layout, hidden, so fleet still opens there.
 */
export function removeFromTop(layout: SidebarLayout, kind: ItemKind, ref: string): SidebarLayout {
  const item = findShortcut(layout, kind, ref);
  if (!item) return normalizeLayout(layout);
  if (layout.home === item.id) return updateItem(layout, item.id, { hidden: true });
  return removeItem(layout, item.id);
}

/** isStartPage: fleet opens to this target. */
export function isStartPage(layout: SidebarLayout, kind: ItemKind, ref: string): boolean {
  const item = findShortcut(layout, kind, ref);
  return Boolean(item && layout.home === item.id);
}

/**
 * setStartPage makes a target the page fleet opens to, wherever it is in the
 * sidebar. A target that is not a top item is added to the layout hidden: it
 * becomes the start page without moving.
 */
export function setStartPage(layout: SidebarLayout, target: ShortcutTarget): SidebarLayout {
  const next = cloneLayout(layout);
  let item = findShortcut(next, target.kind, target.ref);
  if (!item) {
    if (next.items.length >= MAX_ITEMS) return normalizeLayout(next);
    item = {
      id: newItemId(next),
      kind: target.kind,
      ref: target.ref,
      title: cleanText(target.title, MAX_TITLE) || undefined,
      view: target.kind === "chat" ? target.view : undefined,
      hidden: true,
    };
    next.items.push(item);
  }
  next.home = item.id;
  // The item that was the start page, if it was kept only for that, goes.
  next.items = next.items.filter((i) => i.kind === "builtin" || !i.hidden || i.id === next.home);
  return normalizeLayout(next);
}

export function removeItem(layout: SidebarLayout, id: string): SidebarLayout {
  if (isBuiltinId(id)) return normalizeLayout(layout);
  const next = cloneLayout(layout);
  next.items = next.items.filter((i) => i.id !== id);
  if (next.home === id) next.home = "";
  return normalizeLayout(next);
}

export function updateItem(layout: SidebarLayout, id: string, patch: Partial<Omit<SidebarItem, "id" | "kind" | "ref">>): SidebarLayout {
  const next = cloneLayout(layout);
  next.items = next.items.map((i) => {
    if (i.id !== id) return i;
    const merged: SidebarItem = { ...i, ...patch };
    for (const k of Object.keys(merged) as (keyof SidebarItem)[]) {
      if (merged[k] === undefined || merged[k] === "" || merged[k] === false) delete merged[k];
    }
    return merged;
  });
  return normalizeLayout(next);
}

/** moveItem moves an item by `delta` places (clamped to the list). */
export function moveItem(layout: SidebarLayout, id: string, delta: number): SidebarLayout {
  const next = cloneLayout(layout);
  const from = next.items.findIndex((i) => i.id === id);
  if (from < 0) return normalizeLayout(next);
  const to = Math.max(0, Math.min(next.items.length - 1, from + delta));
  const [it] = next.items.splice(from, 1);
  next.items.splice(to, 0, it);
  return normalizeLayout(next);
}

/** moveItemTo places an item at index `to` (a drag-and-drop drop). */
export function moveItemTo(layout: SidebarLayout, id: string, to: number): SidebarLayout {
  const from = layout.items.findIndex((i) => i.id === id);
  if (from < 0) return normalizeLayout(layout);
  return moveItem(layout, id, to - from);
}

/**
 * setHome makes an item already in the layout the page fleet opens to. ""
 * clears it: fleet then opens the last chat the person had open. The item
 * stays where it is; a start page does not have to be at the top.
 */
export function setHome(layout: SidebarLayout, id: string): SidebarLayout {
  const next = cloneLayout(layout);
  next.home = id && id !== "patrols" && next.items.some((i) => i.id === id) ? id : "";
  // An item kept only to be the start page goes with the job.
  next.items = next.items.filter((i) => i.kind === "builtin" || !i.hidden || i.id === next.home);
  return normalizeLayout(next);
}

/**
 * The teammates' shared chats, three ways: mixed into Chats with the
 * person's own ("chats", the default), as a Team chats list of their own
 * ("own"), or not listed at all ("hidden"; search still finds them).
 */
export type TeamChatsMode = "chats" | "own" | "hidden";

export function teamChatsMode(layout: SidebarLayout): TeamChatsMode {
  if (sectionOf(layout, "team").hidden) return "hidden";
  return layout.separate_lists ? "own" : "chats";
}

export function setTeamChatsMode(layout: SidebarLayout, mode: TeamChatsMode): SidebarLayout {
  const next = updateSection(layout, "team", { hidden: mode === "hidden" });
  // Hiding them keeps which of the two lists they were in, so showing them
  // again (the gear's switch) puts them back where they were.
  if (mode !== "hidden") next.separate_lists = mode === "own" ? true : undefined;
  return normalizeLayout(next);
}

export function updateSection(layout: SidebarLayout, id: SectionId, patch: Partial<Omit<SidebarSection, "id">>): SidebarLayout {
  const next = cloneLayout(layout);
  next.sections = next.sections.map((s) => {
    if (s.id !== id) return s;
    const merged: SidebarSection = { ...s, ...patch };
    if (!merged.label) delete merged.label;
    if (!merged.hidden) delete merged.hidden;
    if (!merged.collapsed) delete merged.collapsed;
    return merged;
  });
  return normalizeLayout(next);
}

export function moveSection(layout: SidebarLayout, id: SectionId, delta: number): SidebarLayout {
  const next = cloneLayout(layout);
  const from = next.sections.findIndex((s) => s.id === id);
  if (from < 0) return normalizeLayout(next);
  const to = Math.max(0, Math.min(next.sections.length - 1, from + delta));
  const [s] = next.sections.splice(from, 1);
  next.sections.splice(to, 0, s);
  return normalizeLayout(next);
}

/**
 * topItems: what the top of the sidebar shows, in order — Patrols, Background
 * tasks and the person's kept shortcuts, minus what they hid. The "chat"
 * builtin stays in every layout (older clients and the server expect it) but
 * is never a row: starting a chat is the pen button beside the search field.
 */
export function topItems(layout: SidebarLayout): SidebarItem[] {
  return layout.items.filter((i) => !i.hidden && i.id !== "chat");
}

export function sectionOf(layout: SidebarLayout, id: SectionId): SidebarSection {
  return layout.sections.find((s) => s.id === id) ?? { id };
}

// ── Presets: starting points for the ways people use fleet. ───────────────
// A preset rearranges the top items and the lists only. What the person
// keeps at the top, what they pinned and their start page all stay.

export type PresetId = "classic" | "chat" | "dashboards" | "automation" | "team";

export type Preset = {
  id: PresetId;
  name: string;
  description: string;
  apply: (layout: SidebarLayout) => SidebarLayout;
};

function arrange(
  layout: SidebarLayout,
  opts: {
    order: (BuiltinId | "shortcuts")[];
    hide?: BuiltinId[];
    sections: SectionId[];
    hideSections?: SectionId[];
    separateLists?: boolean;
  },
): SidebarLayout {
  const shortcuts = layout.items.filter((i) => i.kind !== "builtin");
  const builtins = new Map(layout.items.filter((i) => i.kind === "builtin").map((i) => [i.id, i]));
  const items: SidebarItem[] = [];
  // "chat" is in every layout and never a row (topItems); it rides first.
  for (const slot of ["chat" as const, ...opts.order]) {
    if (slot === "shortcuts") items.push(...shortcuts);
    else {
      const b = { ...(builtins.get(slot) ?? { id: slot, kind: "builtin" as const }) };
      if (opts.hide?.includes(slot)) b.hidden = true;
      else delete b.hidden;
      items.push(b);
    }
  }
  const oldSections = new Map(layout.sections.map((s) => [s.id, s]));
  const sections = opts.sections.map((id) => {
    const s: SidebarSection = { id };
    const label = oldSections.get(id)?.label;
    if (label) s.label = label;
    if (opts.hideSections?.includes(id)) s.hidden = true;
    return s;
  });
  const next: SidebarLayout = {
    version: LAYOUT_VERSION,
    items,
    sections,
    // The start page stays, unless the preset hides the destination it is.
    home: layout.home && !opts.hide?.includes(layout.home as BuiltinId) ? layout.home : "",
    separate_lists: opts.separateLists || undefined,
    // How dense the rows are and how much each says is the person's choice,
    // like their pins: a preset arranges the sidebar, it does not restyle it.
    compact: layout.compact || undefined,
    status_off: layout.status_off || undefined,
    dates_off: layout.dates_off || undefined,
  };
  return normalizeLayout(next);
}

export const PRESETS: Preset[] = [
  {
    id: "classic",
    name: tr("Classic"),
    description: tr("Top items, then Pinned, Projects and Chats"),
    apply: (l) =>
      arrange(l, { order: ["patrols", "tasks", "shortcuts"], sections: ["pinned", "projects", "chats", "team"] }),
  },
  {
    id: "chat",
    name: tr("Just chat"),
    description: tr("Your chats and pins, nothing else"),
    apply: (l) =>
      arrange(l, {
        order: ["shortcuts", "patrols", "tasks"],
        hide: ["patrols", "tasks"],
        sections: ["pinned", "chats", "projects", "team"],
        hideSections: ["projects", "team"],
      }),
  },
  {
    id: "dashboards",
    name: tr("Dashboards first"),
    description: tr("Pinned boards up top, opened live"),
    apply: (l) =>
      arrange(l, {
        order: ["shortcuts", "patrols", "tasks"],
        sections: ["pinned", "chats", "projects", "team"],
      }),
  },
  {
    id: "automation",
    name: tr("Automation"),
    description: tr("Background tasks and patrols lead"),
    apply: (l) =>
      arrange(l, {
        order: ["tasks", "patrols", "shortcuts"],
        sections: ["pinned", "projects", "chats", "team"],
      }),
  },
  {
    id: "team",
    name: tr("Team room"),
    description: tr("Teammates' chats in their own list, first"),
    apply: (l) =>
      arrange(l, {
        order: ["patrols", "tasks", "shortcuts"],
        sections: ["team", "pinned", "chats", "projects"],
        separateLists: true,
      }),
  },
];

/** presetMatching: the preset this layout is (ignoring shortcuts' details), if any. */
export function presetMatching(layout: SidebarLayout): PresetId | null {
  for (const p of PRESETS) {
    if (sameLayout(p.apply(layout), layout)) return p.id;
  }
  return null;
}

// ── Display names. ─────────────────────────────────────────────────────────

export function builtinName(id: BuiltinId): string {
  switch (id) {
    case "chat":
      return tr("Chat");
    case "patrols":
      return tr("Patrols");
    case "tasks":
      return tr("Background tasks");
  }
}

export function builtinSubtitle(id: BuiltinId): string {
  switch (id) {
    case "chat":
      return tr("Talk it through");
    case "patrols":
      return tr("Chats that check in on a schedule");
    case "tasks":
      return tr("Runs without a chat");
  }
}

export function sectionName(id: SectionId): string {
  switch (id) {
    case "pinned":
      return tr("Pinned");
    case "projects":
      return tr("Projects");
    case "chats":
      return tr("Chats");
    case "team":
      return tr("Team chats");
  }
}

export function pageName(ref: string): string {
  switch (ref) {
    case "analytics":
      return tr("Analytics");
    case "help":
      return tr("Guides");
    case "settings":
      return tr("Settings");
    default:
      return ref;
  }
}

export function pageHref(ref: string): string {
  switch (ref) {
    case "analytics":
      return "/analytics";
    case "help":
      return "/help";
    default:
      return "/settings";
  }
}

/** The kind's default icon (sprite), used when the item names none. */
export function defaultIcon(item: Pick<SidebarItem, "id" | "kind" | "ref" | "view">, isPatrol = false): string {
  switch (item.kind) {
    case "builtin":
      return item.id === "chat" ? "message" : item.id === "patrols" ? "sparkles" : "grid";
    case "chat":
      return item.view === "dashboard" ? "bar-chart" : isPatrol ? "activity" : "message";
    case "project":
      return "briefcase";
    case "task":
      return "zap";
    case "page":
      return item.ref === "analytics" ? "bar-chart" : item.ref === "help" ? "book" : "settings";
  }
}

/** The kind as a person would say it, for the editor and the collapsed tooltip. */
export function kindName(item: Pick<SidebarItem, "kind" | "view" | "ref">, isPatrol = false): string {
  switch (item.kind) {
    case "builtin":
      return tr("Built in");
    case "chat":
      return item.view === "dashboard" ? tr("Dashboard") : isPatrol ? tr("Patrol") : tr("Chat");
    case "project":
      return tr("Project");
    case "task":
      return tr("Background task");
    case "page":
      return tr("Page");
  }
}

/** The name the rail shows for an item. `liveTitle` is the target's current name, when known. */
export function itemName(item: SidebarItem, liveTitle?: string): string {
  if (item.label) return item.label;
  if (item.kind === "builtin" && isBuiltinId(item.id)) return builtinName(item.id);
  if (item.kind === "page") return pageName(item.ref ?? "");
  return liveTitle || item.title || tr("Untitled");
}
