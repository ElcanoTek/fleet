import { t as tr } from "@/app/lib/i18n";
import { useEffect, useId, useMemo, useRef, useState, type DragEvent } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import { Segmented } from "@/app/shared/ui/Segmented";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";
import {
  ICON_CHOICES,
  MAX_ITEMS,
  MAX_LABEL,
  MAX_SECTION_LABEL,
  PRESETS,
  addShortcut,
  builtinName,
  defaultIcon,
  findShortcut,
  isBuiltinId,
  isEmojiIcon,
  itemName,
  kindName,
  moveItem,
  moveItemTo,
  moveSection,
  pageName,
  presetMatching,
  removeItem,
  sectionName,
  setHome,
  updateItem,
  updateSection,
  type SectionId,
  type SidebarItem,
  type SidebarLayout,
} from "./layout";
import { HomeIcon, ItemIcon } from "./glyphs";
import { loadPickerEntries, type PickerEntry } from "./pickerData";

// SidebarEditor — the body of the Customize panel (SidebarCustomizer, for a
// person's own rail) and of Settings → Admin → Sidebar (the workspace
// default). It edits a layout through pure functions (layout.ts) and hands
// every change up at once; the caller decides whether that is a live preview
// saved to the account or a draft an admin saves for everyone.

const ROW_DRAG_MIME = "application/x-fleet-sidebar-item";

const ICON_BUTTON =
  "inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:pointer-events-none disabled:opacity-35 any-pointer-coarse:size-9";

const HEADING = "text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]";

export type EditorMode = "user" | "workspace";

export function SidebarEditor({
  layout,
  onChange,
  mode = "user",
  isAdmin = false,
}: {
  layout: SidebarLayout;
  onChange: (next: SidebarLayout) => void;
  mode?: EditorMode;
  /** Admins may pin Analytics; it is an admin page. */
  isAdmin?: boolean;
}) {
  const [titles, setTitles] = useState<Map<string, string>>(new Map());
  const [patrolIds, setPatrolIds] = useState<Set<string>>(new Set());
  const matched = presetMatching(layout);
  const compactId = useId();

  // Live names for the editor's rows come from the same lists the picker
  // reads; until they land a row shows the name saved with it.
  const [entries, setEntries] = useState<PickerEntry[] | null>(null);
  useEffect(() => {
    let live = true;
    void loadPickerEntries().then((list) => {
      if (!live) return;
      setEntries(list);
      setTitles(new Map(list.map((e) => [`${e.kind}:${e.ref}`, e.title ?? ""])));
      setPatrolIds(new Set(list.filter((e) => e.group === "patrols").map((e) => e.ref)));
    });
    return () => {
      live = false;
    };
  }, []);

  const liveTitle = (item: SidebarItem) => titles.get(`${item.kind}:${item.ref}`) || undefined;

  return (
    <div className="grid gap-6">
      <section aria-labelledby="sidebar-presets-heading" className="grid gap-2.5">
        <div className="flex items-baseline justify-between gap-2">
          <h3 id="sidebar-presets-heading" className={HEADING}>
            {tr("Start from")}
          </h3>
          <span className="text-[0.7rem] text-[var(--color-text-muted)]">{tr("Your shortcuts stay")}</span>
        </div>
        <div className="grid grid-cols-2 gap-2" role="group" aria-label={tr("Layouts")}>
          {PRESETS.map((p) => {
            const preview = p.apply(layout);
            const selected = matched === p.id;
            return (
              <button
                key={p.id}
                type="button"
                aria-pressed={selected}
                data-testid={`sidebar-preset-${p.id}`}
                onClick={() => onChange(p.apply(layout))}
                className={cn(
                  "group/preset grid content-start gap-1.5 rounded-[var(--radius-md)] border p-2 text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                  selected
                    ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)]"
                    : "border-[var(--color-border)] hover:border-[var(--color-border-strong)] hover:bg-[var(--color-overlay-soft)]",
                )}
              >
                <MiniRail layout={preview} liveTitle={liveTitle} />
                <span className="flex items-center gap-1.5">
                  <span className="text-[0.8rem] font-semibold text-[var(--color-text-primary)]">{p.name}</span>
                  {selected ? <Icon name="check" className="size-3.5 text-[var(--color-accent)]" /> : null}
                </span>
                <span className="line-clamp-2 text-[0.68rem] leading-snug text-[var(--color-text-muted)]" title={p.description}>
                  {p.description}
                </span>
              </button>
            );
          })}
        </div>
      </section>

      <ItemsSection
        layout={layout}
        onChange={onChange}
        liveTitle={liveTitle}
        patrolIds={patrolIds}
        entries={entries}
        mode={mode}
        isAdmin={isAdmin}
      />

      <SectionsSection layout={layout} onChange={onChange} />

      <section aria-labelledby="sidebar-display-heading" className="grid gap-2">
        <h3 id="sidebar-display-heading" className={HEADING}>
          {tr("Display")}
        </h3>
        <div className="flex items-start justify-between gap-3 rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-2.5">
          <span className="grid gap-0.5">
            <label htmlFor={compactId} className="text-[0.84rem] font-medium text-[var(--color-text-primary)]">
              {tr("Compact")}
            </label>
            <span className="text-[0.72rem] text-[var(--color-text-muted)]">
              {tr("One line per item: hide the descriptions under Chat, Patrols and the rest.")}
            </span>
          </span>
          <Switch
            id={compactId}
            checked={Boolean(layout.compact)}
            onCheckedChange={(on) => onChange({ ...layout, compact: on || undefined })}
          />
        </div>
      </section>
    </div>
  );
}

