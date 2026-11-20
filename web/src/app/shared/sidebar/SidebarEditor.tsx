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
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
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
  setStartPage,
  setTeamChatsMode,
  isStartPage,
  teamChatsMode,
  updateItem,
  updateSection,
  type SectionId,
  type SidebarItem,
  type SidebarLayout,
  type TeamChatsMode,
} from "./layout";
import { HomeIcon, ItemIcon } from "./glyphs";
import { loadPickerEntries, type PickerEntry } from "./pickerData";
import { useSidebarTargets, type SidebarPins } from "./targets";

// SidebarEditor — the body of the Customize panel (SidebarCustomizer, for a
// person's own rail) and of Settings → Admin → Sidebar (the workspace
// default). It edits a layout through pure functions (layout.ts) and hands
// every change up at once; the caller decides whether that is a live preview
// saved to the account or a draft an admin saves for everyone.

const ROW_DRAG_MIME = "application/x-fleet-sidebar-item";

const ICON_BUTTON =
  "inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:pointer-events-none disabled:opacity-35 any-pointer-coarse:size-9";

const HEADING = "text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]";

const ADD_BUTTON =
  "inline-flex h-7 items-center gap-1 rounded-md border border-[var(--color-border-strong)] px-2 text-[0.75rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50 any-pointer-coarse:h-9";

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
  // The sidebar's own Pinned section, when this is a person's own sidebar on
  // a page that has one (pins are personal: the workspace default has none).
  const pins = useSidebarTargets()?.pins;

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
    <div className="grid gap-7">
      <StartPageSection
        layout={layout}
        onChange={onChange}
        liveTitle={liveTitle}
        patrolIds={patrolIds}
        pins={mode === "user" ? pins : undefined}
      />

      <section aria-labelledby="sidebar-presets-heading" className="grid gap-2.5">
        <div className="flex items-baseline justify-between gap-2">
          <h3 id="sidebar-presets-heading" className={HEADING}>
            {tr("Start from")}
          </h3>
          <span className="text-[0.72rem] text-[var(--color-text-muted)]">{tr("Pins and start page stay")}</span>
        </div>
        <div className="grid grid-cols-2 gap-2" role="group" aria-label={tr("Layouts")}>
          {PRESETS.map((p) => {
            const selected = matched === p.id;
            return (
              <button
                key={p.id}
                type="button"
                aria-pressed={selected}
                data-testid={`sidebar-preset-${p.id}`}
                onClick={() => onChange(p.apply(layout))}
                className={cn(
                  "grid content-start gap-1 rounded-[var(--radius-md)] border px-3 py-2.5 text-left transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                  selected
                    ? "border-[var(--color-selected-border)] bg-[var(--color-selected-bg)]"
                    : "border-[var(--color-border)] hover:border-[var(--color-border-strong)] hover:bg-[var(--color-overlay-soft)]",
                )}
              >
                <span className="flex items-center gap-1.5">
                  <span className="min-w-0 flex-1 text-[0.84rem] font-semibold text-[var(--color-text-primary)]">{p.name}</span>
                  {selected ? <Icon name="check" className="size-3.5 shrink-0 text-[var(--color-accent)]" /> : null}
                </span>
                <span className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">{p.description}</span>
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

      {mode === "user" && pins ? <PinnedSection pins={pins} entries={entries} /> : null}

      <SectionsSection layout={layout} onChange={onChange} />

      <section aria-labelledby="sidebar-density-heading" className="grid gap-2">
        <h3 id="sidebar-density-heading" className={HEADING}>
          {tr("Density")}
        </h3>
        <DensityControl layout={layout} onChange={onChange} />
      </section>
    </div>
  );
}

/** Comfortable or Compact rows: the one density choice, used by the panel and the gear. */
export function DensityControl({ layout, onChange }: { layout: SidebarLayout; onChange: (next: SidebarLayout) => void }) {
  return (
    <div className="seg-fill" data-testid="sidebar-density">
      <Segmented<"comfortable" | "compact">
        label={tr("Density")}
        value={layout.compact ? "compact" : "comfortable"}
        options={[
          { value: "comfortable", label: tr("Comfortable") },
          { value: "compact", label: tr("Compact") },
        ]}
        onChange={(v) => onChange({ ...layout, compact: v === "compact" || undefined })}
      />
    </div>
  );
}

// ── Fleet opens to ──────────────────────────────────────────────────────────

export type StartPageOption = { key: string; label: string; detail?: string; selected: boolean; apply: (l: SidebarLayout) => SidebarLayout };

/**
 * startPageOptions: where fleet can open — the last chat (no start page), the
 * things kept at the top of the sidebar, and what is pinned. Whatever the
 * start page is now is always among them, so the control shows the truth.
 */
export function startPageOptions(
  layout: SidebarLayout,
  liveTitle: (i: SidebarItem) => string | undefined,
  patrolIds: ReadonlySet<string>,
  pins?: SidebarPins,
): StartPageOption[] {
  const options: StartPageOption[] = [
    { key: "", label: tr("The last chat you had open"), selected: !layout.home || layout.home === "chat", apply: (l) => setHome(l, "") },
  ];
  const seen = new Set<string>();
  const add = (item: SidebarItem) => {
    if (item.id === "chat" || item.id === "patrols") return;
    seen.add(`${item.kind}:${item.ref ?? item.id}`);
    options.push({
      key: item.id,
      label: itemName(item, liveTitle(item)),
      detail: item.kind === "builtin" ? undefined : kindName(item, item.kind === "chat" && patrolIds.has(item.ref ?? "")),
      selected: layout.home === item.id,
      apply: (l) => setHome(l, item.id),
    });
  };
  for (const item of layout.items) if (!item.hidden) add(item);
  for (const pin of pins?.entries ?? []) {
    if (seen.has(`${pin.kind}:${pin.ref}`)) continue;
    seen.add(`${pin.kind}:${pin.ref}`);
    options.push({
      key: pin.id,
      label: pin.title || tr("Untitled"),
      detail: pin.kind === "project" ? tr("Project") : tr("Chat"),
      selected: isStartPage(layout, pin.kind, pin.ref),
      apply: (l) => setStartPage(l, { kind: pin.kind, ref: pin.ref, title: pin.title }),
    });
  }
  // A start page that is neither at the top nor pinned (set from a row's
  // menu): listed so it can be seen, and changed.
  const home = layout.items.find((i) => i.id === layout.home);
  if (home && !options.some((o) => o.selected)) add(home);
  return options;
}

function StartPageSection({
  layout,
  onChange,
  liveTitle,
  patrolIds,
  pins,
}: {
  layout: SidebarLayout;
  onChange: (next: SidebarLayout) => void;
  liveTitle: (i: SidebarItem) => string | undefined;
  patrolIds: Set<string>;
  pins?: SidebarPins;
}) {
  const options = startPageOptions(layout, liveTitle, patrolIds, pins);
  const current = options.find((o) => o.selected) ?? options[0];
  return (
    <section aria-labelledby="sidebar-home-heading" className="grid gap-2">
      <h3 id="sidebar-home-heading" className={HEADING}>
        {tr("Fleet opens to")}
      </h3>
      <div className="flex items-center gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] py-1.5 pl-2.5 pr-1.5">
        <span aria-hidden="true" className="picker-icon-tile size-8 shrink-0">
          <HomeIcon className="size-4" />
        </span>
        <NativeSelect
          aria-labelledby="sidebar-home-heading"
          data-testid="sidebar-home-select"
          value={current.key}
          onChange={(e) => {
            const picked = options.find((o) => o.key === e.target.value);
            if (picked) onChange(picked.apply(layout));
          }}
          className="h-9 min-w-0 flex-1 border-0 bg-transparent pl-1 pr-8 text-[0.875rem] font-medium shadow-none"
        >
          {options.map((o) => (
            <NativeSelectOption key={o.key} value={o.key}>
              {o.detail ? `${o.label} · ${o.detail}` : o.label}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]" data-testid="sidebar-home-summary">
        {current.key === ""
          ? tr("No start page set: fleet opens the last chat you had open.")
          : tr("Opening fleet takes you to {0}. Any chat, dashboard or project can be the start page: ⋮ › Set as start page.", [current.label])}
      </p>
    </section>
  );
}

// ── Pinned: the sidebar's own Pinned section ───────────────────────────────

function PinnedSection({ pins, entries }: { pins: SidebarPins; entries: PickerEntry[] | null }) {
  const [open, setOpen] = useState(false);
  const [announce, setAnnounce] = useState("");
  const listRef = useRef<HTMLOListElement | null>(null);
  const ids = pins.entries.map((e) => e.id);
  const pinnedRefs = new Set(pins.entries.map((e) => `${e.kind}:${e.ref}`));
  const candidates = (entries ?? []).filter(
    (e) => (e.kind === "chat" || e.kind === "project") && e.detail !== tr("Archived") && !pinnedRefs.has(`${e.kind}:${e.ref}`),
  );
  const move = (index: number, delta: number) => {
    const to = index + delta;
    if (!pins.reorder || to < 0 || to >= ids.length) return;
    const next = [...ids];
    const [moved] = next.splice(index, 1);
    next.splice(to, 0, moved);
    pins.reorder(next);
    setAnnounce(tr("{0} moved to position {1} of {2}.", [pins.entries[index].title, to + 1, ids.length]));
    requestAnimationFrame(() => {
      const row = listRef.current?.querySelector<HTMLElement>(`[data-pin-id="${CSS.escape(moved)}"]`);
      const preferred = row?.querySelector<HTMLButtonElement>(delta < 0 ? "[data-move=up]" : "[data-move=down]");
      const other = row?.querySelector<HTMLButtonElement>(delta < 0 ? "[data-move=down]" : "[data-move=up]");
      (preferred && !preferred.disabled ? preferred : other)?.focus();
    });
  };
  return (
    <section aria-labelledby="sidebar-pinned-heading" className="grid gap-2">
      <div className="flex items-center justify-between gap-2">
        <h3 id="sidebar-pinned-heading" className={HEADING}>
          {tr("Pinned")}
        </h3>
        <Popover open={open} onOpenChange={setOpen}>
          <PopoverTrigger asChild>
            <button type="button" data-testid="sidebar-pin-add" className={ADD_BUTTON}>
              <Icon name="plus" className="size-3.5" />
              {tr("Pin")}
            </button>
          </PopoverTrigger>
          <PopoverContent align="end" className="w-[21rem] p-0">
            <Command
              filter={(value, search) => (value.toLowerCase().includes(search.toLowerCase()) ? 1 : 0)}
              label={tr("Pin a chat or a project")}
            >
              <CommandInput placeholder={tr("Find a chat or a project…")} />
              <CommandList className="max-h-[22rem]">
                {entries === null ? (
                  <p className="px-3 py-6 text-center text-[0.8rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
                ) : (
                  <CommandEmpty>{tr("Nothing matches.")}</CommandEmpty>
                )}
                {GROUPS.filter((g) => g.id !== "tasks" && g.id !== "pages").map((g) => {
                  const list = candidates.filter((e) => e.group === g.id);
                  if (list.length === 0) return null;
                  return (
                    <CommandGroup key={g.id} heading={g.heading()}>
                      {list.map((e) => (
                        <CommandItem
                          key={`${e.kind}:${e.ref}`}
                          value={`${e.title ?? ""} ${e.detail ?? ""} ${e.kind}:${e.ref}`}
                          data-testid="sidebar-pin-item"
                          onSelect={() => {
                            if (e.kind === "chat" || e.kind === "project") pins.pin(e.kind, e.ref);
                            setOpen(false);
                          }}
                        >
                          <Icon name={e.kind === "project" ? "briefcase" : "message"} className="size-4 shrink-0 text-[var(--color-text-muted)]" />
                          <span className="grid min-w-0 flex-1">
                            <span className="truncate text-[0.82rem]">{e.title || tr("Untitled")}</span>
                            {e.detail ? <span className="truncate text-[0.68rem] text-[var(--color-text-muted)]">{e.detail}</span> : null}
                          </span>
                        </CommandItem>
                      ))}
                    </CommandGroup>
                  );
                })}
              </CommandList>
            </Command>
          </PopoverContent>
        </Popover>
      </div>
      <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">
        {tr("Chats and projects, in the order you set. Use the arrows to reorder; the bin unpins. A pinned chat is kept: it is never deleted for inactivity.")}
      </p>
      <span className="sr-only" aria-live="polite">
        {announce}
      </span>
      {pins.entries.length === 0 ? (
        <p className="rounded-[var(--radius-md)] border border-dashed border-[var(--color-border-strong)] px-3 py-2.5 text-[0.8rem] text-[var(--color-text-muted)]">
          {tr("Nothing pinned yet. Pin a chat or a project to keep it above your lists.")}
        </p>
      ) : (
        <ol ref={listRef} className="grid gap-1" data-testid="sidebar-editor-pinned">
          {pins.entries.map((e, index) => (
            <li
              key={e.id}
              data-pin-id={e.id}
              className="flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2 py-1.5"
            >
              <span aria-hidden="true" className="picker-icon-tile size-7 shrink-0">
                <Icon name={e.kind === "project" ? "briefcase" : "message"} className="size-3.5" />
              </span>
              <span className="grid min-w-0 flex-1">
                <span className="truncate text-[0.84rem] font-medium text-[var(--color-text-primary)]">{e.title || tr("Untitled")}</span>
                <span className="text-[0.7rem] text-[var(--color-text-muted)]">{e.kind === "project" ? tr("Project") : tr("Chat")}</span>
              </span>
              {pins.reorder ? (
                <span className="flex shrink-0 flex-col">
                  <button
                    type="button"
                    data-move="up"
                    className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                    aria-label={tr("Move {0} up", [e.title])}
                    disabled={index === 0}
                    onClick={() => move(index, -1)}
                  >
                    <Icon name="chevron-down" className="size-3 rotate-180" />
                  </button>
                  <button
                    type="button"
                    data-move="down"
                    className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                    aria-label={tr("Move {0} down", [e.title])}
                    disabled={index === pins.entries.length - 1}
                    onClick={() => move(index, 1)}
                  >
                    <Icon name="chevron-down" className="size-3" />
                  </button>
                </span>
              ) : null}
              <button
                type="button"
                className={cn(ICON_BUTTON, "hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)]")}
                aria-label={tr("Unpin {0}", [e.title])}
                title={e.canUnpin ? tr("Unpin") : tr("Pinned by the project's owner")}
                disabled={!e.canUnpin}
                data-testid="sidebar-pin-remove"
                onClick={() => pins.unpin(e)}
              >
                <Icon name="trash" className="size-3.5" />
              </button>
            </li>
          ))}
        </ol>
      )}
    </section>
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
  // The first row: "chat" (never a row) sits at index 0 of a normalized layout.
  const firstRow = layout.items[0]?.id === "chat" ? 1 : 0;

  const move = (item: SidebarItem, delta: number, name: string) => {
    const next = moveItem(layout, item.id, delta);
    onChange(next);
    const rows = next.items.filter((i) => i.id !== "chat");
    const pos = rows.findIndex((i) => i.id === item.id) + 1;
    setAnnounce(tr("{0} moved to position {1} of {2}.", [name, pos, rows.length]));
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
          {tr("Top of the sidebar")}
        </h3>
        <AddShortcut layout={layout} onChange={onChange} entries={entries} disabled={full} mode={mode} isAdmin={isAdmin} />
      </div>
      <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">
        {mode === "workspace"
          ? tr("Always in view, above everyone's lists: Patrols, Background tasks, and anything you add. A chat or project added here shows only to people who can open it.")
          : tr("Always in view, above your lists: Patrols, Background tasks, and anything you add, like a dashboard, a chat, a project or a page.")}
      </p>
      <span className="sr-only" aria-live="polite">
        {announce}
      </span>
      <ol ref={listRef} className="grid gap-1" data-testid="sidebar-editor-items">
        {layout.items.map((item, index) => {
          // "chat" rides first in every layout and is never a row.
          if (item.id === "chat") return null;
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
                first={index <= firstRow}
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
          aria-label={tr("Make {0} your start page", [name])}
          title={isHome ? tr("Start page — fleet opens here") : tr("Set as start page")}
          disabled={!canHome || (builtin && Boolean(item.hidden))}
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
      {/* Patrols and Background tasks are what they say: one line. A kept
          shortcut says what it is and, for a chat, how it opens. */}
      {builtin ? null : (
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
      )}
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
          className={ADD_BUTTON}
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

// ── Lists: the sections under the top of the sidebar ───────────────────────

function SwitchRow({
  id,
  title,
  detail,
  checked,
  onChange,
  testId,
}: {
  id: string;
  title: string;
  detail: string;
  checked: boolean;
  onChange: (on: boolean) => void;
  testId?: string;
}) {
  return (
    <div className="flex items-center justify-between gap-3 py-1">
      <span className="grid gap-px">
        <label htmlFor={id} className="text-[0.84rem] font-medium text-[var(--color-text-primary)]">
          {title}
        </label>
        <span className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">{detail}</span>
      </span>
      <Switch id={id} data-testid={testId} checked={checked} onCheckedChange={onChange} />
    </div>
  );
}

function SectionsSection({ layout, onChange }: { layout: SidebarLayout; onChange: (next: SidebarLayout) => void }) {
  const datesId = useId();
  const statusId = useId();
  const team = teamChatsMode(layout);
  // Team chats is a list of its own only when the person asks for one; then
  // it is ordered, named and folded like the others.
  const rows = layout.sections.filter((s) => s.id !== "team" || team === "own");
  return (
    <section aria-labelledby="sidebar-sections-heading" className="grid gap-2">
      <h3 id="sidebar-sections-heading" className={HEADING}>
        {tr("Lists")}
      </h3>
      <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">
        {tr("The sections under the top of the sidebar. Reorder, rename or hide them.")}
      </p>
      <ol className="grid gap-1" data-testid="sidebar-editor-sections">
        {rows.map((s, index) => {
          const name = s.label || sectionName(s.id);
          const move = (delta: number) => {
            // Step over a Team chats row that is not shown here.
            const target = rows[index + delta];
            if (!target) return;
            const from = layout.sections.findIndex((x) => x.id === s.id);
            const to = layout.sections.findIndex((x) => x.id === target.id);
            onChange(moveSection(layout, s.id as SectionId, to - from));
          };
          return (
            <li
              key={s.id}
              data-section-id={s.id}
              className={cn(
                "flex items-center gap-1 rounded-[var(--radius-md)] border bg-[var(--color-surface-1)] px-1.5 py-1.5",
                s.hidden ? "border-dashed border-[var(--color-border-strong)] bg-transparent" : "border-[var(--color-border)]",
              )}
            >
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
              <span className="flex shrink-0 flex-col">
                <button
                  type="button"
                  className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                  aria-label={tr("Move {0} up", [name])}
                  disabled={index === 0}
                  onClick={() => move(-1)}
                >
                  <Icon name="chevron-down" className="size-3 rotate-180" />
                </button>
                <button
                  type="button"
                  className={cn(ICON_BUTTON, "h-3.5 any-pointer-coarse:h-[1.125rem]")}
                  aria-label={tr("Move {0} down", [name])}
                  disabled={index === rows.length - 1}
                  onClick={() => move(1)}
                >
                  <Icon name="chevron-down" className="size-3" />
                </button>
              </span>
              <Switch
                checked={!s.hidden}
                onCheckedChange={(on) => onChange(updateSection(layout, s.id, { hidden: !on }))}
                aria-label={tr("Show {0} in the sidebar", [name])}
                title={s.hidden ? tr("Hidden — switch on to show") : tr("Shown — switch off to hide")}
                data-testid={`sidebar-section-show-${s.id}`}
                className="shrink-0"
              />
            </li>
          );
        })}
      </ol>

      <div className="mt-1 grid gap-1.5">
        <span id="sidebar-team-heading" className="text-[0.84rem] font-medium text-[var(--color-text-primary)]">
          {tr("Teammates' shared chats")}
        </span>
        <div className="seg-fill" data-testid="sidebar-editor-team-mode" data-value={team}>
          <Segmented<TeamChatsMode>
            label={tr("Teammates' shared chats")}
            value={team}
            options={[
              { value: "chats", label: tr("In Chats") },
              { value: "own", label: tr("Own list") },
              { value: "hidden", label: tr("Hidden") },
            ]}
            onChange={(mode) => onChange(setTeamChatsMode(layout, mode))}
          />
        </div>
        <p className="text-[0.75rem] leading-snug text-[var(--color-text-muted)]">
          {team === "chats"
            ? tr("Mixed into Chats with your own, with a Shared chip to see only theirs.")
            : team === "own"
              ? tr("In a Team chats list of their own, which you can place above or below your chats.")
              : tr("Not listed. Search still finds them.")}
        </p>
      </div>

      <SwitchRow
        id={datesId}
        testId="sidebar-editor-dates"
        title={tr("Group chats by date")}
        detail={tr("Today, Yesterday, This week, Older")}
        checked={!layout.dates_off}
        onChange={(on) => onChange({ ...layout, dates_off: on ? undefined : true })}
      />
      <SwitchRow
        id={statusId}
        testId="sidebar-editor-status"
        title={tr("Status on rows")}
        detail={tr("Patrol next check, sharing, Live, days left")}
        checked={!layout.status_off}
        onChange={(on) => onChange({ ...layout, status_off: on ? undefined : true })}
      />
    </section>
  );
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
