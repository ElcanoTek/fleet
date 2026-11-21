import { rich } from "@/app/lib/i18n/rich";
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
import { createPortal } from "react-dom";
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { navigate } from "@/app/lib/navigation";
import { NavRail, type RailCollapse, type RailView } from "@/app/shared/ui/NavRail";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import {
  RAIL_HEADING_ACTION_CLASS,
  RAIL_HEADING_CLASS,
  RAIL_ICON_BUTTON_CLASS,
  railIconClass,
} from "@/app/shared/ui/railStyles";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Kbd } from "@/components/ui/kbd";
import { Icon } from "./Icon";
import {
  CHAT_SCOPES,
  MAX_CHAT_ORDER,
  MAX_LABELS,
  MAX_LABEL_LEN,
  PINNED_ORDER_SCOPE,
  RECENT_SORTS,
  dateGroupLabel,
  groupByDate,
  pinnedEntries,
  pinnedGroup,
  projectOrderScope,
  sortRecent,
  type ChatScope,
  type RecentSort,
  addLabel as addLabelTo,
  canAddLabel,
  deriveLabels,
  filterConversations,
  isFiltering as computeIsFiltering,
  matchesSearch,
  searchMatchRanges,
  searchTokens,
  pinnedUnfiled,
  projectGroups,
  railProjectEmptyState,
  recentUnfiled,
  type LabelSummary,
} from "./conversationOrganization";
import type {
  ConversationSummary,
  PendingDeleteConversation,
  ServerConfig,
} from "./chat-experience";
import type { Project } from "./ProjectsModal";
import { previewNodes, type SearchResult } from "./SearchBar";
import { isTemporaryChat } from "./deleteAllUnpinnedScope";
import { expiresInDays, nearCap, retentionPolicy, useNowSeconds } from "./chatRetention";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { teamLinkUrl } from "./teamSharing";
import { copyText } from "@/app/shared/lib/clipboard";
import { ChatReorderList, ReorderDoneButton } from "./ChatReorderList";
import { KeepAtTopItem, StartPageItem } from "@/app/shared/sidebar/SidebarMenuItems";
import { RowStatus, describeRowStatus, type RowStatusInput } from "@/app/shared/sidebar/RowStatus";
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

// A row's options (⋯) button: revealed on hover or keyboard focus where
// there is a mouse, always shown on a touch screen, which has no hover to
// reveal it with (a phone user otherwise had to know where to tap).
const ROW_ACTIONS_REVEAL =
  "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 any-pointer-coarse:opacity-100";

// While the ⋯ button shows, the row's status steps aside for it and the title
// keeps clear of it. A mouse reveals the button on hover or focus (and while
// its menu is open); a touch screen always shows it, so there the status
// stays, to its left.
const ROW_CLEAR_FOR_ACTIONS =
  "group-hover:pr-9 group-focus-within:pr-9 group-has-[[aria-expanded=true]]:pr-9 any-pointer-coarse:pr-11";
const STATUS_YIELDS_TO_ACTIONS =
  "group-hover:hidden group-focus-within:hidden group-has-[[aria-expanded=true]]:hidden any-pointer-coarse:!inline-flex";

const KEBAB_CLASS =
  // A fixed, centered, padded square: a rounded hover highlight around the
  // centered icon. hit-area extends the clickable box without growing the fill.
  "hit-area hit-area-touch pointer-events-auto inline-flex size-7 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none";

const RENAME_INPUT_CLASS =
  "mx-1 my-[0.2rem] w-[calc(100%-0.5rem)] rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1 text-[length:var(--rail-row-text)] text-[var(--color-text-primary)] outline-none";

function usePortalTip(tip: string) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    // Focus shows the tip for a keyboard user only. A tap or click also
    // focuses the button — and so does a menu handing the focus back when it
    // closes — and on a touch screen, with no pointer to move away, the tip
    // then sat over the list for good.
    if (e.type === "focus") {
      let keyboard = true;
      try {
        keyboard = e.currentTarget.matches(":focus-visible");
      } catch {
        // An engine without :focus-visible: show, as before.
      }
      if (!keyboard) return;
    }
    const r = e.currentTarget.getBoundingClientRect();
    // Below the button, arrow pointing back up at it (the design's conv-info
    // tooltip placement: bottom + 7, offset so the arrow lands on the anchor).
    setPos({
      top: Math.round(r.bottom + 7),
      left: Math.round(r.left + r.width / 2 - 17),
    });
  };
  const hide = () => setPos(null);
  // The tip is placed once, where its button was: when anything scrolls, the
  // button moves and the tip would be left pointing at another row.
  const shown = pos !== null;
  useEffect(() => {
    if (!shown) return;
    const onScroll = () => setPos(null);
    window.addEventListener("scroll", onScroll, true);
    return () => window.removeEventListener("scroll", onScroll, true);
  }, [shown]);
  const tipNode =
    pos && typeof document !== "undefined"
      ? createPortal(
          <span
            role="tooltip"
            className="conv-tooltip"
            style={{ top: pos.top, left: pos.left }}
          >
            {tip}
          </span>,
          document.body,
        )
      : null;
  return { show, hide, tipNode };
}