// ── The mini rail drawn on each preset card ────────────────────────────────

function MiniRail({ layout, liveTitle }: { layout: SidebarLayout; liveTitle: (i: SidebarItem) => string | undefined }) {
  const items = layout.items.filter((i) => !i.hidden).slice(0, 3);
  const sections = layout.sections.filter((s) => !s.hidden).slice(0, 1);
  return (
    <span
      aria-hidden="true"
      className="grid gap-[3px] rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-bg)] bg-[image:var(--sidebar-surface)] p-1.5"
    >
      {items.map((i) => (
        <span key={i.id} className="flex items-center gap-1">
          <span className="flex size-3.5 shrink-0 items-center justify-center rounded-[0.2rem] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]">
            <ItemIcon icon={i.icon || defaultIcon(i)} className="size-2.5" />
          </span>
          <span className="min-w-0 flex-1 truncate text-[0.58rem] leading-none text-[var(--color-text-secondary)]">
            {itemName(i, liveTitle(i))}
          </span>
          {layout.home === i.id ? <HomeIcon className="size-2 shrink-0 text-[var(--color-accent)]" /> : null}
        </span>
      ))}
      <span className="my-[2px] h-px bg-[var(--color-border)]" />
      {sections.map((s) => (
        <span key={s.id} className="flex items-center gap-1">
          <span className="h-[3px] w-6 rounded-full bg-[var(--color-text-muted)] opacity-50" />
          <span className="text-[0.52rem] uppercase leading-none tracking-[0.08em] text-[var(--color-text-muted)]">
            {s.label || sectionName(s.id)}
          </span>
        </span>
      ))}
    </span>
  );
}

// ── Items: destinations and shortcuts ──────────────────────────────────────

