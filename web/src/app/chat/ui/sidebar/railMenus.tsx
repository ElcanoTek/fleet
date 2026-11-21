import { t as tr } from "@/app/lib/i18n";
import { useRef, useState } from "react";
import type { ReactNode } from "react";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { RAIL_HEADING_CLASS, RAIL_ICON_BUTTON_CLASS } from "@/app/shared/ui/railStyles";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Icon } from "../Icon";
import { RECENT_SORTS, type RecentSort, type LabelSummary } from "../conversationOrganization";
import { caret } from "./rowMenus";

// The sidebar's section heading (Pinned, Projects, Chats), the Chats menu
// (sort, label filter, select, delete temporary chats) and the + menu.

// ── Section heading ──────────────────────────────────────────────────────────
// "Pinned", "Projects", "Chats": a quiet line of text, with the section's one
// control at its right edge. The label folds the section when clicked. Its
// chevron is drawn where folding is something people do (Projects, a tree
// that gets long) and wherever a section is folded, so there is always a
// visible way back; elsewhere it shows on hover and focus.
export function SectionHeading({
  label,
  open,
  onToggle,
  showChevron = false,
  children,
}: {
  label: string;
  open: boolean;
  onToggle: () => void;
  showChevron?: boolean;
  // The control at the right edge (Reorder, the Chats menu).
  children?: ReactNode;
}) {
  return (
    <div className="flex min-h-8 items-center gap-1 pl-2.5 pr-0.5 pt-1">
      <button
        type="button"
        aria-expanded={open}
        onClick={onToggle}
        className={`group/heading inline-flex min-w-0 items-center gap-1 rounded-md py-1 pr-1 ${RAIL_HEADING_CLASS} transition hover:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-9`}
      >
        <span className="min-w-0 truncate">{label}</span>
        <Icon
          name="chevron-down"
          className={[
            "size-3 shrink-0 transition",
            open ? "" : "-rotate-90",
            showChevron || !open ? "" : "opacity-0 group-hover/heading:opacity-100 group-focus-visible/heading:opacity-100",
          ].join(" ")}
        />
      </button>
      <span className="ml-auto inline-flex shrink-0 items-center">{children}</span>
    </div>
  );
}

// ── The Chats menu ───────────────────────────────────────────────────────────
// The small button at the Chats heading's right edge: how the list is sorted,
// a label to filter by (when the person uses labels), select mode, and the
// one bulk action. "Delete temporary chats" says what it keeps, and that
// temporary chats go on their own: the list no longer carries a standing
// explainer for that.
export function ChatsMenu({
  sort,
  onSort,
  labels,
  filterLabels,
  onToggleLabel,
  onSelect,
  onDeleteTemporary,
  ttlDays,
}: {
  sort: RecentSort;
  onSort?: (next: RecentSort) => void;
  labels: LabelSummary[];
  filterLabels: string[];
  onToggleLabel: (name: string) => void;
  onSelect?: () => void;
  // Absent when the person has no temporary chats.
  onDeleteTemporary?: () => void;
  ttlDays: number;
}) {
  const [open, setOpen] = useState(false);
  const [labelsOpen, setLabelsOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const flyoutAnchorRef = useRef<HTMLElement | null>(null);
  const close = () => {
    setOpen(false);
    setLabelsOpen(false);
  };
  const current = RECENT_SORTS.find((o) => o.value === sort) ?? RECENT_SORTS[0];
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        // Named for what it is and what it is set to.
        aria-label={tr("Chat list options. Sorted by: {0}", [current.label])}
        title={tr("Sort, select and tidy up")}
        data-testid="recent-sort"
        data-value={sort}
        onClick={() => {
          setLabelsOpen(false);
          setOpen((o) => !o);
        }}
        className={`${RAIL_ICON_BUTTON_CLASS} relative size-7`}
      >
        <Icon name="filter" className="size-[0.95rem]" />
        {filterLabels.length > 0 ? (
          <span aria-hidden="true" className="absolute right-1 top-1 size-1.5 rounded-full bg-[var(--color-accent)]" />
        ) : null}
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Chat list options")}
        className="w-[min(17rem,calc(100vw-1rem))]"
        flyout={
          labelsOpen ? (
            <>
              {labels.map((l) => {
                const on = filterLabels.includes(l.name);
                return (
                  <button
                    key={l.name}
                    type="button"
                    role="menuitemcheckbox"
                    aria-checked={on}
                    onClick={() => onToggleLabel(l.name)}
                    className="flex w-full items-center gap-2 rounded-[0.4rem] px-2 py-1.5 text-left text-[0.82rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:bg-[var(--color-overlay-soft)] focus-visible:outline-none any-pointer-coarse:min-h-11"
                  >
                    <span className="label-dot" style={labelChipStyle(l.name)} />
                    <span className="min-w-0 flex-1 truncate">{l.name}</span>
                    <span className="text-[0.7rem] tabular-nums text-[var(--color-text-muted)]">{l.count}</span>
                    <Icon name="check" className={["size-3.5 shrink-0 text-[var(--color-accent)]", on ? "" : "opacity-0"].join(" ")} />
                  </button>
                );
              })}
            </>
          ) : null
        }
        flyoutOpen={labelsOpen}
        flyoutAnchorRef={flyoutAnchorRef}
        onFlyoutClose={() => setLabelsOpen(false)}
        flyoutLabel={tr("Filter by label")}
      >
        {onSort ? (
          <>
            <p className="m-0 px-2 pb-0.5 pt-1 text-[0.7rem] font-medium text-[var(--color-text-muted)]">{tr("Sort")}</p>
            {RECENT_SORTS.map((o) => (
              <button
                key={o.value}
                type="button"
                role="menuitemradio"
                aria-checked={o.value === sort}
                data-testid={`recent-sort-${o.value}`}
                onClick={() => {
                  close();
                  anchorRef.current?.focus();
                  if (o.value !== sort) onSort(o.value);
                }}
                className={[
                  "flex w-full items-center gap-2 rounded-[0.4rem] px-2 py-1.5 text-left text-[0.82rem] transition any-pointer-coarse:min-h-11",
                  "hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:bg-[var(--color-overlay-soft)] focus-visible:outline-none",
                  o.value === sort ? "text-[var(--color-text-primary)]" : "text-[var(--color-text-secondary)]",
                ].join(" ")}
              >
                <span className="min-w-0 flex-1 truncate">{o.label}</span>
                {o.value === sort ? <Icon name="check" className="size-3.5 shrink-0 text-[var(--color-accent)]" /> : null}
              </button>
            ))}
          </>
        ) : null}
        {labels.length > 0 ? (
          <>
            <MenuSeparator />
            <MenuItem
              icon={<Icon name="tag" className="size-4" />}
              ariaHasPopup
              ariaExpanded={labelsOpen}
              trailing={caret}
              testId="chats-menu-labels"
              onClick={(e) => {
                flyoutAnchorRef.current = e.currentTarget;
                setLabelsOpen((o) => !o);
              }}
            >
              {filterLabels.length > 0 ? tr("Filter by label ({0})", [filterLabels.length]) : tr("Filter by label")}
            </MenuItem>
          </>
        ) : null}
        {onSelect ? (
          <>
            <MenuSeparator />
            <MenuItem
              icon={<Icon name="check-square" className="size-4" />}
              testId="chats-menu-select"
              onClick={() => {
                close();
                onSelect();
              }}
            >
              {tr("Select chats…")}
            </MenuItem>
          </>
        ) : null}
        {onDeleteTemporary ? (
          <>
            <MenuSeparator />
            <MenuItem
              danger
              icon={<Icon name="trash" className="size-4" />}
              testId="delete-temporary-chats"
              description={tr("Keeps pinned, filed, shared and patrol chats. Temporary chats go after {0} days without activity anyway.", [ttlDays])}
              onClick={() => {
                close();
                onDeleteTemporary();
              }}
            >
              {tr("Delete temporary chats…")}
            </MenuItem>
          </>
        ) : null}
      </Menu>
    </>
  );
}

