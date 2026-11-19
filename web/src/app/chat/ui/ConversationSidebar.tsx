import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
// Conversation rail (#169 unified shell, #258/#279 organization). This renders
// the chat surface's content inside the shared NavRail: the new-chat / sealed
// affordances, the unified search, the projects tree, the organized
// conversation list (Pinned · Labels · Temporary) with a per-row kebab menu,
// label filtering, and the collapsible Archived section. The footer (update banner +
// delete-all-unpinned) is handed to NavRail; sign-out moved into the rail's
// account menu.
//
// It stays purely presentational + event-forwarding: conversation state and the
// mutation handlers live in ChatExperience and are threaded in via props. The
// per-row kebab and the account menu share one Menu surface, so they read as one
// component family. Per-row accessible names ("Pin/Unpin/Archive/Unarchive/
// Delete/Download <title>") are preserved verbatim — they now live inside the
// kebab menu, and the live conversation-mgmt e2e opens the kebab to reach them.

import { useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { NavRail, type RailCollapse } from "@/app/shared/ui/NavRail";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Icon } from "./Icon";
import {
  MAX_CHAT_ORDER,
  MAX_LABELS,
  MAX_LABEL_LEN,
  PINNED_ORDER_SCOPE,
  RECENT_SORTS,
  isRecentSort,
  projectOrderScope,
  sortRecent,
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
import { expiresInDays, nearCap, retentionPolicy, useNowSeconds, type RetentionPolicy } from "./chatRetention";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { teamLinkUrl } from "./teamSharing";
import { copyText } from "@/app/shared/lib/clipboard";
import { ChatReorderList } from "./ChatReorderList";
import { AddToSidebarItem } from "@/app/shared/sidebar/AddToSidebarItem";
import { defaultIcon, sectionOf, updateSection, type SectionId } from "@/app/shared/sidebar/layout";
import { updateSidebarLayout, useSidebar } from "@/app/shared/sidebar/store";
import { SidebarTargetsContext, usePatrolIds, type SidebarTargets } from "@/app/shared/sidebar/targets";
import { useCanCustomize } from "@/app/shared/sidebar/useCanCustomize";

// ── Sealed-chat button + explainer tooltip ───────────────────────────────────
// The icon-only "new sealed chat" lock lives in the Temporary section's
// heading row (it starts a sealed chat, which lands in that list) and
// explains what a sealed chat is on hover AND keyboard focus, using the
// design's .conv-tooltip surface. The tooltip portals to <body> because the
// rail's transform/backdrop-filter would otherwise capture its fixed
// positioning.
const SEALED_TOOLTIP_TEXT =
  tr("New sealed chat — locked down and private. Your data and an approved model stay inside this sandbox; nothing leaves.");

// PortalTipIconButton is a small icon button whose hover/focus tooltip
// portals to <body> — the rail's overflow containers and transform would
// otherwise clip or capture it (a CSS data-tip's absolutely-positioned
// ::after also OCCUPIES layout even at opacity 0, which put a horizontal
// scrollbar on the projects scroller; the portal has no such footprint).
// A row's options (⋯) button: revealed on hover or keyboard focus where
// there is a mouse, always shown on a touch screen, which has no hover to
// reveal it with (a phone user otherwise had to know where to tap).
const ROW_ACTIONS_REVEAL =
  "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 any-pointer-coarse:opacity-100";

function usePortalTip(tip: string) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    // Below the button, arrow pointing back up at it (the design's conv-info
    // tooltip placement: bottom + 7, offset so the arrow lands on the anchor).
    setPos({
      top: Math.round(r.bottom + 7),
      left: Math.round(r.left + r.width / 2 - 17),
    });
  };
  const hide = () => setPos(null);
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

function PortalTipIconButton({
  tip,
  ariaLabel,
  icon,
  iconClassName,
  className,
  onClick,
}: {
  tip: string;
  ariaLabel: string;
  icon: string;
  iconClassName?: string;
  className?: string;
  onClick: () => void;
}) {
  const { show, hide, tipNode } = usePortalTip(tip);
  return (
    <button
      type="button"
      className={className}
      aria-label={ariaLabel}
      onClick={onClick}
      onMouseEnter={show}
      onMouseLeave={hide}
      onFocus={show}
      onBlur={hide}
    >
      <Icon name={icon} className={iconClassName} />
      {tipNode}
    </button>
  );
}

function SealedNewChatButton({ onClick }: { onClick: () => void }) {
  return (
    <PortalTipIconButton
      tip={SEALED_TOOLTIP_TEXT}
      ariaLabel={tr("New sealed chat — sandboxed, vetted model, nothing leaves")}
      icon="lock"
      iconClassName="size-4"
      className="ml-auto inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-secondary)] transition motion-safe:hover:scale-110 hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
      onClick={onClick}
    />
  );
}

// ── Temporary-section info note ──────────────────────────────────────────────
// The ⚠ next to the "Temporary" group header shows the retention explainer on
// hover and keyboard focus, on the shared .conv-tooltip surface (portaled to
// <body> like the sealed tooltip above, and for the same reason). On touch —
// no hover — the focus a tap gives the button is what shows it. The copy
// states this deployment's retention period and how long before the end a
// chat is flagged (both from the server config), and the keep actions:
// pinning, filing into a project and archiving all exempt a chat from the
// sweep (docs/CHAT-RETENTION.md).
const KEEP_A_CHAT = () => tr("Pin a chat, move it into a project or archive it to keep it.");
const recentInfoText = (policy: RetentionPolicy) =>
  [
    policy.warningDays > 0
      ? tr("Chats here are temporary: each is deleted after {0} days without activity, and shows the days it has left as that approaches.", [policy.ttlDays])
      : tr("Chats here are temporary: each is deleted after {0} days without activity.", [policy.ttlDays]),
    KEEP_A_CHAT(),
  ].join(" ");

// The one-list view (docs/SIDEBAR.md) mixes the person's temporary chats with
// teammates' chats, so "chats here are deleted" would be wrong for half the
// rows: there the hourglass on a row is what marks a temporary chat, and the
// explainer says what the hourglass means.
const unifiedInfoText = (policy: RetentionPolicy) =>
  [
    policy.warningDays > 0
      ? tr("Chats marked with an hourglass are temporary: each is deleted after {0} days without activity, and shows the days it has left as that approaches.", [policy.ttlDays])
      : tr("Chats marked with an hourglass are temporary: each is deleted after {0} days without activity.", [policy.ttlDays]),
    KEEP_A_CHAT(),
    tr("A teammate's chat is theirs to keep."),
  ].join(" ");
// A patrol chat's mark: the icon a patrol has when it is pinned to the rail
// (layout.defaultIcon), so a patrol looks the same wherever it is listed.
const PATROL_ICON = defaultIcon({ id: "", kind: "chat" }, true);
const PATROL_ROW_TEXT = tr("Patrol — this chat checks in on a schedule");

// The row marks below (patrol, temporary) never change a row's accessible
// name: the row is still found, and read, by its title. The mark is a
// tooltip for sighted users, and the same sentence reaches a screen reader
// as the row's description (aria-describedby to one hidden copy per rail).
const PATROL_DESC_ID = "rail-row-desc-patrol";
const TEMPORARY_DESC_ID = "rail-row-desc-temporary";

function PatrolMark() {
  return (
    <span data-testid="patrol-mark" aria-hidden="true" title={PATROL_ROW_TEXT}>
      <Icon name={PATROL_ICON} className="size-3 shrink-0 text-[var(--color-accent)]" />
    </span>
  );
}
const temporaryRowText = (days: number) =>
  tr("Temporary — deleted after {0} days without activity. Pin it, move it into a project or archive it to keep it.", [days]);
// The same row once it is inside the warning window: how long it has left,
// and that using it is enough to keep it.
const expiringRowText = (daysLeft: number) =>
  daysLeft <= 1
    ? tr("Temporary — will be deleted within a day. Send a message in it, pin it, move it into a project or archive it to keep it.")
    : tr("Temporary — will be deleted in {0} days. Send a message in it, pin it, move it into a project or archive it to keep it.", [daysLeft]);
const expiryDescId = (conversationId: string) => `rail-row-desc-expiring-${conversationId}`;

// ── Temporary-chat notices ───────────────────────────────────────────────────
// Under the Recent / Temporary heading, only when there is something to say:
// how many of the person's chats are about to be deleted (a button that takes
// them to the first one), and that they are close to the most temporary chats
// one person keeps. Silent otherwise — a standing notice is one people stop
// reading (docs/CHAT-RETENTION.md).
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
    <div className="mx-2 mb-1 grid gap-1 text-[0.75rem] leading-snug">
      {expiringCount > 0 ? (
        <button
          type="button"
          data-testid="expiring-chats-notice"
          className="flex w-full items-start gap-1.5 rounded-md border border-[var(--color-warning-border)] px-2 py-1.5 text-left text-[var(--color-warning-text)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          title={tr("Show the first of them")}
          onClick={onShowFirst}
        >
          <Icon name="hourglass" className="mt-[0.15rem] size-3 shrink-0" />
          <span className="min-w-0">
            {expiringCount === 1
              ? tr("1 chat will be deleted soon.")
              : tr("{0} chats will be deleted soon.", [expiringCount])}{" "}
            {tr("Pin, file or archive a chat to keep it.")}
          </span>
        </button>
      ) : null}
      {capNear && cap ? (
        <p
          data-testid="temporary-cap-notice"
          role="status"
          className="rounded-md border border-[var(--color-border)] px-2 py-1.5 text-[var(--color-text-secondary)]"
        >
          {tr("You have {0} temporary chats. Past {1}, the least recently active are deleted first.", [temporaryCount, cap])}
        </p>
      ) : null}
    </div>
  );
}