function ItemsSection({
  layout,
  onChange,
  liveTitle,
  patrolIds,
  entries,
  mode,
  isAdmin,
}: {
  layout: SidebarLayout;
  onChange: (next: SidebarLayout) => void;
  liveTitle: (i: SidebarItem) => string | undefined;
  patrolIds: Set<string>;
  entries: PickerEntry[] | null;
  mode: EditorMode;
  isAdmin: boolean;
}) {
  const [dragId, setDragId] = useState<string | null>(null);
  const [dropIndex, setDropIndex] = useState<number | null>(null);
  const [announce, setAnnounce] = useState("");
  const listRef = useRef<HTMLOListElement | null>(null);
  const full = layout.items.length >= MAX_ITEMS;
  // In the workspace editor, a pin to something the team cannot see is
  // flagged: it shows only to the people who can open it (often just the
  // admin), never to the rest.
  const privateTargets = useMemo(
    () =>
      new Set(
        mode === "workspace"
          ? (entries ?? []).filter((e) => !e.shared && e.kind !== "page").map((e) => `${e.kind}:${e.ref}`)
          : [],
      ),
    [entries, mode],
  );
  const homeItem = layout.items.find((i) => i.id === layout.home);

  const move = (item: SidebarItem, delta: number, name: string) => {
    const next = moveItem(layout, item.id, delta);
    onChange(next);
    const pos = next.items.findIndex((i) => i.id === item.id) + 1;
    setAnnounce(tr("{0} moved to position {1} of {2}.", [name, pos, next.items.length]));
    // Keep the focus on the moved row's arrow (or its other arrow at an end).
    requestAnimationFrame(() => {
      const row = listRef.current?.querySelector<HTMLElement>(`[data-item-id="${CSS.escape(item.id)}"]`);
      const preferred = row?.querySelector<HTMLButtonElement>(delta < 0 ? "[data-move=up]" : "[data-move=down]");
      const other = row?.querySelector<HTMLButtonElement>(delta < 0 ? "[data-move=down]" : "[data-move=up]");
      (preferred && !preferred.disabled ? preferred : other)?.focus();
    });
  };

  const onDrop = (e: DragEvent, index: number) => {
    const id = e.dataTransfer.getData(ROW_DRAG_MIME);
    setDragId(null);
    setDropIndex(null);
    if (!id) return;
    e.preventDefault();
    const from = layout.items.findIndex((i) => i.id === id);
    onChange(moveItemTo(layout, id, from < index ? index - 1 : index));
  };

  return (
    <section aria-labelledby="sidebar-items-heading" className="grid gap-2">
      <div className="flex items-center justify-between gap-2">
        <h3 id="sidebar-items-heading" className={HEADING}>
          {tr("In the sidebar")}
        </h3>
        <AddShortcut layout={layout} onChange={onChange} entries={entries} disabled={full} mode={mode} isAdmin={isAdmin} />
      </div>
      <p className="text-[0.72rem] leading-snug text-[var(--color-text-muted)]">
        {mode === "workspace"
          ? tr("Drag to reorder. Rename anything, hide what your team doesn't use, and pick a Home for everyone. A pinned chat shows only to people who can open it.")
          : tr("Drag to reorder. Rename anything, hide what you don't use, and pick a Home: the place fleet opens to.")}
      </p>
      <span className="sr-only" aria-live="polite">
        {announce}
      </span>
      <ol ref={listRef} className="grid gap-1" data-testid="sidebar-editor-items">
        {layout.items.map((item, index) => {
          const isPatrol = item.kind === "chat" && patrolIds.has(item.ref ?? "");
          const name = itemName(item, liveTitle(item));
          return (
            <li key={item.id} data-item-id={item.id} data-testid="sidebar-editor-item">
              <div
              onDragOver={(e) => {
                if (!dragId) return;
                e.preventDefault();
                setDropIndex(index);
              }}
              onDrop={(e) => onDrop(e, index)}
              className={cn(
                "relative rounded-[var(--radius-md)] border bg-[var(--color-surface-1)] transition",
                item.hidden ? "border-dashed border-[var(--color-border-strong)] bg-transparent" : "border-[var(--color-border)]",
                dragId === item.id ? "opacity-40" : "",
                dropIndex === index && dragId !== item.id
                  ? "before:absolute before:-top-[3px] before:left-2 before:right-2 before:h-[2px] before:rounded-full before:bg-[var(--color-accent)]"
                  : "",
              )}
            >
              <ItemRow
                item={item}
                name={name}
                isPatrol={isPatrol}
                isHome={layout.home === item.id}
                notShared={privateTargets.has(`${item.kind}:${item.ref}`)}
                first={index === 0}
                last={index === layout.items.length - 1}
                onDragStart={(e) => {
                  e.dataTransfer.setData(ROW_DRAG_MIME, item.id);
                  e.dataTransfer.effectAllowed = "move";
                  setDragId(item.id);
                }}
                onDragEnd={() => {
                  setDragId(null);
                  setDropIndex(null);
                }}
                onMove={(d) => move(item, d, name)}
                onPatch={(patch) => onChange(updateItem(layout, item.id, patch))}
                onHome={() => onChange(setHome(layout, layout.home === item.id ? "" : item.id))}
                onRemove={() => onChange(removeItem(layout, item.id))}
              />
              </div>
            </li>
          );
        })}
        <li aria-hidden="true">
          <div
            className={cn("h-3 rounded", dropIndex === layout.items.length ? "bg-[color-mix(in_srgb,var(--color-accent)_25%,transparent)]" : "")}
            onDragOver={(e) => {
              if (!dragId) return;
              e.preventDefault();
              setDropIndex(layout.items.length);
            }}
            onDrop={(e) => onDrop(e, layout.items.length)}
          />
        </li>
      </ol>
      <p className="flex items-center gap-1.5 text-[0.72rem] text-[var(--color-text-muted)]" data-testid="sidebar-home-summary">
        <HomeIcon className="size-3.5 shrink-0 text-[var(--color-accent)]" />
        {homeItem
          ? tr("Home: {0}. Opening fleet takes you there.", [itemName(homeItem, liveTitle(homeItem))])
          : tr("No Home set: fleet opens your most recent chat.")}
      </p>
    </section>
  );
}