// ── Temporary-chat notices ───────────────────────────────────────────────────
// Under the Chats heading, only when there is something to say: how many of
// the person's chats are about to be deleted (a line that takes them to the
// first one), and that they are close to the most temporary chats one person
// keeps. Silent otherwise — a standing notice is one people stop reading
// (docs/CHAT-RETENTION.md). One quiet line each: the rows themselves carry
// the days left.
function RetentionNotices({
  expiringCount,
  temporaryCount,
  cap,
  onShowFirst,
}: {
  expiringCount: number;
  temporaryCount: number;
  cap?: number;
  onShowFirst: () => void;
}) {
  const capNear = nearCap(temporaryCount, cap);
  if (expiringCount === 0 && !capNear) return null;
  return (
    <div className="mb-0.5 grid gap-0.5 px-1 text-[0.75rem] leading-snug">
      {expiringCount > 0 ? (
        <button
          type="button"
          data-testid="expiring-chats-notice"
          className="flex w-full items-center gap-1.5 rounded-md px-1.5 py-1 text-left text-[var(--color-warning-text)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-9"
          title={tr("Show the first of them. Pin, file or archive a chat to keep it.")}
          onClick={onShowFirst}
        >
          <Icon name="hourglass" className="size-3 shrink-0" />
          <span className="min-w-0 truncate">
            {expiringCount === 1
              ? tr("1 chat will be deleted soon.")
              : tr("{0} chats will be deleted soon.", [expiringCount])}
          </span>
        </button>
      ) : null}
      {capNear && cap ? (
        <p
          data-testid="temporary-cap-notice"
          role="status"
          className="m-0 px-1.5 py-1 text-[var(--color-text-secondary)]"
        >
          {tr("You have {0} temporary chats. Past {1}, the least recently active are deleted first.", [temporaryCount, cap])}
        </p>
      ) : null}
    </div>
  );
}

// ── Label chips ────────────────────────────────────────────────────────────
function LabelChip({
  name,
  removable,
  onRemove,
}: {
  name: string;
  removable?: boolean;
  onRemove?: () => void;
}) {
  return (
    <span className="conv-label-chip" style={labelChipStyle(name)}>
      {name}
      {removable ? (
        <button
          type="button"
          aria-label={tr("Remove {0}", [name])}
          className="hit-area ml-0.5 inline-flex size-3.5 items-center justify-center rounded-full text-current opacity-70 transition hover:bg-white/20 hover:opacity-100 focus-visible:opacity-100 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={(e) => {
            e.stopPropagation();
            onRemove?.();
          }}
        >
          <Icon name="close" className="size-2.5" />
        </button>
      ) : null}
    </span>
  );
}

// CONV_DRAG_MIME is the custom drag payload type for a conversation row —
// namespaced so a project row only accepts our rows, never a stray text/file
// drag from outside the app.
const CONV_DRAG_MIME = "application/x-fleet-conversation";

// ── Project picker panel (per-row kebab) ─────────────────────────────────────
// The touch/keyboard counterpart of dragging a chat onto a rail project
// (#509 follow-up): pick a project to re-file into, or unfile. Projects are
// created in the Projects modal (they carry instructions/membership), so
// there is no inline "New project…" here.
function ProjectPanel({
  projects,
  currentProjectId,
  onPick,
  onRemove,
}: {
  projects: Project[];
  currentProjectId?: string;
  onPick: (projectID: string) => void;
  onRemove: () => void;
}) {
  return (
    <>
      {projects.map((p) => (
        <MenuItem
          key={p.id}
          icon={
            <Icon
              name="check"
              className={[
                "size-4",
                currentProjectId === p.id
                  ? "opacity-100 text-[var(--color-accent)]"
                  : "opacity-0",
              ].join(" ")}
            />
          }
          onClick={() => onPick(p.id)}
        >
          {p.name}
        </MenuItem>
      ))}
      {currentProjectId ? (
        <>
          {projects.length > 0 ? <MenuSeparator /> : null}
          <MenuItem
            icon={<Icon name="close" className="size-4" />}
            onClick={onRemove}
          >
            {tr("Remove from project")}
          </MenuItem>
        </>
      ) : null}
    </>
  );
}

// ── Labels editor panel (shared by per-row kebab + bulk bar) ─────────────────
function LabelsPanel({
  current,
  suggestions,
  onAdd,
  onRemove,
}: {
  current: string[];
  suggestions: string[];
  onAdd: (label: string) => void;
  onRemove?: (label: string) => void;
}) {
  const [input, setInput] = useState("");
  const atMax = current.length >= MAX_LABELS;
  const add = (raw: string) => {
    if (!canAddLabel(current, raw)) return;
    onAdd(raw.trim().slice(0, MAX_LABEL_LEN));
    setInput("");
  };
  const fresh = suggestions.filter((s) => !current.includes(s));
  return (
    <div className="flex flex-col gap-2 p-1">
      {current.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {current.map((l) => (
            <LabelChip
              key={l}
              name={l}
              removable
              onRemove={() => onRemove?.(l)}
            />
          ))}
        </div>
      ) : null}
      {/* No autoFocus, and no replacement effect: this panel only ever renders
          inside a <Menu> surface, and Menu already moves focus to the first
          focusable item of a menu/flyout it opens. The attribute was fighting
          that — it ran at commit and Menu's effect ran after it, so with any
          existing chips the browser landed on the first chip's Remove button
          anyway. Removing it changes nothing a user sees; the covering test
          asserts the field is still focused when the panel opens. */}
      <input
        className="rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none disabled:cursor-not-allowed disabled:opacity-60"
        placeholder={atMax ? tr("Max {0} labels", [MAX_LABELS]) : tr("Add a label…")}
        maxLength={MAX_LABEL_LEN}
        disabled={atMax}
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") {
            e.preventDefault();
            add(input);
          }
        }}
      />
      {fresh.length > 0 && !atMax ? (
        <div className="flex flex-col gap-1">
          <span className="text-[0.66rem] uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            {tr("Suggestions")}
          </span>
          <div className="flex flex-wrap gap-1.5">
            {fresh.map((s) => (
              <button
                key={s}
                type="button"
                className="conv-label-chip"
                style={labelChipStyle(s)}
                onClick={() => add(s)}
              >
                {s}
              </button>
            ))}
          </div>
        </div>
      ) : null}
    </div>
  );
}