function RecentInfoButton({ text, label = tr("About temporary chats") }: { text: string; label?: string }) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  // Hover AND keyboard focus show the explainer (the sealed-lock tooltip's
  // exact mechanics — was click-toggle, which read as a broken button). The
  // trade-off is touch: with no hover, a tap (focus) is what shows it.
  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    // Same placement math as the sealed tooltip: below the trigger, shifted
    // so the surface's arrow points back at the anchor.
    setPos({
      top: Math.round(r.bottom + 7),
      left: Math.round(r.left + r.width / 2 - 17),
    });
  };
  const hide = () => setPos(null);

  return (
    <span className="relative inline-flex">
      <button
        type="button"
        aria-label={label}
        className="hit-area inline-flex items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        onMouseEnter={show}
        onMouseLeave={hide}
        onFocus={show}
        onBlur={hide}
      >
        <Icon name="info" className="size-[0.9rem]" />
      </button>
      {pos && typeof document !== "undefined"
        ? createPortal(
            <span
              role="tooltip"
              className="conv-tooltip"
              style={{ top: pos.top, left: pos.left }}
            >
              {text}
            </span>,
            document.body,
          )
        : null}
    </span>
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

// ── Per-project kebab menu (rail Projects section) ───────────────────────────
// Decision #23: the hover menu on a project is Open, New chat, Share, Project
// settings. Share and Project settings are the owner's (only the owner can
// edit, share, or delete — B26); both open the project's settings dialog,
// where "Who can see it" lives. The owner's list-keeping items (Pin, Rename,
// Delete project) stay below a separator: the rail is still the only place a
// project is pinned or renamed inline.
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
}: {
  projectId: string;
  projectName: string;
  pinned: boolean;
  isOwner: boolean;
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
  const tip = usePortalTip(tr("Options"));
  const item = (fn: () => void) => () => {
    close();
    fn();
  };
  // Pinning to the sidebar is the viewer's own (docs/SIDEBAR.md), so every
  // member gets it, beside the owner's list-keeping items.
  const sidebarItem = (
    <AddToSidebarItem target={{ kind: "project", ref: projectId, title: projectName }} onDone={close} />
  );
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={tr("Project options for {0}", [projectName])}
        data-project-options={projectId}
        onMouseEnter={(e) => {
          if (!open) tip.show(e);
        }}
        onMouseLeave={tip.hide}
        onFocus={(e) => {
          if (!open) tip.show(e);
        }}
        onBlur={tip.hide}
        className={[
          "hit-area hit-area-touch pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open
            ? "opacity-100"
            : ROW_ACTIONS_REVEAL,
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          tip.hide();
          setOpen((o) => !o);
        }}
      >
        <Icon name="dots" className="size-[1.1rem]" />
        {tip.tipNode}
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for project {0}", [projectName])}
        className="min-w-[11rem]"
      >
        <MenuItem
          icon={<Icon name="external-link" className="size-4" />}
          onClick={item(onOpen)}
        >
          {tr("Open project")}
        </MenuItem>
        <MenuItem
          icon={<Icon name="plus" className="size-4" />}
          onClick={item(onNewChat)}
        >
          {tr("New chat")}
        </MenuItem>
        {onReorder ? (
          <MenuItem
            icon={<Icon name="selector" className="size-4" />}
            onClick={item(onReorder)}
          >
            {tr("Reorder chats")}
          </MenuItem>
        ) : null}
        {isOwner ? (
          <>
            {/* The TEAM glyph, not the chain link: the chain link means "by
                URL" and must not read as "my team can see this"
                (ShareGlyphs.tsx). */}
            <MenuItem
              icon={<TeamGlyph className="size-4" />}
              onClick={item(onShare)}
            >
              {tr("Share…")}
            </MenuItem>
            <MenuItem
              icon={<Icon name="settings" className="size-4" />}
              onClick={item(onEdit)}
            >
              {tr("Project settings…")}
            </MenuItem>
            <MenuSeparator />
            <MenuItem
              icon={<Icon name="pin" className="size-4" />}
              onClick={item(onPin)}
            >
              {pinned ? tr("Unpin") : tr("Pin")}
            </MenuItem>
            {sidebarItem}
            <MenuItem
              icon={<Icon name="edit" className="size-4" />}
              onClick={item(onRename)}
            >
              {tr("Rename")}
            </MenuItem>
            <MenuItem
              danger
              icon={<Icon name="trash" className="size-4" />}
              onClick={item(onDelete)}
            >
              {tr("Delete project")}
            </MenuItem>
          </>
        ) : (
          <>
            <MenuSeparator />
            {sidebarItem}
          </>
        )}
      </Menu>
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

// ── Per-row kebab menu ───────────────────────────────────────────────────────
function ConversationKebab({
  conversation,
  allLabelNames,
  projects,
  onPin,
  onRename,
  onDownload,
  onPromote,
  onSavePrompt,
  onSetLabels,
  onSetProject,
  onShare,
  onTransfer,
  isShared,
  onSelect,
  onArchive,
  onDelete,
}: {
  conversation: ConversationSummary;
  allLabelNames: string[];
  projects: Project[];
  onPin: () => void;
  onRename: () => void;
  onDownload: () => void;
  onPromote: () => void;
  onSavePrompt: () => void;
  onSetLabels: (labels: string[]) => void;
  onSetProject: (projectID: string | null) => void;
  // Opens the Share dialog (both scopes). It no longer mints a link as a
  // side effect of opening: creating one is a click inside the dialog.
  onShare: () => void;
  // Opens the Transfer ownership dialog (docs/CHAT-TRANSFER.md).
  onTransfer: () => void;
  isShared: boolean;
  onSelect: () => void;
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
  const toggleFlyout = (
    which: "labels" | "project",
    el: HTMLElement,
  ) => {
    flyoutAnchorRef.current = el;
    setFlyout((cur) => (cur === which ? null : which));
  };
  const labels = conversation.labels ?? [];
  const tip = usePortalTip(tr("Options"));
  const caret = (
    <span
      aria-hidden="true"
      className="text-[0.62rem] text-[var(--color-text-muted)]"
    >
      ▸
    </span>
  );
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
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={tr("Conversation options for {0}", [conversation.title])}
        onMouseEnter={(e) => {
          if (!open) tip.show(e);
        }}
        onMouseLeave={tip.hide}
        onFocus={(e) => {
          if (!open) tip.show(e);
        }}
        onBlur={tip.hide}
        className={[
          // Fixed, centered, padded square (~1.8rem) per the handoff .conv-kebab —
          // a rounded hover highlight around the centered icon, not hugging it.
          // hit-area extends the clickable box to ~2rem without growing the fill.
          "hit-area hit-area-touch pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open
            ? "opacity-100"
            : ROW_ACTIONS_REVEAL,
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          tip.hide();
          setFlyout(null);
          setOpen((o) => !o);
        }}
      >
        <Icon name="dots" className="size-[1.1rem]" />
        {tip.tipNode}
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for {0}", [conversation.title])}
        className="min-w-[12rem]"
        flyout={flyoutContent}
        flyoutOpen={flyout !== null}
        flyoutAnchorRef={flyoutAnchorRef}
        onFlyoutClose={() => setFlyout(null)}
        flyoutLabel={flyout === "project" ? tr("Move to project") : tr("Labels")}
      >
        {/* Pinning is meaningless for a project chat: it lives only under
            its project (never in Pinned) and is already retention-exempt —
            filing even clears the flag server-side. Hide rather than no-op. */}
        {conversation.project_id ? null : (
          <MenuItem
            icon={<Icon name="pin" className="size-4" />}
            onClick={() => {
              onPin();
              close();
            }}
          >
            {conversation.pinned ? tr("Unpin") : tr("Pin")}
          </MenuItem>
        )}
        <MenuItem
          icon={<Icon name="edit" className="size-4" />}
          onClick={() => {
            close();
            onRename();
          }}
        >
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
        <AddToSidebarItem
          target={{ kind: "chat", ref: conversation.id, title: conversation.title }}
          onDone={close}
        />
        <MenuSeparator />
        {/* Was "Download as JSON" — a label that named a file format instead
            of an outcome, and offered the one format a non-technical reader
            can do the least with. The dialog it opens picks the artifact. */}
        <MenuItem
          icon={<Icon name="download" className="size-4" />}
          onClick={() => {
            onDownload();
            close();
          }}
        >
          {tr("Download chat…")}
        </MenuItem>
        <MenuItem
          icon={<Icon name="clock" className="size-4" />}
          onClick={() => {
            onPromote();
            close();
          }}
        >
          {tr("Make recurring task…")}
        </MenuItem>
        <MenuItem
          icon={<Icon name="book" className="size-4" />}
          onClick={() => {
            onSavePrompt();
            close();
          }}
        >
          {tr("Save as workflow…")}
        </MenuItem>
        {/* One entry, one dialog, two audiences (ADR-0057). The menu used to
            fork into "Share" / "Share link… + Stop sharing" depending on
            whether a public link existed, which made the LINK the whole
            meaning of sharing. The dialog now owns both scopes — share by
            link, and share with the team — so every surface that says a chat
            is shared also lets you change it. */}
        <MenuItem
          // The glyph names the scope in play when only one is: a chat shared
          // with the team but not by link gets the team mark, everything else
          // the link mark (the dialog holds both either way).
          icon={
            !isShared && conversation.team_visible ? (
              <TeamGlyph className="size-4" />
            ) : (
              <ShareGlyph className="size-4" />
            )
          }
          onClick={() => {
            onShare();
            close();
          }}
        >
          {tr("Share…")}
        </MenuItem>
        {/* An archived or Lockdown chat cannot change hands (the server
            refuses it), so the item is not offered on one. */}
        {conversation.lockdown || conversation.archived_at ? null : (
          <MenuItem
            icon={<Icon name="arrow-right" className="size-4" />}
            onClick={() => {
              onTransfer();
              close();
            }}
          >
            {tr("Transfer ownership…")}
          </MenuItem>
        )}
        <MenuItem
          icon={<Icon name="check-square" className="size-4" />}
          onClick={() => {
            onSelect();
            close();
          }}
        >
          {tr("Select…")}
        </MenuItem>
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="archive" className="size-4" />}
          onClick={() => {
            onArchive();
            close();
          }}
        >
          {tr("Archive")}
        </MenuItem>
        <MenuItem
          danger
          icon={<Icon name="trash" className="size-4" />}
          onClick={() => {
            onDelete();
            close();
          }}
        >
          {tr("Delete")}
        </MenuItem>
      </Menu>
    </>
  );
}