function ItemRow({
  item,
  name,
  isPatrol,
  isHome,
  notShared,
  first,
  last,
  onDragStart,
  onDragEnd,
  onMove,
  onPatch,
  onHome,
  onRemove,
}: {
  item: SidebarItem;
  name: string;
  isPatrol: boolean;
  isHome: boolean;
  /** Workspace editor only: the team cannot see this target. */
  notShared?: boolean;
  first: boolean;
  last: boolean;
  onDragStart: (e: DragEvent) => void;
  onDragEnd: () => void;
  onMove: (delta: number) => void;
  onPatch: (patch: Partial<SidebarItem>) => void;
  onHome: () => void;
  onRemove: () => void;
}) {
  const builtin = item.kind === "builtin";
  const placeholder =
    builtin && isBuiltinId(item.id)
      ? builtinName(item.id)
      : item.kind === "page"
        ? pageName(item.ref ?? "")
        : itemName({ ...item, label: undefined });
  const canHome = item.id !== "patrols";
  return (
    <div className="grid gap-1.5 px-1.5 py-1.5">
      <div className="flex items-center gap-1">
        <span
          draggable
          onDragStart={onDragStart}
          onDragEnd={onDragEnd}
          title={tr("Drag to reorder")}
          className="flex h-7 w-4 shrink-0 cursor-grab items-center justify-center text-[var(--color-text-muted)] active:cursor-grabbing any-pointer-coarse:hidden"
        >
          <Icon name="selector" className="size-3.5" />
        </span>
        <IconPicker item={item} isPatrol={isPatrol} name={name} onPick={(icon) => onPatch({ icon })} />
        <NameInput
          value={item.label ?? ""}
          maxLength={MAX_LABEL}
          placeholder={placeholder}
          ariaLabel={tr("Name for {0}", [placeholder])}
          testId="sidebar-item-name"
          onCommit={(label) => onPatch({ label })}
          className="h-7 min-w-0 flex-1 rounded-md border border-transparent bg-transparent px-1.5 text-[0.84rem] font-medium text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-primary)] placeholder:opacity-100 hover:border-[var(--color-border)] focus-visible:border-[var(--color-border-strong)] focus-visible:bg-[var(--color-overlay-soft)] focus-visible:placeholder:text-[var(--color-text-muted)] any-pointer-coarse:h-9"
        />
        <button
          type="button"
          className={cn(ICON_BUTTON, isHome ? "bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]" : "")}
          aria-pressed={isHome}
          aria-label={tr("Make {0} your Home", [name])}
          title={isHome ? tr("Home — fleet opens here") : tr("Make this your Home")}
          disabled={!canHome || Boolean(item.hidden)}
          data-testid="sidebar-item-home"
          onClick={onHome}
        >
          <HomeIcon className="size-3.5" />
        </button>
        <Switch
          checked={!item.hidden}
          onCheckedChange={(on) => onPatch({ hidden: !on })}
          aria-label={tr("Show {0} in the sidebar", [name])}
          title={item.hidden ? tr("Hidden — switch on to show") : tr("Shown — switch off to hide")}
          data-testid="sidebar-item-hide"
          className="shrink-0 scale-90"
        />
        <span className="flex shrink-0 flex-col">
          <button
            type="button"
            data-move="up"
            className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
            aria-label={tr("Move {0} up", [name])}
            disabled={first}
            onClick={() => onMove(-1)}
          >
            <Icon name="chevron-down" className="size-3 rotate-180" />
          </button>
          <button
            type="button"
            data-move="down"
            className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
            aria-label={tr("Move {0} down", [name])}
            disabled={last}
            onClick={() => onMove(1)}
          >
            <Icon name="chevron-down" className="size-3" />
          </button>
        </span>
        {builtin ? (
          <span className="size-7 shrink-0 any-pointer-coarse:size-9" aria-hidden="true" />
        ) : (
          <button
            type="button"
            className={cn(ICON_BUTTON, "hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)]")}
            aria-label={tr("Remove {0} from the sidebar", [name])}
            title={tr("Remove from the sidebar")}
            data-testid="sidebar-item-remove"
            onClick={onRemove}
          >
            <Icon name="close" className="size-3.5" />
          </button>
        )}
      </div>
      <div className="flex min-h-5 flex-wrap items-center gap-2 pl-[3.25rem] any-pointer-coarse:pl-11">
        <span className="rounded-full bg-[var(--color-overlay-soft)] px-1.5 py-px text-[0.64rem] font-medium text-[var(--color-text-muted)]">
          {kindName(item, isPatrol)}
        </span>
        {notShared ? (
          <span
            data-testid="sidebar-item-not-shared"
            className="rounded-full bg-[var(--color-status-warning-bg)] px-1.5 py-px text-[0.64rem] font-medium text-[var(--color-status-warning-fg)]"
            title={tr("Not shared with your team: only people who can open it will see this pin. Share it first, or remove it.")}
          >
            {tr("Not shared with the team")}
          </span>
        ) : null}
        {item.hidden ? (
          <span className="rounded-full border border-dashed border-[var(--color-border-strong)] px-1.5 py-px text-[0.64rem] font-medium text-[var(--color-text-secondary)]">
            {tr("Hidden")}
          </span>
        ) : null}
        {item.label && !builtin && item.kind !== "page" ? (
          <span className="min-w-0 truncate text-[0.68rem] text-[var(--color-text-muted)]">{itemName({ ...item, label: undefined })}</span>
        ) : null}
        {item.kind === "chat" ? (
          <span className="ml-auto origin-right scale-90">
            <Segmented
              label={tr("Opens as")}
              value={item.view === "dashboard" ? "dashboard" : "chat"}
              options={[
                { value: "chat", label: tr("Chat") },
                { value: "dashboard", label: tr("Dashboard") },
              ]}
              onChange={(v) => onPatch({ view: v === "dashboard" ? "dashboard" : undefined })}
            />
          </span>
        ) : null}
      </div>
    </div>
  );
}