// ── Row menus ────────────────────────────────────────────────────────────────
// One order for every ⋯ menu, so the same thing is in the same place on a
// chat, a project and a teammate's chat (docs/SIDEBAR.md):
//
//   where it lives     Pin · Move to the top · Rename · Move to project · Labels
//   where fleet opens  Set as start page
//   make something     Add a patrol · Make a background task · Save as workflow
//   pass it on         Share · Download · Transfer ownership
//   put it away        Archive · Delete
//
// A group with nothing to offer (a teammate's chat cannot be renamed, filed or
// deleted by the viewer) is left out with its separator.

const caret = (
  <Icon name="chevron-right" className="size-3.5 text-[var(--color-text-muted)]" />
);

function KebabButton({
  open,
  label,
  anchorRef,
  onToggle,
  testId,
  dataProjectOptions,
}: {
  open: boolean;
  label: string;
  anchorRef: RefObject<HTMLButtonElement | null>;
  onToggle: () => void;
  testId?: string;
  dataProjectOptions?: string;
}) {
  const tip = usePortalTip(tr("Options"));
  return (
    <button
      ref={anchorRef}
      type="button"
      aria-haspopup="menu"
      aria-expanded={open}
      aria-label={label}
      data-testid={testId}
      data-project-options={dataProjectOptions}
      onMouseEnter={(e) => {
        if (!open) tip.show(e);
      }}
      onMouseLeave={tip.hide}
      onFocus={(e) => {
        if (!open) tip.show(e);
      }}
      onBlur={tip.hide}
      className={[KEBAB_CLASS, open ? "opacity-100" : ROW_ACTIONS_REVEAL].join(" ")}
      onClick={(e) => {
        e.stopPropagation();
        tip.hide();
        onToggle();
      }}
    >
      <Icon name="dots" className="size-[1.05rem]" />
      {tip.tipNode}
    </button>
  );
}