// ── Team chats row options ──────────────────────────────────────────────────
// A teammate's chat is not the viewer's to rename, file, archive or delete, so
// its menu holds what any member can do with it: pin it to their own sidebar,
// and copy a link that opens it. The link is the one that works for this row:
// the team link (/chat?team=<id>, the link Share hands out) for a chat shared
// with the team, the chat link (/chat?c=<id>) for one reached only through
// Multiplayer (a team link refuses it), and none otherwise.
function teamChatLink(c: ConversationSummary): string | null {
  if (c.team_visible) return teamLinkUrl(c.id);
  if (c.multiplayer && typeof window !== "undefined") {
    return `${window.location.origin}/chat?c=${encodeURIComponent(c.id)}`;
  }
  return null;
}

function TeamChatKebab({
  conversation,
  onTransfer,
}: {
  conversation: ConversationSummary;
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
  const tip = usePortalTip(tr("Options"));
  const link = teamChatLink(conversation);
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={tr("Conversation options for {0}", [conversation.title])}
        data-testid="team-chat-options"
        onMouseEnter={(e) => {
          if (!open) tip.show(e);
        }}
        onMouseLeave={tip.hide}
        onFocus={(e) => {
          if (!open) tip.show(e);
        }}
        onBlur={tip.hide}
        className={[
          "hit-area hit-area-touch pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open ? "opacity-100" : ROW_ACTIONS_REVEAL,
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          tip.hide();
          if (open) close();
          else setOpen(true);
        }}
      >
        <Icon name="dots" className="size-[1.1rem]" />
        {tip.tipNode}
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={tr("Options for {0}", [conversation.title])}
        className="min-w-[12rem]"
      >
        <AddToSidebarItem
          target={{ kind: "chat", ref: conversation.id, title: conversation.title }}
          onDone={close}
        />
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

// ── Conversation row ─────────────────────────────────────────────────────────
function ConvRow({
  conversation,
  active,
  focused,
  streaming,
  editing,
  selecting,
  checked,
  onOpen,
  onToggleSelect,
  onCommitRename,
  onCancelRename,
  kebab,
  onDragStart,
  titleNode,
  context,
  patrol,
  temporary,
  expiresIn,
}: {
  conversation: ConversationSummary;
  // The chat is temporary AND inside the warning window: whole days until the
  // sweep deletes it. The hourglass turns to the warning colour and carries
  // the number, in every list. `temporary` is then the sentence that says so.
  expiresIn?: number;
  // The one-list view only: this chat is temporary (the retention sweep
  // would delete it), marked with an hourglass whose tooltip is this text:
  // that it is temporary, after how long it goes, and how to keep it.
  temporary?: string;
  // Search results only: the title with the matched words marked, the place
  // the chat lives (its project's name). A result is shown out of its
  // section, so the row says where it belongs.
  titleNode?: ReactNode;
  context?: string;
  // The chat has a patrol (armed or paused): marked wherever it is listed.
  patrol?: boolean;
  active: boolean;
  // focused = the keyboard j/k cursor is on this row (distinct from active =
  // the open conversation). Renders a ring so the cursor is visible.
  focused: boolean;
  streaming: boolean;
  editing: boolean;
  selecting: boolean;
  checked: boolean;
  onOpen: () => void;
  onToggleSelect: () => void;
  onCommitRename: (title: string) => void;
  onCancelRename: () => void;
  kebab: ReactNode;
  // When set, the row is draggable (HTML5 DnD) — the rail's drag-a-chat-into-
  // a-project affordance. Mouse only by nature; the kebab's "Move to project"
  // is the touch/keyboard path to the same action.
  onDragStart?: (e: React.DragEvent<HTMLDivElement>) => void;
}) {
  const labels = conversation.labels ?? [];
  const shown = labels.slice(0, 2);
  const extra = labels.length - shown.length;
  // Rename starts from the kebab, so the field has to take focus itself or the
  // rename is unusable — dropping autoFocus without a replacement would be a
  // real regression, not a lint win. Gated on `editing` (the field only renders
  // then, so this is exactly the old mount-time behavior with an explicit
  // cause). onFocus still selects the text, so focusing programmatically
  // preserves the select-all.
  const renameInputRef = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (editing) renameInputRef.current?.focus();
  }, [editing]);

  return (
    <div
      data-conversation-id={conversation.id}
      data-focused={focused ? "true" : undefined}
      draggable={Boolean(onDragStart)}
      onDragStart={onDragStart}
      className={[
        "group relative rounded-md transition",
        selecting && checked
          ? "bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)]"
          : active
            ? "bg-[var(--rail-active)]"
            : "hover:bg-[var(--rail-hover)]",
        focused ? "ring-1 ring-inset ring-[var(--color-accent)]" : "",
      ].join(" ")}
    >
      {/* The expiry sentence differs per row (days left), so each expiring
          row carries its own hidden copy for aria-describedby. */}
      {temporary && expiresIn !== undefined ? (
        <span id={expiryDescId(conversation.id)} hidden>
          {temporary}
        </span>
      ) : null}
      {editing ? (
        <input
          // Uncontrolled: remounts each time editing starts (it only renders
          // while editing), so defaultValue tracks the live title without a
          // sync effect. The focus effect above + select-on-focus mirror the
          // prior behavior.
          ref={renameInputRef}
          aria-label={tr("Rename {0}", [conversation.title])}
          className="mx-[0.4rem] my-[0.32rem] w-[calc(100%-0.8rem)] rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1 text-[0.875rem] text-[var(--color-text-primary)] outline-none"
          defaultValue={conversation.title}
          onFocus={(e) => e.currentTarget.select()}
          onBlur={(e) => onCommitRename(e.currentTarget.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              onCommitRename(e.currentTarget.value);
            } else if (e.key === "Escape") {
              e.preventDefault();
              onCancelRename();
            }
          }}
        />
      ) : (
        <button
          type="button"
          aria-pressed={selecting ? checked : undefined}
          aria-describedby={
            [
              temporary ? (expiresIn === undefined ? TEMPORARY_DESC_ID : expiryDescId(conversation.id)) : "",
              patrol ? PATROL_DESC_ID : "",
            ]
              .filter(Boolean)
              .join(" ") || undefined
          }
          data-rail-row=""
          className={[
            "block w-full min-w-0 rounded-md py-2 pl-[0.55rem] pr-9 text-left text-[0.875rem] transition any-pointer-coarse:min-h-11",
            selecting && checked
              ? "text-[var(--color-text-primary)]"
              : active
                ? "text-[var(--color-text-primary)]"
                : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
          ].join(" ")}
          onClick={selecting ? onToggleSelect : onOpen}
          title={conversation.title}
        >
          <span className="flex min-w-0 items-center gap-1.5">
            {/* Leading checkbox — select mode only (the design's .conv-check:
                a 1rem square that fills primary with a check when on). */}
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
                {checked ? (
                  <Icon name="check" className="size-[0.7rem] text-[var(--color-on-primary-fill)]" />
                ) : null}
              </span>
            ) : null}
            {streaming ? (
              <span
                aria-label={tr("Working")}
                title={tr("Working…")}
                className="inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--color-accent)]"
              />
            ) : null}
            {temporary && expiresIn !== undefined ? (
              <span
                data-testid="expiring-mark"
                aria-hidden="true"
                title={temporary}
                className="inline-flex shrink-0 items-center gap-0.5 text-[0.7rem] font-medium tabular-nums text-[var(--color-warning-text)]"
              >
                <Icon name="hourglass" className="size-3 shrink-0" />
                {tr("{0}d", [Math.max(1, expiresIn)])}
              </span>
            ) : temporary ? (
              <span data-testid="temporary-mark" aria-hidden="true" title={temporary}>
                <Icon name="hourglass" className="size-3 shrink-0 text-[var(--color-text-muted)]" />
              </span>
            ) : null}
            {patrol ? <PatrolMark /> : null}
            {/* Two audiences, two marks, each labeled (ADR-0057). A single
                unlabeled chain link stood for "shared" and, inside a
                team-shared project, read as "my team can see this" — which it
                never meant. A chat can carry one badge, both, or neither. */}
            {conversation.team_visible || conversation.multiplayer ? (
              <span
                aria-label={
                  conversation.multiplayer
                    ? tr("Multiplayer chat")
                    : tr("Shared with your team")
                }
                title={
                  conversation.multiplayer
                    ? tr("Multiplayer — teammates can chat here")
                    : tr("Shared with your team — teammates can read it")
                }
              >
                <TeamGlyph className="size-3 shrink-0 text-[var(--color-accent)]" />
              </span>
            ) : null}
            {conversation.share_token ? (
              <span
                aria-label={tr("Shared by link — anyone with the link")}
                title={tr("Shared by link — anyone with the link")}
              >
                <ShareGlyph className="size-3 shrink-0 text-[var(--color-accent)]" />
              </span>
            ) : null}
            <span className="block min-w-0 truncate">
              {context ? (
                <span className="text-[var(--color-text-muted)]">{context} › </span>
              ) : null}
              {titleNode ?? conversation.title}
            </span>
            {conversation.lockdown ? (
              <span
                aria-label={tr("Sealed chat")}
                title={tr("Sealed — private chat")}
                className="ml-auto inline-flex shrink-0 pl-1"
              >
                <Icon
                  name="lock"
                  className="size-3 shrink-0 text-[var(--color-text-muted)]"
                />
              </span>
            ) : null}
          </span>
          {/* Label chips hide in select mode (the design's .conv-row.selecting
              .conv-meta) so rows read as a compact pick list. */}
          {labels.length > 0 && !selecting ? (
            <span className="mt-1 flex flex-wrap items-center gap-1 pl-0">
              {shown.map((l) => (
                <LabelChip key={l} name={l} />
              ))}
              {extra > 0 ? (
                <span className="font-[family-name:var(--font-code)] text-[0.68rem] text-[var(--color-text-muted)]">
                  +{extra}
                </span>
              ) : null}
            </span>
          ) : null}
        </button>
      )}

      {!editing && !selecting ? (
        <div className="absolute inset-y-0 right-1 flex items-center">
          {kebab}
        </div>
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

// ── Collapsible section header (Folders / Labels) ────────────────────────────
function SectionToggle({
  icon,
  label,
  open,
  onToggle,
}: {
  icon: string;
  label: string;
  open: boolean;
  onToggle: () => void;
}) {
  return (
    <button
      type="button"
      aria-expanded={open}
      onClick={onToggle}
      className="flex w-full items-center gap-1.5 rounded-md px-2 py-1.5 text-[0.8rem] font-semibold text-[var(--color-text-secondary)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
    >
      <Icon
        name={icon}
        className="size-3.5 shrink-0 text-[var(--color-accent)]"
      />
      <span className="min-w-0 flex-1 text-left">{label}</span>
      <Icon
        name="chevron-right"
        className={["size-3.5 transition", open ? "rotate-90" : ""].join(" ")}
      />
    </button>
  );
}

// ── Main sidebar ─────────────────────────────────────────────────────────────
export function ConversationSidebar({
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
  setPendingDeleteConversation,
  setConversationLabels,
  openShareDialog,
  openTransferDialog,
  canTransferTeamChats,
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
  onCreateProject,
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
}: {
  sidebarOpen: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
  // Shared rail collapse state (owned by ChatExperience via useRailCollapse).
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
  // Keyboard j/k cursor position (owned by ChatExperience so the same order
  // drives nav and rendering). null when no row is focused.
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
  // The person's own chat orders by scope ("pinned", "project:<id>") and the
  // save for a new one (docs/CHAT-ORDER.md). Optional: without them the
  // rail keeps the server's order and offers no reorder mode.
  chatOrders?: Readonly<Record<string, readonly string[]>>;
  onReorderChats?: (scope: string, ids: string[]) => void;
  // How Temporary chats are sorted, and the change for it. Without the
  // handler the list stays most recently active first, with no sort control.
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
  setPendingDeleteConversation: Dispatch<
    SetStateAction<PendingDeleteConversation | null>
  >;
  setConversationLabels: (conversationId: string, labels: string[]) => void;
  // Read-only sharing (#226): issue+copy a public link, revoke it, or re-copy.
  // Opens the Share dialog for one conversation — link scope and team scope
  // in one place (ADR-0057).
  openShareDialog: (conversation: ConversationSummary) => void;
  // Opens the Transfer ownership dialog for one conversation. The owner's
  // row menu always offers it; a Team chats row offers it to an admin.
  openTransferDialog: (conversation: ConversationSummary) => void;
  canTransferTeamChats: boolean;
  archivedConversations: ConversationSummary[];
  showArchived: boolean;
  setShowArchived: Dispatch<SetStateAction<boolean>>;
  setConfirmBulkDelete: Dispatch<SetStateAction<boolean>>;
  // Select mode (#279, redesigned): entered via a row kebab's "Select…" item —
  // that is the only way checkboxes appear. Bulk actions live in the compact
  // icon bar; Escape/Cancel exit.
  selectMode: boolean;
  selectedIds: Set<string>;
  onToggleSelection: (id: string) => void;
  onEnterSelectMode: (id: string) => void;
  onExitSelectMode: () => void;
  onBulkDelete: () => void;
  onBulkPin: () => void;
  onBulkAddLabel: (label: string) => void;
  // Platform-appropriate hint for the unified search bar ("⌘K" / "Ctrl+K").
  searchShortcut: string;
  // Opens the Projects modal (#509). Lives in the rail (like Claude/ChatGPT)
  // rather than the page header; ChatExperience owns the modal state.
  // Opens the modal straight into the new-project form (the section
  // header's + button).
  onCreateProject: () => void;
  // Opens the project's HOME (title · chats · sources · instructions ·
  // per-project settings) in the main pane — a project row's name click and
  // the kebab's "Project settings…" (settings=true opens the dialog too).
  onOpenProjectHome: (projectID: string, settings?: boolean) => void;
  // Pin/unpin a project (owner-only, from the project kebab): a pinned
  // project floats to the top of the rail's list in place — no separate
  // Pinned section like chats have.
  onPinProject: (projectID: string, pinned: boolean) => void;
  // Start a chat in this project (the project menu's "New chat").
  onNewChatInProject?: (projectID: string) => void;
  // Inline rename from the rail (project kebab → Rename); the parent PATCHes
  // just the name.
  onRenameProject: (projectID: string, name: string) => void;
  // Delete from the rail (project kebab); the parent confirms + DELETEs —
  // the server detaches the project's chats rather than deleting them.
  onDeleteProject: (projectID: string) => void;
  // Projects for the rail's Projects section (#509 follow-up): the top few
  // by recent update render as expandable groups + drag targets. The full
  // list/management stays in the modal.
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
  // the kebab's "Move to project" both land here; the parent owns the
  // optimistic state + POST.
  onMoveToProject: (conversationId: string, projectID: string) => void;
  // The project whose home is open in the main pane, for the sidebar's
  // project shortcuts (docs/SIDEBAR.md).
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
}) {
  const [editingId, setEditingId] = useState<string | null>(null);
  // The person's sidebar layout (docs/SIDEBAR.md): the order, names and
  // visibility of the Projects, Chats and Team chats sections, and which
  // start folded. The default layout is the rail as it always was.
  const { layout: railLayout } = useSidebar();
  const projectsSec = sectionOf(railLayout, "projects");
  const chatsSec = sectionOf(railLayout, "chats");
  const teamSec = sectionOf(railLayout, "team");
  const sectionIndex = (id: SectionId) => railLayout.sections.findIndex((x) => x.id === id);
  const projectsFirst = sectionIndex("projects") < sectionIndex("chats");
  const teamFirst = sectionIndex("team") < sectionIndex("chats");
  // One list for all chats, the default (docs/SIDEBAR.md): teammates' chats
  // are listed in the Chats section with the person's own, and Team chats
  // has no section of its own. A person who hid Team chats still sees none
  // of them here; search finds them all the same.
  const unified = !railLayout.separate_lists;
  // Projects section (#509 follow-up): the section itself starts open (its
  // presence is the affordance) unless the layout folds it; each project
  // starts collapsed and remembers its expansion for the session only, like
  // the Labels section.
  const [projectsSectionOpen, setProjectsSectionOpen] = useState(!projectsSec.collapsed);
  // Chats section (mirrors Projects): one heading over everything from the
  // New-chat row down to the conversation list, collapsible, default open.
  const [chatsSectionOpen, setChatsSectionOpen] = useState(!chatsSec.collapsed);
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(
    new Set(),
  );
  // The project being renamed inline (kebab → Rename), mirroring the chat
  // rows' editingId.
  const [renamingProjectId, setRenamingProjectId] = useState<string | null>(
    null,
  );
  // Same story as the chat rows' rename field: it opens from the project kebab
  // and must take focus, but it is rendered inside a .map() so the focus lives
  // here, keyed on which project is being renamed. Only one renames at a time,
  // so one ref is enough.
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
  const [contentResults, setContentResultsRaw] = useState<SearchResult[] | null>(
    null,
  );
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
  const [labelsOpen, setLabelsOpen] = useState(true);
  const [teamChatsOpen, setTeamChatsOpen] = useState(!teamSec.collapsed);
  // When the layout's "starts folded" choices change (the read landed, or
  // the person changed them in the Customize panel), the sections follow.
  // Compared during render, React's "reset state when a prop changes".
  const foldKey = `${Boolean(projectsSec.collapsed)}|${Boolean(chatsSec.collapsed)}|${Boolean(teamSec.collapsed)}`;
  const [seenFoldKey, setSeenFoldKey] = useState(foldKey);
  if (foldKey !== seenFoldKey) {
    setSeenFoldKey(foldKey);
    setProjectsSectionOpen(!projectsSec.collapsed);
    setChatsSectionOpen(!chatsSec.collapsed);
    setTeamChatsOpen(!teamSec.collapsed);
  }
  const [bulkPanel, setBulkPanel] = useState<"none" | "labels">("none");
  const bulkLabelsRef = useRef<HTMLButtonElement | null>(null);
  // The transient per-row "copied ✓" flash is gone with the kebab's copy
  // item: copying now happens inside the Share dialog, which reports it in
  // place (a button that says "Copied ✓") instead of flashing a mark on a row
  // the user may not even be looking at.

  // The parent's `r` shortcut asks for an inline rename by bumping renameSignal.
  // We open the inline editor for the requested id when the nonce changes,
  // comparing *during render* (React's "reset state when a prop changes"
  // pattern) so it fires once per request without a setState-in-effect cascade.
  const [seenRenameNonce, setSeenRenameNonce] = useState<number | null>(
    renameSignal?.nonce ?? null,
  );
  if (renameSignal && renameSignal.nonce !== seenRenameNonce) {
    setSeenRenameNonce(renameSignal.nonce);
    setEditingId(renameSignal.id);
  }

  const labelSummaries: LabelSummary[] = deriveLabels(conversations);
  const allLabelNames = labelSummaries.map((l) => l.name);
  // A chat whose project the viewer cannot see is treated as unfiled, so it
  // still appears somewhere. Only meaningful once the projects list has
  // loaded: an empty list on first paint would move every project chat into
  // Temporary for a frame, so the set is withheld until there is one.
  const knownProjectIds =
    projects.length > 0 ? new Set(projects.map((p) => p.id)) : undefined;
  const pinned = pinnedUnfiled(conversations, knownProjectIds, chatOrders?.[PINNED_ORDER_SCOPE]);
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
  const recent = sortRecent(recentUnfiled(conversations, knownProjectIds), recentSort);
  // The one-list view: the person's unpinned, unfiled chats and teammates'
  // chats in one list under one sort. The person's own rows keep the relative
  // order they have on their own (sortRecent breaks ties by input order, and
  // theirs come first), so j/k — which walks only their own — still matches.
  // A chat is listed once: a teammate's row wins over a copy in the own list.
  const listedTeamChats = teamSec.hidden ? [] : teamSharedConversations;
  const teamRowIds = new Set(listedTeamChats.map((c) => c.id));
  const allRecent = unified
    ? sortRecent(
        [
          ...recentUnfiled(conversations, knownProjectIds).filter((c) => !teamRowIds.has(c.id)),
          ...listedTeamChats,
        ],
        recentSort,
      )
    : recent;
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
  // Which chats have a schedule. Patrols come from one read of the person's
  // patrols per page; the page's own knowledge of a chat's schedules (read
  // when the chat is opened, updated as a patrol is made, paused or ended)
  // wins over it, so a patrol made a minute ago is marked without a reload.
  const patrolIds = usePatrolIds(true);
  const scheduleKind = (id: string): "patrol" | "timer" | "none" =>
    scheduleKindByConv?.get(id) ?? (patrolIds.has(id) ? "patrol" : "none");
  const isPatrol = (id: string) => scheduleKind(id) === "patrol";
  // Temporary is exactly what the server deletes — for inactivity, and by
  // "Delete temporary chats" (isTemporaryChat mirrors the server's one
  // definition). The server's listing says which chats a patrol or timer
  // keeps; the patrols list and what this page knows fill in what changed
  // since.
  const isTemporary = (c: ConversationSummary) =>
    isTemporaryChat({ ...c, has_schedule: c.has_schedule || patrolIds.has(c.id) }, scheduleKindByConv);
  const retention = retentionPolicy(serverConfig);
  const ttlDays = retention.ttlDays;
  // Days left for a temporary chat inside the warning window (null before it).
  const nowSeconds = useNowSeconds();
  const expiresIn = (c: ConversationSummary) =>
    isTemporary(c) ? expiresInDays(c.updated_at, retention, nowSeconds) : null;
  // The person's chats that are about to be deleted, in the order the list
  // shows them, for the notice under the heading and its jump to the first.
  const expiringRecent = recent.filter((c) => expiresIn(c) !== null);
  const showFirstExpiring = () => {
    const first = (unified ? allRecent : recent).find((c) => expiresIn(c) !== null);
    if (!first) return;
    const row = document.querySelector<HTMLElement>(
      `[data-conversation-id="${CSS.escape(first.id)}"] [data-rail-row]`,
    );
    row?.scrollIntoView({ block: "center" });
    row?.focus();
  };
  const canCustomize = useCanCustomize();
  const showSection = (id: SectionId) =>
    canCustomize ? () => updateSidebarLayout((l) => updateSection(l, id, { hidden: false })) : undefined;
  // The Projects SECTION's tree: every project, or — while searching — the
  // name matches, as normal expandable tree rows (results live under their
  // real heading, not in an ad-hoc group inside the Chats list). Only the tree
  // on screen is built, and only when its inputs change: the rail re-renders
  // on every streamed event, and the tree now spans every project.
  const visibleProjectTree = useMemo(
    () =>
      searching
        ? projectGroups(conversations, matchingProjects, Math.max(matchingProjects.length, 1), chatOrders)
        : projectGroups(conversations, projects, Number.POSITIVE_INFINITY, chatOrders),
    [searching, conversations, matchingProjects, projects, chatOrders],
  );
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
    setFilterLabels((ls) =>
      ls.includes(name) ? ls.filter((l) => l !== name) : [...ls, name],
    );
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
      onPin={() => void togglePin(conversation)}
      onRename={() => setEditingId(conversation.id)}
      onDownload={() => void downloadConversation(conversation)}
      onPromote={() => void promoteConversation(conversation)}
      onSavePrompt={() => savePromptFromConversation(conversation)}
      onSetLabels={(labels) => setConversationLabels(conversation.id, labels)}
      onSetProject={(projectID) =>
        onMoveToProject(conversation.id, projectID ?? "")
      }
      isShared={Boolean(conversation.share_token)}
      onShare={() => openShareDialog(conversation)}
      onTransfer={() => openTransferDialog(conversation)}
      onSelect={() => onEnterSelectMode(conversation.id)}
      onArchive={() => void toggleArchive(conversation, true)}
      onDelete={() =>
        setPendingDeleteConversation({
          id: conversation.id,
          title: conversation.title,
        })
      }
    />
  );

  const renderRowIn = (conversation: ConversationSummary, inResults: boolean, markTemporary = false) => (
    <ConvRow
      key={conversation.id}
      conversation={conversation}
      temporary={
        expiresIn(conversation) !== null
          ? expiringRowText(expiresIn(conversation) ?? 0)
          : markTemporary && isTemporary(conversation)
            ? temporaryRowText(ttlDays)
            : undefined
      }
      expiresIn={expiresIn(conversation) ?? undefined}
      titleNode={inResults ? markMatches(conversation.title, tokens) : undefined}
      context={
        inResults && conversation.project_id
          ? projects.find((p) => p.id === conversation.project_id)?.name
          : undefined
      }
      patrol={isPatrol(conversation.id)}
      active={activeConversationId === conversation.id}
      focused={focusedConversationId === conversation.id}
      streaming={streamingConvs.has(conversation.id)}
      editing={editingId === conversation.id}
      selecting={selecting}
      checked={selectedIds.has(conversation.id)}
      onOpen={() => void loadConversation(conversation.id)}
      onToggleSelect={() => onToggleSelection(conversation.id)}
      onCommitRename={(title) => commitRename(conversation.id, title)}
      onCancelRename={() => setEditingId(null)}
      kebab={rowKebab(conversation)}
      onDragStart={
        selecting
          ? undefined
          : (e) => {
              e.dataTransfer.setData(CONV_DRAG_MIME, conversation.id);
              e.dataTransfer.effectAllowed = "move";
            }
      }
    />
  );
  const renderRow = (conversation: ConversationSummary) => renderRowIn(conversation, false);
  const renderResultRow = (conversation: ConversationSummary) => renderRowIn(conversation, true);

  // Projects section (#509 follow-up): every project, pinned first, as
  // expandable groups inside the section's own scroller; each project row is also the drop target for a
  // conversation drag. The header always renders — its + creates a project
  // (opens the modal straight on the new-project form), and each row's kebab
  // (where the chat count used to be) carries edit/rename/delete.
  const projectTreeRows = (
visibleProjectTree.map(({ project, chats }) => {
            const expanded = expandedProjects.has(project.id);
            const dropReady = dragOverProject === project.id;
            // Team sharing, on the row itself. Until now the only tell was
            // the "Shared with team" chip on the project's home, so a rail
            // holding one shared and one personal project looked like two of
            // the same thing — testers renamed their projects by hand to keep
            // them apart. Same glyph as the chat badge, same rule: always
            // labeled with its audience, never a bare "shared"
            // (docs/TEAM-SHARING.md vocabulary).
            const teamShared = Boolean(project.team_id);
            const teamSharedLabel = tr("Shared with {0}", [project.team_id]);
            // undefined = the counts haven't loaded (or the read failed); a
            // loaded map omits projects with none, so a missing key is a real
            // zero. The difference decides whether the empty state may quote
            // a number at all.
            const sharedChats = teamSharedChatCounts
              ? (teamSharedChatCounts[project.id] ?? 0)
              : undefined;
            const emptyState = railProjectEmptyState({
              teamShared,
              teamSharedChatCount: sharedChats,
            });
            const openHome = () => {
              setSidebarOpen(false);
              onOpenProjectHome(project.id);
            };
            return (
              // The WHOLE group (header row + expanded children) is the drop
              // zone, not just the header — dropping a dragged chat onto an
              // expanded project's chat list is the natural gesture and used
              // to silently miss. dragover/drop bubble up from the children.
              <div
                key={project.id}
                onDragOver={(e) => {
                  if (!e.dataTransfer.types.includes(CONV_DRAG_MIME)) return;
                  e.preventDefault();
                  e.dataTransfer.dropEffect = "move";
                  setDragOverProject(project.id);
                }}
                onDragLeave={() =>
                  setDragOverProject((cur) => (cur === project.id ? null : cur))
                }
                onDrop={(e) => {
                  setDragOverProject(null);
                  const convID = e.dataTransfer.getData(CONV_DRAG_MIME);
                  if (!convID) return;
                  e.preventDefault();
                  onMoveToProject(convID, project.id);
                  // Reveal where the chat landed.
                  setExpandedProjects((s) => new Set(s).add(project.id));
                }}
              >
                {renamingProjectId === project.id ? (
                  <input
                    // Uncontrolled like the chat rows' rename input: mounts
                    // only while renaming, so defaultValue tracks the live
                    // name without a sync effect.
                    ref={projectRenameInputRef}
                    aria-label={tr("Rename project {0}", [project.name])}
                    className="mx-[0.4rem] my-[0.32rem] w-[calc(100%-0.8rem)] rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1 text-[0.875rem] text-[var(--color-text-primary)] outline-none"
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
                      "group relative rounded-md transition",
                      dropReady
                        ? "bg-[var(--rail-hover)] ring-1 ring-inset ring-[var(--color-accent)]"
                        : "",
                    ].join(" ")}
                  >
                    {/* Decision #23: the NAME opens the project's home and
                        expands it; the chevron only folds. Two buttons, so
                        each does one thing and says so. The chevron keeps
                        the row's "Project <name> (N chats)" name and its
                        aria-expanded — it is the disclosure control. */}
                    <div className="flex w-full items-center rounded-md pr-9 any-pointer-coarse:min-h-11">
                      <button
                        type="button"
                        aria-expanded={expanded}
                        aria-label={tr("Project {0} ({1} chats)", [project.name, chats.length])}
                        title={expanded ? tr("Collapse") : tr("Expand")}
                        className="inline-flex size-6 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
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
                          name="chevron-right"
                          className={[
                            "size-3 shrink-0 transition",
                            expanded ? "rotate-90" : "",
                          ].join(" ")}
                        />
                      </button>
                      <button
                        type="button"
                        aria-label={tr("Open project {0}", [project.name])}
                        data-rail-row=""
                        className={[
                          "flex min-w-0 flex-1 items-center gap-2 rounded-md py-1.5 pl-1 pr-2 text-[0.875rem] transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                          dropReady
                            ? "text-[var(--color-text-primary)]"
                            : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
                        ].join(" ")}
                        onClick={() => {
                          setExpandedProjects((s) =>
                            s.has(project.id) ? s : new Set(s).add(project.id),
                          );
                          openHome();
                        }}
                      >
                        {project.pinned ? (
                          <Icon
                            name="pin"
                            className="size-3 shrink-0 text-[var(--color-accent)]"
                          />
                        ) : null}
                        {teamShared ? (
                          // Decorative HERE on purpose: this button carries
                          // its own aria-label, which swallows any label
                          // nested inside it. The audience text lives in the
                          // sr-only sibling just below, where a screen reader
                          // actually reaches it.
                          <span title={teamSharedLabel} className="flex shrink-0">
                            <TeamGlyph className="size-3 text-[var(--color-accent)]" />
                          </span>
                        ) : null}
                        <span className="min-w-0 flex-1 truncate text-left">
                          {project.name}
                        </span>
                      </button>
                    </div>
                    {teamShared ? (
                      <span className="sr-only">{teamSharedLabel}</span>
                    ) : null}
                    <div className="absolute inset-y-0 right-1 flex items-center gap-0.5">
                      <ProjectKebab
                        projectId={project.id}
                        projectName={project.name}
                        pinned={Boolean(project.pinned)}
                        isOwner={project.owner_email === userEmail}
                        onOpen={() => {
                          setExpandedProjects((s) =>
                            s.has(project.id) ? s : new Set(s).add(project.id),
                          );
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
                        // Project sharing lives in settings ("Who can see
                        // it"), so Share opens the same dialog.
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
                                setExpandedProjects((s) =>
                                  s.has(project.id) ? s : new Set(s).add(project.id),
                                );
                                setReorderScope(projectOrderScope(project.id));
                              }
                            : undefined
                        }
                      />
                    </div>
                  </div>
                )}
                {expanded ? (
                  chats.length === 0 ? (
                    <p className="py-1 pl-7 pr-2 text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
                      {/* Three empty states, because "empty" means different
                          things to the owner and to a teammate. See
                          railProjectEmptyState. */}
                      {emptyState.kind === "team-has-chats" ? (
                        <>
                          {tr("No chats of yours yet ·")}{" "}
                          <button
                            type="button"
                            aria-label={tr(emptyState.count === 1 ? "Open {0} — {1} chat shared by your team" : "Open {0} — {1} chats shared by your team", [project.name, emptyState.count])}
                            className="rounded text-left text-[var(--color-accent)] underline decoration-dotted underline-offset-2 transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                            onClick={openHome}
                          >
                            {emptyState.count} {tr("shared by your team")}{" "}
                            <span aria-hidden="true">&rarr;</span>
                          </button>
                        </>
                      ) : emptyState.kind === "team-count-unknown" ? (
                        // Teammates may have shared chats here that the rail
                        // can't count yet, so this says nothing about theirs.
                        <>{tr("No chats of yours yet")}</>
                      ) : (
                        // Decision #23: the short line, replacing the
                        // filing-paths paragraph.
                        <>{tr("No chats yet")}</>
                      )}
                    </p>
                  ) : (
                    <div className="ml-3 border-l border-[var(--color-border)] pl-1">
                      {reorderScope === projectOrderScope(project.id) && onReorderChats && reorderable(chats.length) ? (
                        <ChatReorderList
                          chats={chats.map((c) => ({ id: c.id, title: c.title }))}
                          label={tr("Chats in {0}, in order", [project.name])}
                          hint={tr("Drag a chat, or use its arrows, to set the order. A chat added for the first time appears at the top.")}
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
                  )
                ) : null}
              </div>
            );
          })
  );
  const projectsSection = (
    <div className="mb-1">
      <div className="flex items-center gap-1">
        <div className="min-w-0 flex-1">
          <SectionToggle
            icon="briefcase"
            label={projectsSec.label || tr("Projects")}
            open={projectsSectionOpen}
            onToggle={() => setProjectsSectionOpen((o) => !o)}
          />
        </div>
        <PortalTipIconButton
          tip="Create project"
          ariaLabel={tr("Create project")}
          icon="plus"
          iconClassName="size-4"
          className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition motion-safe:hover:scale-110 hover:bg-[var(--color-status-success-bg)] hover:text-[var(--color-status-success-fg)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={onCreateProject}
        />
      </div>
      {projectsSectionOpen ? (
        visibleProjectTree.length === 0 ? (
          searching ? (
            <p className="px-2 py-1.5 text-[0.78rem] text-[var(--color-text-muted)]">
              {tr("No matching projects.")}
            </p>
          ) : (
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
              onClick={onCreateProject}
            >
              <Icon name="plus" className="size-3.5 shrink-0" />
              {tr("New project…")}
            </button>
          )
        ) : (
          projectTreeRows
        )
      ) : null}
    </div>
  );

  const labelsSection =
    labelSummaries.length > 0 ? (
      <div className="mb-1">
        <SectionToggle
          icon="tag"
          label={tr("Labels")}
          open={labelsOpen}
          onToggle={() => setLabelsOpen((o) => !o)}
        />
        {labelsOpen
          ? labelSummaries.map((l) => (
              <button
                key={l.name}
                type="button"
                aria-pressed={filterLabels.includes(l.name)}
                onClick={() => toggleLabelFilter(l.name)}
                className={[
                  "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.875rem] transition",
                  filterLabels.includes(l.name)
                    ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]"
                    : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
                ].join(" ")}
              >
                <span className="label-dot" style={labelChipStyle(l.name)} />
                <span className="min-w-0 flex-1 truncate text-left">
                  {l.name}
                </span>
                <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                  {l.count}
                </span>
              </button>
            ))
          : null}
      </div>
    ) : null;

  // What the sidebar's shortcuts need from this page (sidebar/targets.tsx):
  // live names, the open chat or project, whether a target can still be
  // opened by this account, and opening it in place.
  // "Not available" needs every list a chat could be in to have answered:
  // own (the history load), archived and shared — each read on its own.
  const listsKnown = !isLoadingHistory && teamSharedLoaded && archivedLoaded;
  const sidebarTargets: SidebarTargets = {
    activeConversationId,
    activeProjectId,
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
        if (activeConversationId === ref && onSetDashboardView) {
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

  // The Projects section with its divider, placed above or below the chats
  // as the person's layout orders the sections (hidden when they hid it).
  const projectsBlock = projectsSec.hidden || searching ? null : projectsFirst ? (
    <>
      {/* Projects section — ABOVE the New-chat row (the rail's primary nav
          block). Wide-collapsed (≥sm) it hides with the rest of the wide-only
          content; the max-height guard keeps a long or deeply expanded
          tree from pushing New chat and the list off-screen — past it the
          section scrolls on its own, like the Chats list below. */}
      <div
        className={[
          "max-h-[30vh] overflow-y-auto overflow-x-hidden",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      >
        {projectsSection}
      </div>

      {/* Divider between the Projects and Chats sections. */}
      <div
        className={[
          "my-1.5 border-t border-[var(--color-border)]",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      />
    </>
  ) : (
    <>
      {/* Divider between the Projects and Chats sections. */}
      <div
        className={[
          "my-1.5 border-t border-[var(--color-border)]",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      />
      {/* Projects section — ABOVE the New-chat row (the rail's primary nav
          block). Wide-collapsed (≥sm) it hides with the rest of the wide-only
          content; the max-height guard keeps a long or deeply expanded
          tree from pushing New chat and the list off-screen — past it the
          section scrolls on its own, like the Chats list below. */}
      <div
        className={[
          "max-h-[30vh] overflow-y-auto overflow-x-hidden",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      >
        {projectsSection}
      </div>

    </>
  );

  // One Archived row: in the collapsible section, and in the search results.
  const renderArchivedRow = (conversation: ConversationSummary, inResults: boolean) => (
                    <div
                      key={conversation.id}
                      className={[
                        "group relative rounded-md transition",
                        activeConversationId === conversation.id
                          ? "bg-[var(--rail-active)]"
                          : "hover:bg-[var(--rail-hover)]",
                      ].join(" ")}
                    >
                      <button
                        type="button"
                        data-rail-row=""
                        className="block w-full min-w-0 rounded-md py-1.5 pl-3 pr-20 text-left text-[0.8125rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)] any-pointer-coarse:min-h-11 any-pointer-coarse:pr-28"
                        onClick={() => void loadConversation(conversation.id)}
                      >
                        <span className="block truncate italic">
                          {inResults ? markMatches(conversation.title, tokens) : conversation.title}
                        </span>
                      </button>
                      <div className={`absolute inset-y-0 right-1 flex items-center gap-1 transition any-pointer-coarse:gap-4 ${ROW_ACTIONS_REVEAL}`}>
                        <button
                          type="button"
                          aria-label={tr("Unarchive {0}", [conversation.title])}
                          data-tip-top="Unarchive"
                          className="hit-area-touch inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                          onClick={() =>
                            void toggleArchive(conversation, false)
                          }
                        >
                          <Icon name="unarchive" className="size-3.5" />
                        </button>
                        <button
                          type="button"
                          aria-label={tr("Delete {0}", [conversation.title])}
                          data-tip-top="Delete"
                          className="hit-area-touch inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition motion-safe:hover:scale-110 hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)] sm:size-7"
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
                    </div>
                  );

  // One Team chats row: in the section, and in the search results (where the
  // title's matched words are marked).
  const renderTeamRow = (c: ConversationSummary, inResults: boolean, merged = false) => {
                        const active = activeConversationId === c.id;
                        const ownerShort = c.user_email
                          ? c.user_email.split("@")[0]
                          : "";
                        return (
                          <div key={c.id} className="group relative">
                          <button
                            type="button"
                            data-testid="team-shared-conv-row"
                            data-rail-row=""
                            aria-describedby={isPatrol(c.id) ? PATROL_DESC_ID : undefined}
                            className={[
                              "flex w-full min-w-0 items-center gap-1.5 rounded-md pr-10 text-left transition any-pointer-coarse:min-h-11 any-pointer-coarse:pr-12",
                              // In the one-list view a teammate's row sits
                              // between the person's own, so it takes their size.
                              merged ? "py-2 pl-[0.55rem] text-[0.875rem]" : "py-1.5 pl-2 text-[0.84rem]",
                              active
                                ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]"
                                : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
                            ].join(" ")}
                            onClick={() => {
                              setSidebarOpen(false);
                              if (onSelectTeamSharedConversation) {
                                onSelectTeamSharedConversation(c);
                              } else {
                                void loadConversation(c.id);
                              }
                            }}
                            title={
                              c.user_email
                                ? `${c.title} (${c.user_email})`
                                : c.title
                            }
                          >
                            <TeamGlyph className="size-3 shrink-0 text-[var(--color-accent)]" />
                            {isPatrol(c.id) ? <PatrolMark /> : null}
                            <span className="min-w-0 flex-1 truncate">
                              {inResults ? markMatches(c.title, tokens) : c.title}
                            </span>
                            {c.multiplayer ? (
                              <span className="shrink-0 rounded-full bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] px-1.5 py-px text-[0.62rem] font-medium text-[var(--color-accent)]">
                                {tr("Live")}
                              </span>
                            ) : null}
                            {ownerShort ? (
                              // Shortens, never pushes: a long mailbox name
                              // ran out of the row and over the sidebar edge.
                              <span className="max-w-[45%] min-w-0 shrink truncate font-[family-name:var(--font-code)] text-[0.66rem] text-[var(--color-text-muted)]">
                                {ownerShort}
                              </span>
                            ) : null}
                          </button>
                          <div className="absolute inset-y-0 right-1 flex items-center">
                            <TeamChatKebab
                              conversation={c}
                              onTransfer={canTransferTeamChats ? () => openTransferDialog(c) : undefined}
                            />
                          </div>
                          </div>
                        );
                      };

  // Teammates' shared and multiplayer chats, above or below the person's own
  // chats as their layout orders the sections.
  const teamBlock =
    !unified && !teamSec.hidden && teamSharedConversations.length > 0 ? (
                <div
                  data-testid="team-chats-section"
                  className={
                    teamFirst
                      ? "mb-1.5 border-b border-[var(--color-border)] pb-1.5"
                      : "mb-1 mt-2 border-t border-[var(--color-border)] pt-2"
                  }
                >
                  <SectionToggle
                    icon="message"
                    label={
                      teamSec.label
                        ? `${teamSec.label} (${teamSharedConversations.length})`
                        : tr("Team chats ({0})", [teamSharedConversations.length])
                    }
                    open={teamChatsOpen}
                    onToggle={() => setTeamChatsOpen((o) => !o)}
                  />
                  {teamChatsOpen
                    ? teamSharedConversations.map((c) => renderTeamRow(c, false))
                    : null}
                </div>
    ) : null;

  // Team chats are a section of their own, placed where the person's layout
  // orders it (above or below the Chats heading, and around Projects), and
  // scrolling on its own like Projects — never inside the Chats list, so
  // folding Chats does not hide them.
  const teamAboveProjects = sectionIndex("team") < sectionIndex("projects");
  // While searching, the sections step aside for one list of results (below).
  const teamTop = teamBlock && !searching ? (
    <div
      className={[
        "max-h-[30vh] shrink-0 overflow-y-auto overflow-x-hidden",
        railCollapsed ? "sm:hidden" : "",
      ].join(" ")}
    >
      {teamBlock}
    </div>
  ) : null;

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
  // email) opens through the shared door, as its Team chats row does. If the
  // shared list has not loaded, the hit itself stands in for the row: the
  // page tries it as a chat the person takes part in, then as a read-only
  // team chat.
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
      {resultCount === 0 ? (
        messageSearchPending ? (
          <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">{tr("Searching…")}</p>
        ) : (
          <div data-testid="search-empty" className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
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
          // In the one-list view teammates' chats live in the Chats section,
          // so hiding either one hides them.
          hidden={teamSec.hidden || (unified && chatsSec.hidden)}
          onShow={showSection(teamSec.hidden ? "team" : "chats")}
        >
          {teamMatches.map((c) => renderTeamRow(c, true))}
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
          {projectTreeRows}
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
                aria-describedby={isPatrol(r.conversation_id) ? PATROL_DESC_ID : undefined}
                className="block w-full rounded-md px-2 py-1.5 text-left transition hover:bg-[var(--rail-hover)] any-pointer-coarse:min-h-11"
                onClick={() => {
                  if (r.user_email) setSidebarOpen(false);
                  openMessageHit(r);
                }}
              >
                <span className="flex min-w-0 items-center gap-1.5 text-[0.85rem] text-[var(--color-text-secondary)]">
                  {r.user_email ? <TeamGlyph className="size-3 shrink-0 text-[var(--color-accent)]" /> : null}
                  <span className="min-w-0 flex-1 truncate">
                    {proj ? <span className="text-[var(--color-text-muted)]">{proj.name} › </span> : null}
                    {r.title || tr("Untitled")}
                  </span>
                  {archived ? <ResultTag>{tr("Archived")}</ResultTag> : null}
                  {isPatrol(r.conversation_id) ? <PatrolMark /> : null}
                  {r.user_email ? (
                    // Whose chat it is, as on a Team chats row.
                    <span className="max-w-[45%] min-w-0 shrink truncate font-[family-name:var(--font-code)] text-[0.66rem] text-[var(--color-text-muted)]">
                      {r.user_email.split("@")[0]}
                    </span>
                  ) : null}
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

  return (
    <SidebarTargetsContext.Provider value={sidebarTargets}>
    <NavRail
      activeView="chat"
      brandName={branding.app_name}
      brandLogoSrc={branding.logo_url || undefined}
      sidebarOpen={sidebarOpen}
      setSidebarOpen={setSidebarOpen}
      collapse={collapse}
      account={{ email: userEmail, onSignOut }}
      footer={
        <div
          className={["grid gap-1 pt-1", railCollapsed ? "sm:hidden" : ""].join(
            " ",
          )}
        >
          {conversations.some(isTemporary) ? (
            <button
              type="button"
              // Red at rest, not only on hover: this deletes many chats at
              // once, so it must not read like the rail's ordinary rows.
              className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.75rem] font-medium text-[var(--color-danger-text)] transition hover:bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)]"
              onClick={() => setConfirmBulkDelete(true)}
              title={tr("Delete every temporary chat")}
            >
              <Icon name="trash" className="size-3.5 shrink-0" />
              {tr("Delete temporary chats")}
            </button>
          ) : null}
        </div>
      }
    >
      {/* Unified search — one bar for everything in the rail: the person's
          own chats, teammates' chats, projects and archived chats by name,
          and (debounced) Postgres full-text hits inside message content.
          While it holds text the sections below step aside for one list of
          results, so a hidden or folded section never hides a match
          (docs/SIDEBAR-SEARCH.md). ⌘K focuses this input. */}
      <div
        className={[
          "relative mb-1.5 flex items-center",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      >
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
          placeholder={tr("Search… ({0})", [searchShortcut])}
          aria-label={tr("Search")}
          data-testid="search-input"
          className="search-input-no-native-clear min-h-[2.2rem] w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-[0.4rem] pl-[2.1rem] pr-8 text-[0.85rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
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
        ) : null}
      </div>

      {/* What a row's mark means, once, for the rows that point here. */}
      <span id={PATROL_DESC_ID} hidden>
        {PATROL_ROW_TEXT}
      </span>
      <span id={TEMPORARY_DESC_ID} hidden>
        {temporaryRowText(ttlDays)}
      </span>

      {teamFirst && teamAboveProjects ? teamTop : null}
      {projectsFirst ? projectsBlock : null}


      {/* Team chats ordered above the person's own chats: their own block,
          above the Chats heading, scrolling on its own like Projects. */}
      {teamFirst && !teamAboveProjects ? teamTop : null}

      {/* Chats section header (mirrors the Projects header): a collapsible
          heading over the conversation list. + is THE new-chat affordance
          now (sealed automatically when the deployment is lockdown-only);
          the lock starts a sealed chat where both modes exist. Hidden in
          the ≥sm collapsed strip, where the body renders as icons. */}
      <div
        className={[
          "flex items-center gap-1",
          railCollapsed ? "sm:hidden" : "",
        ].join(" ")}
      >
        <div className="min-w-0 flex-1">
          {searching ? (
            // The sections step aside while searching: this row names what
            // the list below is, and keeps the new-chat buttons in reach.
            <p
              aria-hidden="true"
              className="m-0 flex w-full items-center gap-1.5 px-2 py-1.5 text-[0.8rem] font-semibold text-[var(--color-text-secondary)]"
            >
              <Icon name="search" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
              <span className="min-w-0 flex-1 truncate">
                {resultCount === 1 ? tr("1 result") : tr("{0} results", [resultCount])}
              </span>
            </p>
          ) : chatsSec.hidden ? null : (
            <SectionToggle
              icon="message"
              label={chatsSec.label || tr("Chats")}
              open={chatsSectionOpen}
              onToggle={() => setChatsSectionOpen((o) => !o)}
            />
          )}
        </div>
        {serverConfig.lockdownAvailable && !serverConfig.lockdownOnly ? (
          <SealedNewChatButton
            onClick={() => clearConversation({ lockdown: true })}
          />
        ) : null}
        <PortalTipIconButton
          tip="New chat"
          ariaLabel={tr("New chat")}
          icon="plus"
          iconClassName="size-4"
          className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition motion-safe:hover:scale-110 hover:bg-[var(--color-status-success-bg)] hover:text-[var(--color-status-success-fg)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={() =>
            clearConversation(
              serverConfig.lockdownOnly ? { lockdown: true } : undefined,
            )
          }
        />
      </div>

      {/* Chats section body. Closed on the wide rail = hidden; the ≥sm
          collapsed strip shows it regardless (its icons ARE the strip). */}
      <div
        className={[
          "flex min-h-0 flex-1 flex-col",
          // A search shows its results whether or not Chats is folded.
          chatsSectionOpen || chatsSec.hidden || searching ? "" : railCollapsed ? "hidden sm:flex" : "hidden",
        ].join(" ")}
      >
        {/* The full-size New-chat row is gone (the Chats heading's + owns it);
            the ≥sm collapsed strip still needs an icon. */}
        {railCollapsed ? (
          <button
            type="button"
            className="hidden w-full items-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-status-success-bg)] hover:text-[var(--color-status-success-fg)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:mx-auto sm:flex sm:size-10 sm:justify-center sm:p-0"
            title={tr("New chat")}
            aria-label={tr("New chat")}
            data-tip="New chat"
            onClick={() =>
              clearConversation(
                serverConfig.lockdownOnly ? { lockdown: true } : undefined,
              )
            }
          >
            <Icon
              name={serverConfig.lockdownOnly ? "lock" : "plus"}
              className="size-4"
            />
          </button>
        ) : null}

        {/* Active-filter chips */}
        {filterLabels.length > 0 ? (
          <div
            className={[
              "mt-2 flex items-center gap-1.5 motion-safe:animate-[filter-in_var(--motion-fast)_ease_both]",
              railCollapsed ? "sm:hidden" : "",
            ].join(" ")}
          >
            <div className="flex min-w-0 flex-1 flex-wrap gap-1.5">
              {filterLabels.map((l) => (
                <span
                  key={l}
                  className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.78rem] text-[var(--color-text-primary)]"
                >
                  <span className="text-[var(--color-text-muted)]">{tr("Label:")}</span>{" "}
                  {l}
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
              {tr("Clear")}</button>
          </div>
        ) : null}

        {/* Conversation list */}
        {/* The arrow-key handler is delegated from the rows (buttons) inside. */}
        {/* eslint-disable-next-line jsx-a11y/no-static-element-interactions */}
        <div
          ref={listRef}
          onKeyDown={onResultsKeyDown}
          className={[
            "mt-2 flex-1 overflow-y-auto",
            railCollapsed ? "sm:hidden" : "",
          ].join(" ")}
        >
          {isLoadingHistory ? (
            <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
              {tr("Loading…")}
            </p>
          ) : searching ? (
            searchResults
          ) : filtering ? (
            // A label filter alone: the person's own chats carrying it.
            filteredConversations.length === 0 ? (
              <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
                {tr("Nothing matches this filter.")}
              </p>
            ) : (
              filteredConversations.map(renderRow)
            )
          ) : (
            <>
              {chatsSec.hidden ? null : (
              <>
              {pinned.length > 0 ? (
                <div className="mb-1" data-testid="pinned-section">
                  <div className="flex items-center gap-1.5 px-2 pb-1 pt-2 text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
                    <Icon
                      name="pin"
                      className="size-3 shrink-0 text-[var(--color-accent)]"
                    />
                    {tr("Pinned")}
                    {canReorderPinned && reorderScope !== PINNED_ORDER_SCOPE && !selecting ? (
                      <button
                        type="button"
                        ref={reorderToggleRef}
                        data-testid="pinned-reorder-toggle"
                        aria-label={tr("Reorder pinned chats")}
                        title={tr("Reorder pinned chats")}
                        onClick={() => setReorderScope(PINNED_ORDER_SCOPE)}
                        className="ml-auto inline-flex h-6 items-center gap-1 rounded-md px-1.5 text-[0.68rem] font-semibold normal-case tracking-normal text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-9"
                      >
                        <Icon name="selector" className="size-3.5" />
                        {tr("Reorder")}
                      </button>
                    ) : null}
                  </div>
                  {reorderScope === PINNED_ORDER_SCOPE && canReorderPinned && onReorderChats ? (
                    <ChatReorderList
                      chats={pinned.map((c) => ({ id: c.id, title: c.title }))}
                      label={tr("Pinned chats, in order")}
                      hint={tr("Drag a chat, or use its arrows, to set the order. A chat pinned for the first time appears at the top.")}
                      onReorder={(ids) => onReorderChats(PINNED_ORDER_SCOPE, ids)}
                      onDone={() => endReorder(() => reorderToggleRef.current?.focus())}
                    />
                  ) : (
                    pinned.map(renderRow)
                  )}
                </div>
              ) : null}
              {labelsSection}
              <div className="mb-1">
                {/* any-pointer-coarse:flex-wrap: at the 16px a touch screen
                    forces on the sort control, "TEMPORALES" and "Actividad
                    reciente" do not fit one row; the control then takes the
                    next row whole instead of being cut off. */}
                <div className="flex items-center gap-1.5 px-2 pb-1 pt-2 text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)] any-pointer-coarse:flex-wrap">
                  {unified ? (
                    <Icon name="message" className="size-3 shrink-0 text-[var(--color-accent)]" />
                  ) : (
                    <Icon name="hourglass" className="size-3 shrink-0 text-[var(--color-accent)]" />
                  )}
                  {unified ? tr("Recent") : tr("Temporary")}
                  {unified ? (
                    <RecentInfoButton text={unifiedInfoText(retention)} label={tr("About this list")} />
                  ) : (
                    <RecentInfoButton text={recentInfoText(retention)} />
                  )}
                  {onRecentSortChange && allRecent.length > 1 ? (
                    // On a touch screen every select is forced to 16px text
                    // (so iOS does not zoom on focus); the width cap below is
                    // sized for the 11px desktop label and cut the value off
                    // ("Recent activit"), so it is lifted there. Without a
                    // cap the control must still give way to the row: it is
                    // sized to the chosen value where the engine can
                    // (field-sizing) and may shrink (min-w-0), so a longer
                    // translation no longer pushes it past the sidebar.
                    <select
                      aria-label={unified ? tr("Sort chats") : tr("Sort temporary chats")}
                      data-testid="recent-sort"
                      value={recentSort}
                      onChange={(e) => {
                        if (isRecentSort(e.target.value)) onRecentSortChange(e.target.value);
                      }}
                      className="ml-auto h-6 max-w-[8.5rem] cursor-pointer rounded-md border border-transparent bg-transparent px-1 text-[0.68rem] font-semibold normal-case tracking-normal min-w-0 shrink any-pointer-coarse:max-w-full any-pointer-coarse:font-medium any-pointer-coarse:[field-sizing:content] text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-9"
                    >
                      {RECENT_SORTS.map((o) => (
                        <option key={o.value} value={o.value}>
                          {o.label}
                        </option>
                      ))}
                    </select>
                  ) : null}
                </div>
                <RetentionNotices
                  expiringCount={expiringRecent.length}
                  temporaryCount={conversations.filter(isTemporary).length}
                  cap={retention.cap}
                  onShowFirst={showFirstExpiring}
                />
                {allRecent.length === 0 ? (
                  <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
                    {tr("No saved chats yet.")}
                  </p>
                ) : unified ? (
                  <div data-testid="unified-chat-list">
                    {allRecent.map((c) =>
                      teamRowIds.has(c.id) ? renderTeamRow(c, false, true) : renderRowIn(c, false, true),
                    )}
                  </div>
                ) : (
                  recent.map(renderRow)
                )}
              </div>
              </>
              )}
            </>
          )}

          {/* Archived (collapsible) */}
          {!chatsSec.hidden && !searching && archivedConversations.length > 0 ? (
            <div className="mt-3 border-t border-[var(--color-border)] pt-2">
              <button
                type="button"
                aria-expanded={showArchived}
                aria-label={tr("Archived conversations ({0})", [archivedConversations.length])}
                className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-[0.6875rem] font-medium text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-secondary)]"
                onClick={() => setShowArchived((v) => !v)}
              >
                <Icon
                  name={showArchived ? "chevron-down" : "chevron-right"}
                  className="size-3 shrink-0"
                />
                {tr("Archived (")}{archivedConversations.length})
              </button>
              {showArchived
                ? archivedConversations.map((conversation) => renderArchivedRow(conversation, false))
                : null}
            </div>
          ) : null}
        </div>
      </div>
      {!teamFirst && (projectsFirst || teamAboveProjects) ? teamTop : null}
      {projectsFirst ? null : projectsBlock}
      {!teamFirst && !projectsFirst && !teamAboveProjects ? teamTop : null}
      {/* Multi-select bulk bar (#279, the design's .bulk-bar): pinned to the
          rail's foot below the conversation list — count + 1.9rem icon actions with data-tip-top tooltips. Add-label
          reuses the kebab's panel (including its inline create input) in a
          menu that opens above the bar. Actions disable at zero selected. */}
      {selecting ? (
        <>
          <div
            className={[
              "mt-2 flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-[0.4rem] pl-[0.65rem] pr-[0.3rem] motion-safe:animate-pop-up",
              railCollapsed ? "sm:hidden" : "",
            ].join(" ")}
          >
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
                onClick={() =>
                  setBulkPanel((p) => (p === "labels" ? "none" : "labels"))
                }
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
            <p
              className={[
                "mt-1.5 text-[0.6875rem] text-[var(--color-danger)]",
                railCollapsed ? "sm:hidden" : "",
              ].join(" ")}
            >
        {rich("Selecting {0} conversations — large bulk deletes are permanent.", selectedIds.size)}
      </p>
          ) : null}
        </>
      ) : null}
    </NavRail>
    </SidebarTargetsContext.Provider>
  );
}