// ── Icon picker: the sprite choices, an emoji, or the default ───────────────

function IconPicker({
  item,
  isPatrol,
  name,
  onPick,
}: {
  item: SidebarItem;
  isPatrol: boolean;
  name: string;
  onPick: (icon: string | undefined) => void;
}) {
  const [open, setOpen] = useState(false);
  const [emoji, setEmoji] = useState("");
  const current = item.icon || defaultIcon(item, isPatrol);
  const emojiOk = emoji === "" || isEmojiIcon(emoji.trim());
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          aria-label={tr("Icon for {0}", [name])}
          data-testid="sidebar-item-icon"
          className="flex size-7 shrink-0 items-center justify-center rounded-[0.55rem] bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] text-[var(--color-accent)] transition hover:bg-[color-mix(in_srgb,var(--color-accent)_22%,transparent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:size-9"
        >
          <ItemIcon icon={current} className="size-[0.95rem]" />
        </button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[17rem] p-2.5">
        <div className="grid grid-cols-8 gap-1" role="group" aria-label={tr("Icons")}>
          {ICON_CHOICES.map((name) => (
            <button
              key={name}
              type="button"
              aria-label={name}
              aria-pressed={item.icon === name}
              onClick={() => {
                onPick(name);
                setOpen(false);
              }}
              className={cn(
                "flex size-7 items-center justify-center rounded-md text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                item.icon === name ? "bg-[color-mix(in_srgb,var(--color-accent)_18%,transparent)] text-[var(--color-accent)]" : "",
              )}
            >
              <Icon name={name} className="size-4" />
            </button>
          ))}
        </div>
        <form
          className="mt-2.5 flex items-center gap-1.5 border-t border-[var(--color-border)] pt-2.5"
          onSubmit={(e) => {
            e.preventDefault();
            const v = emoji.trim();
            if (!v || !isEmojiIcon(v)) return;
            onPick(v);
            setEmoji("");
            setOpen(false);
          }}
        >
          <input
            value={emoji}
            onChange={(e) => setEmoji(e.target.value)}
            placeholder={tr("Or an emoji 🚀")}
            aria-label={tr("Emoji icon")}
            aria-invalid={!emojiOk}
            className="h-8 min-w-0 flex-1 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 text-[0.84rem] text-[var(--color-text-primary)] outline-none focus-visible:border-[var(--color-border-strong)] aria-invalid:border-[var(--color-danger)]"
          />
          <button
            type="submit"
            disabled={!emoji.trim() || !emojiOk}
            className="h-8 rounded-md border border-[var(--color-border-strong)] px-2 text-[0.75rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] disabled:opacity-50"
          >
            {tr("Use")}
          </button>
          {item.icon ? (
            <button
              type="button"
              onClick={() => {
                onPick(undefined);
                setOpen(false);
              }}
              className="h-8 rounded-md px-2 text-[0.75rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
            >
              {tr("Default")}
            </button>
          ) : null}
        </form>
      </PopoverContent>
    </Popover>
  );
}