// ── Per-project menu ─────────────────────────────────────────────────────────
// Open and New chat for every member; the owner's list-keeping (Pin, Rename),
// sharing and settings (both open the project's settings dialog, where "Who
// can see it" lives) and Delete are the owner's: only the owner can edit,
// share or delete a project.
function ProjectKebab({
  projectId,
  projectName,
  pinned,
  isOwner,
  onOpen,
  onNewChat,
  onEdit,
  onPin,
  onShare,
  onRename,
  onDelete,
  onReorder,
  readOnly = false,
}: {
  projectId: string;
  projectName: string;
  pinned: boolean;
  isOwner: boolean;
  // A read-only (Viewer) account: no New chat, and none of the owner's
  // changes, whoever owns the project — the API refuses them all.
  readOnly?: boolean;
  onOpen: () => void;
  onNewChat: () => void;
  // Offered when the project holds two or more of the viewer's chats.
  onReorder?: () => void;
  onEdit: () => void;
  onPin: () => void;
  onShare: () => void;
  onRename: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const close = () => setOpen(false);
  const item = (fn: () => void) => () => {
    close();
    fn();
  };
  const target = { kind: "project" as const, ref: projectId, title: projectName };
  const owner = isOwner && !readOnly;
  return (
    <>
      <KebabButton
        open={open}
        label={tr("Project options for {0}", [projectName])}
        anchorRef={anchorRef}
        dataProjectOptions={projectId}
        onToggle={() => setOpen((o) => !o)}
      />
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for project {0}", [projectName])}
        className="min-w-[13rem]"
      >
        <MenuItem icon={<Icon name="external-link" className="size-4" />} onClick={item(onOpen)}>
          {tr("Open project")}
        </MenuItem>
        {readOnly ? null : (
          <MenuItem icon={<Icon name="edit" className="size-4" />} onClick={item(onNewChat)}>
            {tr("New chat")}
          </MenuItem>
        )}
        {onReorder ? (
          <MenuItem icon={<Icon name="selector" className="size-4" />} onClick={item(onReorder)}>
            {tr("Reorder chats")}
          </MenuItem>
        ) : null}
        <MenuSeparator />
        {owner ? (
          <MenuItem icon={<Icon name="pin" className="size-4" />} onClick={item(onPin)}>
            {pinned ? tr("Unpin") : tr("Pin")}
          </MenuItem>
        ) : null}
        {/* Keeping it at the top and opening fleet to it are the viewer's own
            (docs/SIDEBAR.md), so every member gets them. */}
        <KeepAtTopItem target={target} onDone={close} />
        {owner ? (
          <MenuItem icon={<Icon name="edit" className="size-4" />} onClick={item(onRename)}>
            {tr("Rename")}
          </MenuItem>
        ) : null}
        <StartPageItem target={target} onDone={close} />
        {owner ? (
          <>
            <MenuSeparator />
            {/* The TEAM glyph, not the chain link: the chain link means "by
                URL" and must not read as "my team can see this"
                (ShareGlyphs.tsx). */}
            <MenuItem icon={<TeamGlyph className="size-4" />} onClick={item(onShare)}>
              {tr("Share…")}
            </MenuItem>
            <MenuItem icon={<Icon name="settings" className="size-4" />} onClick={item(onEdit)}>
              {tr("Project settings…")}
            </MenuItem>
            <MenuSeparator />
            <MenuItem danger icon={<Icon name="trash" className="size-4" />} onClick={item(onDelete)}>
              {tr("Delete project")}
            </MenuItem>
          </>
        ) : null}
      </Menu>
    </>
  );
}

// ── Per-chat menu (the person's own chat) ────────────────────────────────────
function ConversationKebab({
  conversation,
  allLabelNames,
  projects,
  hasPatrol,
  onPin,
  onRename,
  onDownload,
  onPromote,
  onSavePrompt,
  onPatrol,
  onSetLabels,
  onSetProject,
  onShare,
  onTransfer,
  isShared,
  onArchive,
  onDelete,
  readOnly = false,
}: {
  conversation: ConversationSummary;
  // A read-only (Viewer) account: the menu keeps Download and leaves out
  // every change the API would refuse (Move to the top and Set as start
  // page already hide themselves for a Viewer: useCanCustomize).
  readOnly?: boolean;
  allLabelNames: string[];
  projects: Project[];
  // The chat already checks in on a schedule: the item opens its patrols
  // rather than offering to add one.
  hasPatrol: boolean;
  onPin: () => void;
  onRename: () => void;
  onDownload: () => void;
  onPromote: () => void;
  onSavePrompt: () => void;
  // Opens the chat with its patrols dialog up. Absent where the page cannot.
  onPatrol?: () => void;
  onSetLabels: (labels: string[]) => void;
  onSetProject: (projectID: string | null) => void;
  // Opens the Share dialog (both scopes). It no longer mints a link as a
  // side effect of opening: creating one is a click inside the dialog.
  onShare: () => void;
  // Opens the Transfer ownership dialog (docs/CHAT-TRANSFER.md).
  onTransfer: () => void;
  isShared: boolean;
  onArchive: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [flyout, setFlyout] = useState<null | "labels" | "project">(null);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  // The menu item that opened the active flyout — the flyout anchors to it and
  // focus returns here on Escape. Captured from the click's currentTarget.
  const flyoutAnchorRef = useRef<HTMLElement | null>(null);
  const close = () => {
    setOpen(false);
    setFlyout(null);
  };
  const toggleFlyout = (which: "labels" | "project", el: HTMLElement) => {
    flyoutAnchorRef.current = el;
    setFlyout((cur) => (cur === which ? null : which));
  };
  const item = (fn: () => void) => () => {
    close();
    fn();
  };
  const labels = conversation.labels ?? [];
  const target = { kind: "chat" as const, ref: conversation.id, title: conversation.title };
  const flyoutContent =
    flyout === "project" ? (
      <ProjectPanel
        projects={projects}
        currentProjectId={conversation.project_id || undefined}
        onPick={(projectID) => {
          onSetProject(projectID);
          close();
        }}
        onRemove={() => {
          onSetProject(null);
          close();
        }}
      />
    ) : flyout === "labels" ? (
      <LabelsPanel
        current={labels}
        suggestions={allLabelNames}
        onAdd={(label) => onSetLabels(addLabelTo(labels, label))}
        onRemove={(label) => onSetLabels(labels.filter((l) => l !== label))}
      />
    ) : null;

  return (
    <>
      <KebabButton
        open={open}
        label={tr("Conversation options for {0}", [conversation.title])}
        anchorRef={anchorRef}
        onToggle={() => {
          setFlyout(null);
          setOpen((o) => !o);
        }}
      />
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for {0}", [conversation.title])}
        className="min-w-[13.5rem]"
        flyout={flyoutContent}
        flyoutOpen={flyout !== null}
        flyoutAnchorRef={flyoutAnchorRef}
        onFlyoutClose={() => setFlyout(null)}
        flyoutLabel={flyout === "project" ? tr("Move to project") : tr("Labels")}
      >
        {/* A read-only (Viewer) account keeps only Download: every other
            item here is a write the API refuses (the two layout items hide
            themselves for a Viewer). */}
        {/* Pinning is meaningless for a project chat: it lives only under
            its project (never in Pinned) and is already retention-exempt —
            filing even clears the flag server-side. Hide rather than no-op. */}
        {readOnly || conversation.project_id ? null : (
          <MenuItem icon={<Icon name="pin" className="size-4" />} onClick={item(onPin)}>
            {conversation.pinned ? tr("Unpin") : tr("Pin")}
          </MenuItem>
        )}
        <KeepAtTopItem target={target} onDone={close} />
        {readOnly ? null : (
          <>
            <MenuItem icon={<Icon name="edit" className="size-4" />} onClick={item(onRename)}>
              {tr("Rename")}
            </MenuItem>
            {projects.length > 0 || conversation.project_id ? (
              <MenuItem
                icon={<Icon name="briefcase" className="size-4" />}
                ariaHasPopup
                ariaExpanded={flyout === "project"}
                trailing={caret}
                onClick={(e) => toggleFlyout("project", e.currentTarget)}
              >
                {tr("Move to project")}
              </MenuItem>
            ) : null}
            <MenuItem
              icon={<Icon name="tag" className="size-4" />}
              ariaHasPopup
              ariaExpanded={flyout === "labels"}
              trailing={caret}
              onClick={(e) => toggleFlyout("labels", e.currentTarget)}
            >
              {tr("Labels")}
            </MenuItem>
          </>
        )}
        <MenuSeparator />
        <StartPageItem target={target} onDone={close} />
        <MenuSeparator />
        {readOnly ? null : (
          <>
            {onPatrol && !conversation.lockdown ? (
              <MenuItem icon={<Icon name="activity" className="size-4" />} testId="chat-menu-patrol" onClick={item(onPatrol)}>
                {hasPatrol ? tr("Patrols…") : tr("Add a patrol…")}
              </MenuItem>
            ) : null}
            <MenuItem icon={<Icon name="grid" className="size-4" />} onClick={item(onPromote)}>
              {tr("Make a background task…")}
            </MenuItem>
            <MenuItem icon={<Icon name="book" className="size-4" />} onClick={item(onSavePrompt)}>
              {tr("Save as workflow…")}
            </MenuItem>
            <MenuSeparator />
            {/* One entry, one dialog, two audiences (ADR-0057): share by link, and
                share with the team. The glyph names the scope in play when only
                one is: a chat shared with the team but not by link gets the team
                mark, everything else the link mark (the dialog holds both). */}
            <MenuItem
              icon={
                !isShared && conversation.team_visible ? (
                  <TeamGlyph className="size-4" />
                ) : (
                  <ShareGlyph className="size-4" />
                )
              }
              onClick={item(onShare)}
            >
              {tr("Share…")}
            </MenuItem>
          </>
        )}
        {/* Was "Download as JSON" — a label that named a file format instead
            of an outcome. The dialog it opens picks the artifact. */}
        <MenuItem icon={<Icon name="download" className="size-4" />} onClick={item(onDownload)}>
          {tr("Download chat…")}
        </MenuItem>
        {readOnly ? null : (
          <>
            {/* An archived or Lockdown chat cannot change hands (the server
                refuses it), so the item is not offered on one. */}
            {conversation.lockdown || conversation.archived_at ? null : (
              <MenuItem icon={<Icon name="arrow-right" className="size-4" />} onClick={item(onTransfer)}>
                {tr("Transfer ownership…")}
              </MenuItem>
            )}
            <MenuSeparator />
            <MenuItem icon={<Icon name="archive" className="size-4" />} onClick={item(onArchive)}>
              {tr("Archive")}
            </MenuItem>
            <MenuItem danger icon={<Icon name="trash" className="size-4" />} onClick={item(onDelete)}>
              {tr("Delete…")}
            </MenuItem>
          </>
        )}
      </Menu>
    </>
  );
}

// ── A teammate's chat ────────────────────────────────────────────────────────
// A teammate's chat is not the viewer's to rename, file, archive or delete, so
// its menu holds what any member can do with it: pin it for themselves, keep
// it at the top, open fleet to it, copy a link that opens it (how a teammate
// passes it on), and — in a Multiplayer chat they take part in — download it.
// The link is the one that works for this row: the team link
// (/chat?team=<id>, the link Share hands out) for a chat shared with the team,
// the chat link (/chat?c=<id>) for one reached only through Multiplayer (a
// team link refuses it), and none otherwise.
function teamChatLink(c: ConversationSummary): string | null {
  if (c.team_visible) return teamLinkUrl(c.id);
  if (c.multiplayer && typeof window !== "undefined") {
    return `${window.location.origin}/chat?c=${encodeURIComponent(c.id)}`;
  }
  return null;
}


function TeamChatKebab({
  conversation,
  onPin,
  onDownload,
  onTransfer,
}: {
  conversation: ConversationSummary;
  // Pin or unpin the chat for the viewer alone (their own list; nothing
  // changes for its owner). Absent for an account that cannot save (a
  // Viewer), or while the role is unknown.
  onPin?: () => void;
  // Download the chat. Set only for a Multiplayer chat the viewer takes part
  // in: the export route serves a participant (with other people's private
  // context left out) and refuses a read-only team reader.
  onDownload?: () => void;
  // Set for an admin only: an admin may hand any chat to another person.
  onTransfer?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [copy, setCopy] = useState<"idle" | "copied" | "failed">("idle");
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const closeTimer = useRef<number | null>(null);
  // Bumped on every close: a copy still pending when the menu closed (a
  // browser asking for permission) must not write its result into, or
  // close, a later opening of the menu.
  const attempt = useRef(0);
  const fallbackRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (copy !== "failed") return;
    fallbackRef.current?.focus();
    fallbackRef.current?.select();
  }, [copy]);
  useEffect(
    () => () => {
      if (closeTimer.current !== null) window.clearTimeout(closeTimer.current);
    },
    [],
  );
  const close = () => {
    attempt.current += 1;
    if (closeTimer.current !== null) window.clearTimeout(closeTimer.current);
    closeTimer.current = null;
    setOpen(false);
    setCopy("idle");
  };
  const link = teamChatLink(conversation);
  const target = { kind: "chat" as const, ref: conversation.id, title: conversation.title };
  const passOn = Boolean(link || onDownload || (onTransfer && !conversation.lockdown && !conversation.archived_at));
  return (
    <>
      <KebabButton
        open={open}
        label={tr("Conversation options for {0}", [conversation.title])}
        anchorRef={anchorRef}
        testId="team-chat-options"
        onToggle={() => {
          if (open) close();
          else setOpen(true);
        }}
      />
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for {0}", [conversation.title])}
        className="min-w-[13rem]"
      >
        {onPin ? (
          <MenuItem
            icon={<Icon name="pin" className="size-4" />}
            testId="team-chat-pin"
            onClick={() => {
              onPin();
              close();
            }}
          >
            {conversation.pinned ? tr("Unpin") : tr("Pin")}
          </MenuItem>
        ) : null}
        <KeepAtTopItem target={target} onDone={close} />
        <StartPageItem target={target} onDone={close} />
        {passOn ? <MenuSeparator /> : null}
        {link ? (
          <MenuItem
            icon={<ShareGlyph className="size-4" />}
            onClick={() => {
              // The menu stays open long enough to say whether it worked.
              // Each press is its own attempt: only the latest may report,
              // and an earlier success's pending close is called off.
              const mine = ++attempt.current;
              if (closeTimer.current !== null) window.clearTimeout(closeTimer.current);
              closeTimer.current = null;
              void copyText(link).then((ok) => {
                if (mine !== attempt.current) return;
                setCopy(ok ? "copied" : "failed");
                if (!ok) return;
                closeTimer.current = window.setTimeout(() => {
                  // As Escape does: a keyboard user still on the menu goes
                  // back to the ⋯ button, not to the page body when the
                  // menu unmounts. Focus moved elsewhere meanwhile stays.
                  if (document.activeElement?.closest('[role="menu"]')) anchorRef.current?.focus();
                  close();
                }, 900);
              });
            }}
          >
            {copy === "copied" ? tr("Copied ✓") : copy === "failed" ? tr("Copy failed") : tr("Copy link")}
          </MenuItem>
        ) : null}
        {link && copy === "failed" ? (
          // The clipboard refused (no permission, an insecure page): the
          // link is shown to copy by hand, as Share does. It sits in a
          // read-only field, focused and selected, so a keyboard user can
          // copy it too (Tab reaches it inside the menu's focus trap).
          <div data-testid="team-chat-link-fallback" className="max-w-[16rem] px-3 py-1.5">
            <p role="status" className="m-0 text-[0.72rem] text-[var(--color-text-secondary)]">
              {tr("Couldn’t copy the link. Copy it from here: {0}", [""]).trim()}
            </p>
            <input
              ref={fallbackRef}
              readOnly
              value={link}
              aria-label={tr("Copy link")}
              onFocus={(e) => e.currentTarget.select()}
              className="mt-1 w-full rounded-[var(--radius-sm)] border border-[var(--color-border)] bg-transparent px-1.5 py-1 text-[0.72rem] text-[var(--color-text-primary)] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none any-pointer-coarse:min-h-11"
            />
          </div>
        ) : null}
        {onDownload ? (
          <MenuItem
            icon={<Icon name="download" className="size-4" />}
            testId="team-chat-download"
            onClick={() => {
              close();
              onDownload();
            }}
          >
            {tr("Download chat…")}
          </MenuItem>
        ) : null}
        {onTransfer && !conversation.lockdown && !conversation.archived_at ? (
          <MenuItem
            icon={<Icon name="arrow-right" className="size-4" />}
            onClick={() => {
              close();
              onTransfer();
            }}
          >
            {tr("Transfer ownership…")}
          </MenuItem>
        ) : null}
      </Menu>
    </>
  );
}

// ── A chat's row ─────────────────────────────────────────────────────────────
// The title, and one status at the right edge when it matters (RowStatus).
// The same row serves the person's own chats and teammates' chats, in Pinned
// (where it carries its kind's icon), in a project (indented under it), in
// Chats and in search results. What a status means reaches a screen reader as
// the row's description; it never changes the row's name, which is the title.
function ChatRow({
  conversation,
  own,
  icon,
  active,
  focused,
  editing = false,
  selecting = false,
  checked = false,
  status,
  showStatus = true,
  now,
  onOpen,
  onToggleSelect,
  onCommitRename,
  onCancelRename,
  kebab,
  onDragStart,
  titleNode,
  context,
  testId,
}: {
  conversation: ConversationSummary;
  // The person's own chat: it can be renamed, selected and walked with j/k.
  own: boolean;
  // Pinned rows carry their kind's icon; rows in Chats and in a project do not.
  icon?: string;
  active: boolean;
  // focused = the keyboard j/k cursor is on this row (distinct from active =
  // the open conversation). Renders a ring so the cursor is visible.
  focused?: boolean;
  editing?: boolean;
  selecting?: boolean;
  checked?: boolean;
  // What the row has to say for itself. `showStatus` is false when the
  // person turned row statuses off (docs/SIDEBAR.md): the mark is not drawn,
  // and its sentence still reaches a screen reader.
  status: RowStatusInput;
  showStatus?: boolean;
  now: number;
  onOpen: () => void;
  onToggleSelect?: () => void;
  onCommitRename?: (title: string) => void;
  onCancelRename?: () => void;
  kebab: ReactNode;
  // When set, the row is draggable (HTML5 DnD) — the drag-a-chat-into-a-
  // project affordance. Mouse only by nature; the menu's "Move to project"
  // is the touch/keyboard path to the same action.
  onDragStart?: (e: React.DragEvent<HTMLDivElement>) => void;
  // Search results only: the title with the matched words marked, and the
  // place the chat lives (its project's name). A result is shown out of its
  // section, so the row says where it belongs.
  titleNode?: ReactNode;
  context?: string;
  testId?: string;
}) {
  // Rename starts from the menu, so the field has to take focus itself or the
  // rename is unusable. Gated on `editing` (the field only renders then).
  // onFocus still selects the text.
  const renameInputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (editing) renameInputRef.current?.focus();
  }, [editing]);

  const description = describeRowStatus(status, now);
  const descId = `rail-row-desc-${conversation.id}`;
  const labels = conversation.labels ?? [];
  const tooltip = [
    conversation.title,
    !own && conversation.user_email ? `(${conversation.user_email})` : "",
    labels.length > 0 ? `· ${labels.join(", ")}` : "",
  ]
    .filter(Boolean)
    .join(" ");

  return (
    <div
      data-conversation-id={conversation.id}
      data-own-row={own ? "" : undefined}
      data-focused={focused ? "true" : undefined}
      draggable={Boolean(onDragStart)}
      onDragStart={onDragStart}
      className={[
        "group relative rounded-[var(--radius-md)] transition-colors",
        selecting && checked
          ? "bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)]"
          : active
            ? "bg-[var(--rail-active)]"
            : "hover:bg-[var(--rail-hover)]",
        focused ? "ring-1 ring-inset ring-[var(--color-accent)]" : "",
      ].join(" ")}
    >
      {description ? (
        <span id={descId} hidden>
          {description}
        </span>
      ) : null}
      {editing ? (
        <input
          // Uncontrolled: remounts each time editing starts (it only renders
          // while editing), so defaultValue tracks the live title without a
          // sync effect.
          ref={renameInputRef}
          aria-label={tr("Rename {0}", [conversation.title])}
          className={RENAME_INPUT_CLASS}
          defaultValue={conversation.title}
          onFocus={(e) => e.currentTarget.select()}
          onBlur={(e) => onCommitRename?.(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              onCommitRename?.(e.currentTarget.value);
            } else if (e.key === "Escape") {
              e.preventDefault();
              onCancelRename?.();
            }
          }}
        />
      ) : (
        <button
          type="button"
          aria-pressed={selecting ? checked : undefined}
          aria-describedby={description ? descId : undefined}
          data-rail-row=""
          data-testid={testId}
          className={[
            "flex min-h-[var(--rail-row-h)] w-full min-w-0 items-center gap-2.5 rounded-[var(--radius-md)] pl-2.5 pr-2.5 text-left text-[length:var(--rail-row-text)] leading-tight transition-colors focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11",
            selecting ? "" : ROW_CLEAR_FOR_ACTIONS,
            active || (selecting && checked)
              ? "font-medium text-[var(--color-text-primary)]"
              : "text-[var(--color-text-secondary)] group-hover:text-[var(--color-text-primary)]",
          ].join(" ")}
          onClick={selecting && onToggleSelect ? onToggleSelect : onOpen}
          title={tooltip}
        >
          {/* Leading checkbox — select mode only: a 1rem square that fills
              primary with a check when on. */}
          {selecting ? (
            <span
              aria-hidden="true"
              className={[
                "inline-flex size-4 shrink-0 items-center justify-center rounded-[0.3rem] border-[1.5px] transition",
                checked
                  ? "border-[var(--color-primary-fill)] bg-[var(--color-primary-fill)]"
                  : "border-[var(--color-border-strong)]",
              ].join(" ")}
            >
              {checked ? <Icon name="check" className="size-[0.7rem] text-[var(--color-on-primary-fill)]" /> : null}
            </span>
          ) : icon ? (
            <Icon name={icon} className={railIconClass(active)} />
          ) : null}
          <span className="block min-w-0 flex-1 truncate">
            {context ? <span className="text-[var(--color-text-muted)]">{context} › </span> : null}
            {titleNode ?? conversation.title}
          </span>
          {showStatus ? (
            <span className={["ml-auto inline-flex shrink-0", selecting ? "" : STATUS_YIELDS_TO_ACTIONS].join(" ")}>
              <RowStatus status={status} now={now} />
            </span>
          ) : null}
        </button>
      )}

      {!editing && !selecting ? (
        <div className="absolute inset-y-0 right-1 flex items-center">{kebab}</div>
      ) : null}
    </div>
  );
}