// ── The + menu ───────────────────────────────────────────────────────────────
// Everything a person can start, in one place beside the pen: a chat, a
// patrol, a background task; then a project and (where the deployment has
// them) a sealed chat.
export function NewMenu({
  collapsed,
  newChatShortcut,
  sealedAvailable,
  onNewChat,
  onNewPatrol,
  onNewTask,
  onNewProject,
  onNewSealedChat,
}: {
  collapsed: boolean;
  newChatShortcut?: string;
  sealedAvailable: boolean;
  onNewChat: () => void;
  onNewPatrol: (returnFocusTo: HTMLElement | null) => void;
  onNewTask?: () => void;
  onNewProject: () => void;
  onNewSealedChat: () => void;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const close = () => setOpen(false);
  const item = (fn: () => void) => () => {
    close();
    fn();
  };
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={tr("New…")}
        data-testid="new-menu-button"
        data-tip={collapsed ? tr("New…") : undefined}
        title={collapsed ? undefined : tr("New chat, patrol, background task or project")}
        onClick={() => setOpen((o) => !o)}
        className={[
          "inline-flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:size-11",
          collapsed ? "sm:size-10" : "",
          open ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]" : "",
        ].join(" ")}
      >
        <Icon name="plus" className="size-[1.05rem]" />
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        // Collapsed, the sidebar is an icon strip: the menu opens beside it.
        placement={collapsed ? "right-start" : "bottom-end"}
        label={tr("New")}
        className="w-[min(19rem,calc(100vw-1rem))]"
      >
        <MenuItem
          icon={<Icon name="edit" className="size-4" />}
          description={tr("Talk it through with an agent.")}
          trailing={
            newChatShortcut ? (
              <span className="text-[0.68rem] tabular-nums text-[var(--color-text-muted)] any-pointer-coarse:hidden">{newChatShortcut}</span>
            ) : undefined
          }
          testId="new-menu-chat"
          onClick={item(onNewChat)}
        >
          {tr("New chat")}
        </MenuItem>
        <MenuItem
          icon={<Icon name="sparkles" className="size-4" />}
          description={tr("A chat that checks in on a schedule. Opens the patrol wizard.")}
          testId="new-menu-patrol"
          onClick={() => {
            close();
            onNewPatrol(anchorRef.current);
          }}
        >
          {tr("New patrol")}
        </MenuItem>
        {onNewTask ? (
          <MenuItem
            icon={<Icon name="grid" className="size-4" />}
            description={tr("Runs on its own, without a chat.")}
            testId="new-menu-task"
            onClick={item(onNewTask)}
          >
            {tr("New background task")}
          </MenuItem>
        ) : null}
        <MenuSeparator />
        <MenuItem icon={<Icon name="briefcase" className="size-4" />} testId="new-menu-project" onClick={item(onNewProject)}>
          {tr("New project")}
        </MenuItem>
        {sealedAvailable ? (
          <MenuItem
            icon={<Icon name="lock" className="size-4" />}
            testId="new-menu-sealed"
            title={tr("Locked down and private. Your data and an approved model stay inside this sandbox; nothing leaves.")}
            onClick={item(onNewSealedChat)}
          >
            {tr("New sealed chat")}
          </MenuItem>
        ) : null}
      </Menu>
    </>
  );
}