// ── Add to sidebar: a searchable list of everything that can be pinned ──────

const GROUPS: { id: PickerEntry["group"]; heading: () => string }[] = [
  { id: "patrols", heading: () => tr("Patrols") },
  { id: "chats", heading: () => tr("Your chats") },
  { id: "team", heading: () => tr("Team chats") },
  { id: "projects", heading: () => tr("Projects") },
  { id: "tasks", heading: () => tr("Background tasks") },
  { id: "pages", heading: () => tr("Pages") },
];

function AddShortcut({
  layout,
  onChange,
  entries,
  disabled,
  mode,
  isAdmin,
}: {
  layout: SidebarLayout;
  onChange: (next: SidebarLayout) => void;
  entries: PickerEntry[] | null;
  disabled: boolean;
  mode: EditorMode;
  isAdmin: boolean;
}) {
  const [open, setOpen] = useState(false);
  const shown = useMemo(
    () =>
      (entries ?? []).filter(
        (e) =>
          !(e.kind === "page" && e.ref === "analytics" && !isAdmin) &&
          // The workspace sidebar offers only what the team can see: a chat
          // or project shared with it, or a page (never a task: only its
          // owner and admins can open one, and the server refuses it). A
          // person's own sidebar offers everything they can open.
          !(mode === "workspace" && !e.shared),
      ),
    [entries, isAdmin, mode],
  );
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <button
          type="button"
          disabled={disabled}
          data-testid="sidebar-add-shortcut"
          title={disabled ? tr("The sidebar is full ({0} items).", [MAX_ITEMS]) : undefined}
          className="inline-flex h-7 items-center gap-1 rounded-md border border-[var(--color-border-strong)] px-2 text-[0.75rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50 any-pointer-coarse:h-9"
        >
          <Icon name="plus" className="size-3.5" />
          {tr("Add")}
        </button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[21rem] p-0">
        <Command
          filter={(value, search) => (value.toLowerCase().includes(search.toLowerCase()) ? 1 : 0)}
          label={tr("Add to the sidebar")}
        >
          <CommandInput placeholder={tr("Find a chat, dashboard, project, task…")} />
          <CommandList className="max-h-[22rem]">
            {entries === null ? (
              <p className="px-3 py-6 text-center text-[0.8rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
            ) : (
              <CommandEmpty>{tr("Nothing matches.")}</CommandEmpty>
            )}
            {GROUPS.map((g) => {
              const list = shown.filter((e) => e.group === g.id);
              if (list.length === 0) return null;
              return (
                <CommandGroup key={g.id} heading={g.heading()}>
                  {list.map((e) => {
                    const pinned = findShortcut(layout, e.kind, e.ref);
                    const title = e.kind === "page" ? pageName(e.ref) : e.title || tr("Untitled");
                    return (
                      <CommandItem
                        key={`${e.kind}:${e.ref}`}
                        value={`${title} ${e.detail ?? ""} ${e.kind}:${e.ref}`}
                        data-testid="sidebar-picker-item"
                        onSelect={() => {
                          onChange(addShortcut(layout, { kind: e.kind, ref: e.ref, title: e.title, icon: e.icon }));
                          setOpen(false);
                        }}
                      >
                        <span className="flex size-6 shrink-0 items-center justify-center rounded-md bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] text-[var(--color-accent)]">
                          <Icon
                            name={e.icon || defaultIcon({ id: "", kind: e.kind, ref: e.ref })}
                            className="size-3.5 text-[var(--color-accent)]"
                          />
                        </span>
                        <span className="grid min-w-0 flex-1">
                          <span className="truncate text-[0.82rem]">{title}</span>
                          {e.detail ? (
                            <span className="truncate text-[0.68rem] text-[var(--color-text-muted)]">{e.detail}</span>
                          ) : null}
                        </span>
                        {pinned && !pinned.hidden ? (
                          <Icon name="check" className="size-3.5 text-[var(--color-accent)]" />
                        ) : null}
                      </CommandItem>
                    );
                  })}
                </CommandGroup>
              );
            })}
          </CommandList>
          {mode === "workspace" ? (
            <p className="border-t border-[var(--color-border)] px-3 py-2 text-[0.68rem] leading-snug text-[var(--color-text-muted)]">
              {tr("Only chats and projects shared with your team are listed. Pin a shared or multiplayer chat to give your whole team one place to meet.")}
            </p>
          ) : null}
        </Command>
      </PopoverContent>
    </Popover>
  );
}