// ── Search results (the rail's unified search) ──────────────────────────────
// A small pill naming what a result is or where it lives ("Patrol",
// "Archived"): a result is shown out of its section, so it says so itself.
function ResultTag({ children }: { children: ReactNode }) {
  return (
    <span className="shrink-0 rounded-full border border-[var(--color-border)] px-1.5 py-px text-[0.62rem] font-medium text-[var(--color-text-muted)]">
      {children}
    </span>
  );
}

// The matched words of a result's title.
const MARK_CLASS =
  "rounded-[2px] bg-[color-mix(in_srgb,var(--color-accent)_25%,transparent)] text-[var(--color-text-primary)]";

function markMatches(text: string, tokens: readonly string[]): ReactNode {
  const ranges = searchMatchRanges(text, tokens);
  if (ranges.length === 0) return text;
  const nodes: ReactNode[] = [];
  let last = 0;
  for (const [start, end] of ranges) {
    if (start > last) nodes.push(text.slice(last, start));
    nodes.push(
      <mark key={start} className={MARK_CLASS}>
        {text.slice(start, end)}
      </mark>,
    );
    last = end;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}

// One group of results, under the name of the sidebar section its rows come
// from. When the person has hidden that section, the group says so — the
// reason these chats are nowhere else in the rail — and offers to bring the
// section back (to an account that may change its sidebar).
function SearchGroup({
  testId,
  title,
  count,
  hidden,
  onShow,
  children,
}: {
  testId: string;
  title: string;
  count: number;
  hidden?: boolean;
  onShow?: () => void;
  children: ReactNode;
}) {
  return (
    <section data-testid={testId} aria-label={title} className="mb-2">
      <div className="flex items-center gap-1.5 px-2 pb-1 pt-1.5 text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
        <span className="min-w-0 truncate">{title}</span>
        <span className="font-[family-name:var(--font-code)] font-normal tabular-nums">{count}</span>
      </div>
      {hidden ? (
        <p
          data-testid="search-hidden-note"
          className="mx-2 mb-1 flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.72rem] text-[var(--color-text-secondary)]"
        >
          <span className="min-w-0 flex-1">{tr("Hidden in your sidebar. Search still finds what is in it.")}</span>
          {onShow ? (
            <button
              type="button"
              className="shrink-0 rounded px-1 font-semibold text-[var(--color-accent)] underline decoration-dotted underline-offset-2 transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
              aria-label={tr("Show {0} in the sidebar", [title])}
              onClick={onShow}
            >
              {tr("Show it")}
            </button>
          ) : null}
        </p>
      ) : null}
      {children}
    </section>
  );
}

// ── Bulk bar action button (the design's .bulk-btn) ──────────────────────────
const BULK_BTN_CLASS =
  "relative inline-flex size-[1.9rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition enabled:hover:bg-[var(--rail-hover)] enabled:hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-40";


// ── Section heading ──────────────────────────────────────────────────────────
// "Pinned", "Projects", "Chats": a quiet line of text, with the section's one
// control at its right edge. The label folds the section when clicked. Its
// chevron is drawn where folding is something people do (Projects, a tree
// that gets long) and wherever a section is folded, so there is always a
// visible way back; elsewhere it shows on hover and focus.
function SectionHeading({
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
function ChatsMenu({
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
function NewMenu({
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

// How many projects the Projects section lists before "All N projects".
const PROJECTS_SHOWN = 8;

// What a platform's search shortcut says about its new-chat chord.
function newChatChord(searchShortcut: string): string {
  return searchShortcut.startsWith("⌘") ? "⌘⇧O" : "Ctrl+Shift+O";
}

// ── Main sidebar ─────────────────────────────────────────────────────────────
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
  streamingConvs,
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
  selectMode,
  selectedIds,
  onToggleSelection,
  onEnterSelectMode,
  onExitSelectMode,
  onBulkDelete,
  onBulkPin,
  onBulkAddLabel,
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
  streamingConvs: Set<string>;
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
  // above the account menu; Escape/Cancel exit.
  selectMode: boolean;
  selectedIds: Set<string>;
  onToggleSelection: (id: string) => void;
  onEnterSelectMode: (id?: string) => void;
  onExitSelectMode: () => void;
  onBulkDelete: () => void;
  onBulkPin: () => void;
  onBulkAddLabel: (label: string) => void;
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
  const [bulkPanel, setBulkPanel] = useState<"none" | "labels">("none");
  const bulkLabelsRef = useRef<HTMLButtonElement | null>(null);

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
  const largeSelection = selectedIds.size > 50;
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
    pin: (kind, ref) => {
      if (readOnly) return false;
      if (kind === "project") {
        const project = projects.find((p) => p.id === ref);
        if (!project || project.pinned || (project.owner_email ?? "").trim().toLowerCase() !== mineEmail) return false;
        onPinProject(ref, true);
        return true;
      }
      const own = conversations.find((c) => c.id === ref);
      const chat = own ?? (canPinTeamChats ? teamSharedConversations.find((c) => c.id === ref) : undefined);
      // A project's chat lives under its project and is never pinned.
      if (!chat || chat.pinned || (own && own.project_id && knownProjectIds?.has(own.project_id))) return false;
      void togglePin(chat);
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

  // ── Select mode: the bar above the account menu ───────────────────────────
  // Count + icon actions with data-tip-top tooltips. Add-label reuses the
  // menu's panel (including its inline create input) in a menu that opens
  // above the bar. Actions disable at zero selected.
  const bulkBar = selecting ? (
    <div className={railCollapsed ? "sm:hidden" : ""}>
      <div className="mb-1.5 flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-[0.4rem] pl-[0.65rem] pr-[0.3rem] motion-safe:animate-pop-up">
        <span className="min-w-0 flex-1 text-[0.8rem] font-medium tabular-nums text-[var(--color-text-primary)]">
          {rich("{0} selected", selectedIds.size)}
        </span>
        <div className="flex items-center gap-[0.15rem]">
          <button
            type="button"
            aria-label={tr("Pin selected")}
            data-tip-top="Pin"
            disabled={selectedIds.size === 0}
            className={BULK_BTN_CLASS}
            onClick={onBulkPin}
          >
            <Icon name="pin" className="size-[0.95rem]" />
          </button>
          <button
            ref={bulkLabelsRef}
            type="button"
            aria-label={tr("Add label")}
            aria-haspopup="menu"
            aria-expanded={bulkPanel === "labels"}
            data-tip-top="Add label"
            disabled={selectedIds.size === 0}
            className={BULK_BTN_CLASS}
            onClick={() => setBulkPanel((p) => (p === "labels" ? "none" : "labels"))}
          >
            <Icon name="tag" className="size-[0.95rem]" />
          </button>
          <button
            type="button"
            aria-label={tr("Delete selected")}
            data-tip-top="Delete"
            disabled={selectedIds.size === 0}
            className={[
              BULK_BTN_CLASS,
              "enabled:hover:bg-[var(--color-status-error-bg)] enabled:hover:text-[var(--color-status-error-fg)]",
            ].join(" ")}
            onClick={onBulkDelete}
          >
            <Icon name="trash" className="size-[0.95rem]" />
          </button>
          <button
            type="button"
            aria-label={tr("Exit selection")}
            data-tip-top="Cancel"
            className={BULK_BTN_CLASS}
            onClick={onExitSelectMode}
          >
            <Icon name="close" className="size-[0.95rem]" />
          </button>
        </div>
        <Menu
          open={bulkPanel === "labels"}
          onClose={() => setBulkPanel("none")}
          anchorRef={bulkLabelsRef}
          placement="top-end"
          label={tr("Add label to selected")}
        >
          <LabelsPanel
            current={[]}
            suggestions={allLabelNames}
            onAdd={(label) => {
              onBulkAddLabel(label);
              setBulkPanel("none");
            }}
          />
        </Menu>
      </div>
      {largeSelection ? (
        <p className="mb-1.5 text-[0.6875rem] text-[var(--color-danger)]">
          {rich("Selecting {0} conversations — large bulk deletes are permanent.", selectedIds.size)}
        </p>
      ) : null}
    </div>
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