// ── The chat page's sections ───────────────────────────────────────────────

function SectionsSection({ layout, onChange }: { layout: SidebarLayout; onChange: (next: SidebarLayout) => void }) {
  return (
    <section aria-labelledby="sidebar-sections-heading" className="grid gap-2">
      <h3 id="sidebar-sections-heading" className={HEADING}>
        {tr("On the Chat page")}
      </h3>
      <p className="text-[0.72rem] leading-snug text-[var(--color-text-muted)]">
        {tr("The lists under the search box. Order them, rename them, or hide one you never open.")}
      </p>
      <ol className="grid gap-1" data-testid="sidebar-editor-sections">
        {layout.sections.map((s, index) => {
          const name = s.label || sectionName(s.id);
          return (
            <li
              key={s.id}
              className={cn(
                "flex items-center gap-1 rounded-[var(--radius-md)] border bg-[var(--color-surface-1)] px-1.5 py-1.5",
                s.hidden ? "border-dashed border-[var(--color-border-strong)] bg-transparent" : "border-[var(--color-border)]",
              )}
            >
              <span className="flex size-7 shrink-0 items-center justify-center text-[var(--color-text-muted)]">
                <Icon name={sectionIcon(s.id)} className="size-3.5" />
              </span>
              <NameInput
                value={s.label ?? ""}
                maxLength={MAX_SECTION_LABEL}
                placeholder={sectionName(s.id)}
                ariaLabel={tr("Name for {0}", [sectionName(s.id)])}
                onCommit={(label) => onChange(updateSection(layout, s.id, { label }))}
                className="h-7 min-w-0 flex-1 rounded-md border border-transparent bg-transparent px-1.5 text-[0.84rem] font-medium text-[var(--color-text-primary)] outline-none transition placeholder:text-[var(--color-text-primary)] hover:border-[var(--color-border)] focus-visible:border-[var(--color-border-strong)] focus-visible:bg-[var(--color-overlay-soft)] focus-visible:placeholder:text-[var(--color-text-muted)] any-pointer-coarse:h-9"
              />
              <button
                type="button"
                className={ICON_BUTTON}
                aria-pressed={Boolean(s.collapsed)}
                aria-label={tr("Start {0} folded", [name])}
                title={s.collapsed ? tr("Starts folded") : tr("Starts open")}
                onClick={() => onChange(updateSection(layout, s.id, { collapsed: !s.collapsed }))}
              >
                <Icon name={s.collapsed ? "chevron-right" : "chevron-down"} className="size-3.5" />
              </button>
              <Switch
                checked={!s.hidden}
                onCheckedChange={(on) => onChange(updateSection(layout, s.id, { hidden: !on }))}
                aria-label={tr("Show {0} on the Chat page", [name])}
                title={s.hidden ? tr("Hidden — switch on to show") : tr("Shown — switch off to hide")}
                className="shrink-0 scale-90"
              />
              <span className="flex shrink-0 flex-col">
                <button
                  type="button"
                  className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                  aria-label={tr("Move {0} up", [name])}
                  disabled={index === 0}
                  onClick={() => onChange(moveSection(layout, s.id as SectionId, -1))}
                >
                  <Icon name="chevron-down" className="size-3 rotate-180" />
                </button>
                <button
                  type="button"
                  className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                  aria-label={tr("Move {0} down", [name])}
                  disabled={index === layout.sections.length - 1}
                  onClick={() => onChange(moveSection(layout, s.id as SectionId, 1))}
                >
                  <Icon name="chevron-down" className="size-3" />
                </button>
              </span>
            </li>
          );
        })}
      </ol>
    </section>
  );
}

function sectionIcon(id: SectionId): string {
  return id === "projects" ? "folder" : id === "team" ? "persona" : "message";
}

// NameInput keeps what is typed while it has focus (a trailing space, a name
// being cleared and retyped) and hands each change up at once, so the rail
// shows the new name as it is typed; the layout stores the trimmed name.
function NameInput({
  value,
  onCommit,
  ariaLabel,
  testId,
  ...rest
}: {
  value: string;
  onCommit: (next: string) => void;
  ariaLabel: string;
  testId?: string;
  maxLength: number;
  placeholder: string;
  className: string;
}) {
  const [draft, setDraft] = useState(value);
  const [focused, setFocused] = useState(false);
  return (
    <input
      type="text"
      {...rest}
      value={focused ? draft : value}
      aria-label={ariaLabel}
      data-testid={testId}
      onFocus={() => {
        setDraft(value);
        setFocused(true);
      }}
      onBlur={() => setFocused(false)}
      onKeyDown={(e) => {
        if (e.key === "Enter") e.currentTarget.blur();
      }}
      onChange={(e) => {
        setDraft(e.target.value);
        onCommit(e.target.value);
      }}
    />
  );
}
