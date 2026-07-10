"use client";

// Conversation rail (#169 unified shell, #258/#279 organization). This renders
// the chat surface's content inside the shared NavRail: the new-chat / sealed
// affordances, the search filter, the organized conversation list (Pinned ·
// Folders · Labels · Recent) with a per-row kebab menu, folder/label filtering,
// and the collapsible Archived section. The footer (update banner +
// delete-all-unpinned) is handed to NavRail; sign-out moved into the rail's
// account menu.
//
// It stays purely presentational + event-forwarding: conversation state and the
// mutation handlers live in ChatExperience and are threaded in via props. The
// per-row kebab and the account menu share one Menu surface, so they read as one
// component family. Per-row accessible names ("Pin/Unpin/Archive/Unarchive/
// Delete/Download <title>") are preserved verbatim — they now live inside the
// kebab menu, and the live conversation-mgmt e2e opens the kebab to reach them.

import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { NavRail, type RailCollapse } from "@/app/shared/ui/NavRail";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { labelChipStyle } from "@/app/shared/lib/labelColors";
import { Icon } from "./Icon";
import {
  MAX_LABELS,
  MAX_LABEL_LEN,
  addLabel as addLabelTo,
  canAddLabel,
  deriveFolders,
  deriveLabels,
  isFiltering as computeIsFiltering,
  pinnedUnfiled,
  projectGroups,
  recentUnfiled,
  type FolderSummary,
  type LabelSummary,
} from "./conversationOrganization";
import type { ConversationSummary, PendingDeleteConversation, ServerConfig } from "./chat-experience";
import type { Project } from "./ProjectsModal";

// ── Share glyph (#226) ───────────────────────────────────────────────────────
// The chain-link icon used for share affordances; `off` adds the slash for the
// "stop sharing" variant.
function ShareGlyph({ className, off }: { className?: string; off?: boolean }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M10 13a5 5 0 0 0 7.07 0l2-2a5 5 0 0 0-7.07-7.07l-1 1" />
      <path d="M14 11a5 5 0 0 0-7.07 0l-2 2a5 5 0 0 0 7.07 7.07l1-1" />
      {off ? <path d="M4 4l16 16" /> : null}
    </svg>
  );
}

// ── Sealed-chat button + explainer tooltip ───────────────────────────────────
// The icon-only "new sealed chat" button next to New chat explains what a
// sealed chat is on hover AND keyboard focus, using the design's .conv-tooltip
// surface. The tooltip portals to <body> because the rail's
// transform/backdrop-filter would otherwise capture its fixed positioning.
const SEALED_TOOLTIP_TEXT =
  "Sealed chat — locked down and private. Your data and an approved model stay inside this sandbox; nothing leaves.";

function SealedNewChatButton({ onClick }: { onClick: () => void }) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    // Below the button, arrow pointing back up at it (the design's conv-info
    // tooltip placement: bottom + 7, offset so the arrow lands on the anchor).
    setPos({ top: Math.round(r.bottom + 7), left: Math.round(r.left + r.width / 2 - 17) });
  };
  const hide = () => setPos(null);
  return (
    <button
      type="button"
      className="inline-flex size-10 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
      aria-label="New sealed chat — sandboxed, vetted model, nothing leaves"
      onClick={onClick}
      onMouseEnter={show}
      onMouseLeave={hide}
      onFocus={show}
      onBlur={hide}
    >
      {/* Inherits the button's text-primary (the design's .rail-lock), not
          the accent the row indicators use. */}
      <Icon name="lock" className="size-4" />
      {pos && typeof document !== "undefined"
        ? createPortal(
            <span role="tooltip" className="conv-tooltip" style={{ top: pos.top, left: pos.left }}>
              {SEALED_TOOLTIP_TEXT}
            </span>,
            document.body,
          )
        : null}
    </button>
  );
}

// ── Recent-section info note ─────────────────────────────────────────────────
// The ⓘ next to the "Recent" group header click-toggles the retention
// explainer on the shared .conv-tooltip surface (portaled to <body> like the
// sealed tooltip above, and for the same reason). Click-toggle rather than
// hover-only so it works on touch; dismissed by clicking anywhere else or
// pressing Escape. The copy states the server's default TTL
// (CONVERSATION_TTL_DAYS, default 14); pinning is the one user action that
// always exempts a chat from the sweep, so that is the action we name.
const RECENT_INFO_TEXT =
  "Unpinned chats are deleted after 14 days of inactivity. Pin a chat to keep it.";

function RecentInfoButton() {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const btnRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    if (!pos) return;
    // Clicks on the trigger itself are the toggle's job; everything else
    // (including clicks inside the tooltip, which only carries static text)
    // dismisses. Listeners attach only while open.
    const onDocClick = (e: MouseEvent) => {
      if (btnRef.current && e.target instanceof Node && btnRef.current.contains(e.target)) return;
      setPos(null);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setPos(null);
    };
    document.addEventListener("click", onDocClick);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("click", onDocClick);
      document.removeEventListener("keydown", onKey);
    };
  }, [pos]);

  return (
    <span className="relative inline-flex">
      <button
        ref={btnRef}
        type="button"
        aria-label="About recent chats"
        aria-expanded={pos !== null}
        className="hit-area inline-flex items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)] aria-expanded:text-[var(--color-text-secondary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        onClick={(e) => {
          const r = e.currentTarget.getBoundingClientRect();
          // Same placement math as the sealed tooltip: below the button,
          // shifted so the surface's arrow points back at the anchor.
          setPos(
            pos ? null : { top: Math.round(r.bottom + 7), left: Math.round(r.left + r.width / 2 - 17) },
          );
        }}
      >
        <Icon name="info" className="size-[0.9rem]" />
      </button>
      {pos && typeof document !== "undefined"
        ? createPortal(
            <span
              role="tooltip"
              className="conv-tooltip"
              style={{ top: pos.top, left: pos.left }}
              onClick={(e) => e.stopPropagation()}
            >
              {RECENT_INFO_TEXT}
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
          aria-label={`Remove ${name}`}
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

// ── Folder picker panel (shared by per-row kebab + bulk bar) ─────────────────
function FolderPanel({
  folders,
  currentFolder,
  onPick,
  onRemove,
}: {
  folders: FolderSummary[];
  currentFolder?: string;
  onPick: (name: string) => void;
  onRemove?: () => void;
}) {
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const submit = () => {
    const trimmed = name.trim();
    if (trimmed) onPick(trimmed);
  };
  return (
    <>
      {folders.map((f) => (
        <MenuItem
          key={f.name}
          icon={
            <Icon
              name="check"
              className={["size-4", currentFolder === f.name ? "opacity-100 text-[var(--color-accent)]" : "opacity-0"].join(" ")}
            />
          }
          onClick={() => onPick(f.name)}
        >
          {f.name}
        </MenuItem>
      ))}
      {folders.length > 0 ? <MenuSeparator /> : null}
      {creating ? (
        <input
          autoFocus
          className="mx-0.5 my-0.5 rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none"
          placeholder="Folder name…"
          maxLength={64}
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              submit();
            } else if (e.key === "Escape") {
              e.preventDefault();
              setCreating(false);
              setName("");
            }
          }}
        />
      ) : (
        <MenuItem icon={<Icon name="plus" className="size-4" />} onClick={() => setCreating(true)}>
          New folder…
        </MenuItem>
      )}
      {currentFolder && onRemove ? (
        <MenuItem icon={<Icon name="close" className="size-4" />} onClick={onRemove}>
          Remove from folder
        </MenuItem>
      ) : null}
    </>
  );
}

// CONV_DRAG_MIME is the custom drag payload type for a conversation row —
// namespaced so a project row only accepts our rows, never a stray text/file
// drag from outside the app.
const CONV_DRAG_MIME = "application/x-fleet-conversation";

// ── Project picker panel (per-row kebab) ─────────────────────────────────────
// The touch/keyboard counterpart of dragging a chat onto a rail project
// (#509 follow-up): pick a project to re-file into, or unfile. Unlike
// folders, projects are created in the Projects modal (they carry
// instructions/membership), so there is no inline "New project…" here.
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
              className={["size-4", currentProjectId === p.id ? "opacity-100 text-[var(--color-accent)]" : "opacity-0"].join(" ")}
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
          <MenuItem icon={<Icon name="close" className="size-4" />} onClick={onRemove}>
            Remove from project
          </MenuItem>
        </>
      ) : null}
    </>
  );
}

// ── Per-project kebab menu (rail Projects section) ───────────────────────────
// Sits where the chat count used to be. "Edit project…" opens the full modal
// editor (instructions/sharing/memories) and shows for everyone — members get
// the modal's read-only view. Rename and Delete mutate the project itself, so
// they render for the OWNER only (the store's owner-scoped statements would
// reject anyone else anyway; hiding them is honest UI, not the enforcement).
function ProjectKebab({
  projectName,
  isOwner,
  onEdit,
  onRename,
  onDelete,
}: {
  projectName: string;
  isOwner: boolean;
  onEdit: () => void;
  onRename: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const close = () => setOpen(false);
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Project options for ${projectName}`}
        title="Project options"
        className={[
          "hit-area pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open ? "opacity-100" : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100",
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((o) => !o);
        }}
      >
        <Icon name="dots" className="size-[1.1rem]" />
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={`Options for project ${projectName}`}
        className="min-w-[11rem]"
      >
        <MenuItem
          icon={<Icon name="briefcase" className="size-4" />}
          onClick={() => {
            close();
            onEdit();
          }}
        >
          Edit project…
        </MenuItem>
        {isOwner ? (
          <>
            <MenuItem
              icon={<Icon name="edit" className="size-4" />}
              onClick={() => {
                close();
                onRename();
              }}
            >
              Rename
            </MenuItem>
            <MenuSeparator />
            <MenuItem
              danger
              icon={<Icon name="trash" className="size-4" />}
              onClick={() => {
                close();
                onDelete();
              }}
            >
              Delete project
            </MenuItem>
          </>
        ) : null}
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
            <LabelChip key={l} name={l} removable onRemove={() => onRemove?.(l)} />
          ))}
        </div>
      ) : null}
      <input
        autoFocus
        className="rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none disabled:cursor-not-allowed disabled:opacity-60"
        placeholder={atMax ? `Max ${MAX_LABELS} labels` : "Add a label…"}
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
            Suggestions
          </span>
          <div className="flex flex-wrap gap-1.5">
            {fresh.map((s) => (
              <button key={s} type="button" className="conv-label-chip" style={labelChipStyle(s)} onClick={() => add(s)}>
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
  folders,
  allLabelNames,
  projects,
  onPin,
  onRename,
  onDownload,
  onPromote,
  onSetFolder,
  onSetLabels,
  onSetProject,
  onShare,
  onCopyLink,
  onUnshare,
  isShared,
  onSelect,
  onArchive,
  onDelete,
}: {
  conversation: ConversationSummary;
  folders: FolderSummary[];
  allLabelNames: string[];
  projects: Project[];
  onPin: () => void;
  onRename: () => void;
  onDownload: () => void;
  onPromote: () => void;
  onSetFolder: (folder: string | null) => void;
  onSetLabels: (labels: string[]) => void;
  onSetProject: (projectID: string | null) => void;
  onShare: () => void;
  onCopyLink: () => void;
  onUnshare: () => void;
  isShared: boolean;
  onSelect: () => void;
  onArchive: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [flyout, setFlyout] = useState<null | "folder" | "labels" | "project">(null);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  // The menu item that opened the active flyout — the flyout anchors to it and
  // focus returns here on Escape. Captured from the click's currentTarget.
  const flyoutAnchorRef = useRef<HTMLElement | null>(null);
  const close = () => {
    setOpen(false);
    setFlyout(null);
  };
  const toggleFlyout = (which: "folder" | "labels" | "project", el: HTMLElement) => {
    flyoutAnchorRef.current = el;
    setFlyout((cur) => (cur === which ? null : which));
  };
  const labels = conversation.labels ?? [];
  const caret = (
    <span aria-hidden="true" className="text-[0.62rem] text-[var(--color-text-muted)]">
      ▸
    </span>
  );
  const flyoutContent =
    flyout === "folder" ? (
      <FolderPanel
        folders={folders}
        currentFolder={conversation.folder || undefined}
        onPick={(name) => {
          onSetFolder(name);
          close();
        }}
        onRemove={() => {
          onSetFolder(null);
          close();
        }}
      />
    ) : flyout === "project" ? (
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
        aria-label={`Conversation options for ${conversation.title}`}
        title="Conversation options"
        className={[
          // Fixed, centered, padded square (~1.8rem) per the handoff .conv-kebab —
          // a rounded hover highlight around the centered icon, not hugging it.
          // hit-area extends the clickable box to ~2rem without growing the fill.
          "hit-area pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
          open ? "opacity-100" : "opacity-0 group-hover:opacity-100 group-focus-within:opacity-100",
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          setFlyout(null);
          setOpen((o) => !o);
        }}
      >
        <Icon name="dots" className="size-[1.1rem]" />
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={anchorRef}
        placement="bottom-end"
        label={`Options for ${conversation.title}`}
        className="min-w-[12rem]"
        flyout={flyoutContent}
        flyoutOpen={flyout !== null}
        flyoutAnchorRef={flyoutAnchorRef}
        onFlyoutClose={() => setFlyout(null)}
        flyoutLabel={flyout === "folder" ? "Add to folder" : flyout === "project" ? "Move to project" : "Labels"}
      >
        <MenuItem
          icon={<Icon name="pin" className="size-4" />}
          onClick={() => {
            onPin();
            close();
          }}
        >
          {conversation.pinned ? "Unpin" : "Pin"}
        </MenuItem>
        <MenuItem
          icon={<Icon name="edit" className="size-4" />}
          onClick={() => {
            close();
            onRename();
          }}
        >
          Rename
        </MenuItem>
        <MenuItem
          icon={<Icon name="folder" className="size-4" />}
          ariaHasPopup
          ariaExpanded={flyout === "folder"}
          trailing={caret}
          onClick={(e) => toggleFlyout("folder", e.currentTarget)}
        >
          Add to folder
        </MenuItem>
        {projects.length > 0 || conversation.project_id ? (
          <MenuItem
            icon={<Icon name="briefcase" className="size-4" />}
            ariaHasPopup
            ariaExpanded={flyout === "project"}
            trailing={caret}
            onClick={(e) => toggleFlyout("project", e.currentTarget)}
          >
            Move to project
          </MenuItem>
        ) : null}
        <MenuItem
          icon={<Icon name="tag" className="size-4" />}
          ariaHasPopup
          ariaExpanded={flyout === "labels"}
          trailing={caret}
          onClick={(e) => toggleFlyout("labels", e.currentTarget)}
        >
          Labels
        </MenuItem>
        <MenuSeparator />
        <MenuItem
          icon={<Icon name="download" className="size-4" />}
          onClick={() => {
            onDownload();
            close();
          }}
        >
          Download as JSON
        </MenuItem>
        <MenuItem
          icon={<Icon name="clock" className="size-4" />}
          onClick={() => {
            onPromote();
            close();
          }}
        >
          Make recurring task…
        </MenuItem>
        {isShared ? (
          <>
            <MenuItem
              icon={<ShareGlyph className="size-4" />}
              onClick={() => {
                onCopyLink();
                close();
              }}
            >
              Share link…
            </MenuItem>
            <MenuItem
              icon={<ShareGlyph off className="size-4" />}
              onClick={() => {
                onUnshare();
                close();
              }}
            >
              Stop sharing
            </MenuItem>
          </>
        ) : (
          <MenuItem
            icon={<ShareGlyph className="size-4" />}
            onClick={() => {
              onShare();
              close();
            }}
          >
            Share
          </MenuItem>
        )}
        <MenuItem
          icon={<Icon name="check-square" className="size-4" />}
          onClick={() => {
            onSelect();
            close();
          }}
        >
          Select…
        </MenuItem>
        <MenuSeparator />
        <MenuItem
          icon={
            <svg
              aria-hidden="true"
              viewBox="0 0 24 24"
              className="size-4"
              fill="none"
              stroke="currentColor"
              strokeWidth={1.8}
              strokeLinecap="round"
              strokeLinejoin="round"
            >
              <rect x="3" y="4" width="18" height="4" rx="1" />
              <path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" />
              <path d="M10 12h4" />
            </svg>
          }
          onClick={() => {
            onArchive();
            close();
          }}
        >
          Archive
        </MenuItem>
        <MenuItem
          danger
          icon={<Icon name="trash" className="size-4" />}
          onClick={() => {
            onDelete();
            close();
          }}
        >
          Delete
        </MenuItem>
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
  copied,
  onOpen,
  onToggleSelect,
  onCommitRename,
  onCancelRename,
  kebab,
  onDragStart,
}: {
  conversation: ConversationSummary;
  active: boolean;
  // focused = the keyboard j/k cursor is on this row (distinct from active =
  // the open conversation). Renders a ring so the cursor is visible.
  focused: boolean;
  streaming: boolean;
  editing: boolean;
  selecting: boolean;
  checked: boolean;
  copied: boolean;
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
      {editing ? (
        <input
          // Uncontrolled: remounts each time editing starts (it only renders
          // while editing), so defaultValue tracks the live title without a
          // sync effect. autoFocus + select-on-focus mirror the prior behavior.
          autoFocus
          aria-label={`Rename ${conversation.title}`}
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
          className={[
            "block w-full min-w-0 rounded-md py-2 pl-[0.55rem] pr-9 text-left text-[0.875rem] transition",
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
                    ? "border-[var(--color-primary)] bg-[var(--color-primary)]"
                    : "border-[var(--color-border-strong)]",
                ].join(" ")}
              >
                {checked ? <Icon name="check" className="size-[0.7rem] text-white" /> : null}
              </span>
            ) : null}
            {streaming ? (
              <span
                aria-label="Working"
                title="Working…"
                className="inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--color-accent)]"
              />
            ) : null}
            {conversation.lockdown ? (
              <Icon name="lock" className="size-3 shrink-0 text-[var(--color-accent)]" />
            ) : null}
            {copied ? (
              <span aria-label="Link copied" title="Link copied!">
                <Icon name="check" className="size-3 shrink-0 text-[var(--color-accent)]" />
              </span>
            ) : conversation.share_token ? (
              <span aria-label="Shared" title="Shared — read-only link is live">
                <ShareGlyph className="size-3 shrink-0 text-[var(--color-accent)]" />
              </span>
            ) : null}
            <span className="block truncate">{conversation.title}</span>
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
        <div className="absolute inset-y-0 right-1 flex items-center">{kebab}</div>
      ) : null}
    </div>
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
      className="flex w-full items-center gap-1.5 rounded-md px-2 py-1.5 text-[0.8rem] font-semibold text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
    >
      <Icon name={icon} className="size-3.5 shrink-0 text-[var(--color-accent)]" />
      <span className="min-w-0 flex-1 text-left">{label}</span>
      <Icon name="chevron-right" className={["size-3.5 transition", open ? "rotate-90" : ""].join(" ")} />
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
  filterFolder,
  setFilterFolder,
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
  toggleArchive,
  renameConversation,
  downloadConversation,
  promoteConversation,
  setPendingDeleteConversation,
  setConversationFolder,
  setConversationLabels,
  shareConversation,
  unshareConversation,
  copyShareLink,
  archivedConversations,
  showArchived,
  setShowArchived,
  updateAvailable,
  setConfirmBulkDelete,
  selectMode,
  selectedIds,
  onToggleSelection,
  onEnterSelectMode,
  onExitSelectMode,
  onBulkDelete,
  onBulkPin,
  onBulkMoveFolder,
  onBulkAddLabel,
  onOpenProjects,
  onCreateProject,
  onEditProject,
  onRenameProject,
  onDeleteProject,
  projects,
  onMoveToProject,
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
  filterFolder: string | null;
  setFilterFolder: Dispatch<SetStateAction<string | null>>;
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
  loadConversation: (conversationId: string, options?: { preserveScroll?: boolean }) => Promise<void>;
  streamingConvs: Set<string>;
  togglePin: (conversation: ConversationSummary) => Promise<void>;
  toggleArchive: (conversation: ConversationSummary, archived: boolean) => Promise<void>;
  renameConversation: (conversationId: string, nextTitle: string) => Promise<boolean>;
  downloadConversation: (conversation: ConversationSummary) => Promise<void>;
  promoteConversation: (conversation: ConversationSummary) => Promise<void>;
  setPendingDeleteConversation: Dispatch<SetStateAction<PendingDeleteConversation | null>>;
  setConversationFolder: (conversationId: string, folder: string | null) => void;
  setConversationLabels: (conversationId: string, labels: string[]) => void;
  // Read-only sharing (#226): issue+copy a public link, revoke it, or re-copy.
  shareConversation: (conversation: ConversationSummary) => Promise<boolean>;
  unshareConversation: (conversation: ConversationSummary) => Promise<void>;
  copyShareLink: (conversation: ConversationSummary) => Promise<boolean>;
  archivedConversations: ConversationSummary[];
  showArchived: boolean;
  setShowArchived: Dispatch<SetStateAction<boolean>>;
  updateAvailable: boolean;
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
  onBulkMoveFolder: (folder: string) => void;
  onBulkAddLabel: (label: string) => void;
  // Opens the Projects modal (#509). Lives in the rail (like Claude/ChatGPT)
  // rather than the page header; ChatExperience owns the modal state.
  onOpenProjects: () => void;
  // Opens the modal straight into the new-project form (the section
  // header's + button).
  onCreateProject: () => void;
  // Opens the modal with this project selected (the project kebab's
  // "Edit project…" — full editor incl. instructions/sharing/memories).
  onEditProject: (projectID: string) => void;
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
  // Re-files a conversation into a project ("" = unfile) — drag-and-drop and
  // the kebab's "Move to project" both land here; the parent owns the
  // optimistic state + POST.
  onMoveToProject: (conversationId: string, projectID: string) => void;
}) {
  const [editingId, setEditingId] = useState<string | null>(null);
  const [foldersOpen, setFoldersOpen] = useState(true);
  // Projects section (#509 follow-up): the section itself starts open (its
  // presence is the affordance); each project starts collapsed and remembers
  // its expansion for the session only, like Folders/Labels above.
  const [projectsSectionOpen, setProjectsSectionOpen] = useState(true);
  const [expandedProjects, setExpandedProjects] = useState<Set<string>>(new Set());
  // The project being renamed inline (kebab → Rename), mirroring the chat
  // rows' editingId.
  const [renamingProjectId, setRenamingProjectId] = useState<string | null>(null);
  // The project row a conversation drag is currently hovering — drives the
  // drop-target highlight.
  const [dragOverProject, setDragOverProject] = useState<string | null>(null);
  const [labelsOpen, setLabelsOpen] = useState(true);
  const [bulkPanel, setBulkPanel] = useState<"none" | "folder" | "labels">("none");
  const bulkFolderRef = useRef<HTMLButtonElement | null>(null);
  const bulkLabelsRef = useRef<HTMLButtonElement | null>(null);
  // Transient "copied" feedback for share/copy actions (#226), keyed by conv id.
  // The only effect just clears the pending timer on unmount — setState happens
  // in handlers + the timeout callback, never synchronously in the effect body.
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const copiedTimer = useRef<number | null>(null);
  const flashCopied = (id: string) => {
    setCopiedId(id);
    if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
    copiedTimer.current = window.setTimeout(() => setCopiedId(null), 1500);
  };
  useEffect(
    () => () => {
      if (copiedTimer.current) window.clearTimeout(copiedTimer.current);
    },
    [],
  );

  // The parent's `r` shortcut asks for an inline rename by bumping renameSignal.
  // We open the inline editor for the requested id when the nonce changes,
  // comparing *during render* (React's "reset state when a prop changes"
  // pattern) so it fires once per request without a setState-in-effect cascade.
  const [seenRenameNonce, setSeenRenameNonce] = useState<number | null>(renameSignal?.nonce ?? null);
  if (renameSignal && renameSignal.nonce !== seenRenameNonce) {
    setSeenRenameNonce(renameSignal.nonce);
    setEditingId(renameSignal.id);
  }

  const folders = deriveFolders(conversations);
  const labelSummaries: LabelSummary[] = deriveLabels(conversations);
  const allLabelNames = labelSummaries.map((l) => l.name);
  const pinned = pinnedUnfiled(conversations);
  const recent = recentUnfiled(conversations);
  const projectTree = projectGroups(conversations, projects);
  const filtering = computeIsFiltering({ folder: filterFolder, labels: filterLabels, query: sidebarQuery });
  const searching = sidebarQuery.trim().length > 0;

  const selecting = selectMode;
  const largeSelection = selectedIds.size > 50;
  const railCollapsed = collapse.collapsed;

  const toggleLabelFilter = (name: string) =>
    setFilterLabels((ls) => (ls.includes(name) ? ls.filter((l) => l !== name) : [...ls, name]));
  const clearFilters = () => {
    setFilterFolder(null);
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
      folders={folders}
      allLabelNames={allLabelNames}
      projects={projects}
      onPin={() => void togglePin(conversation)}
      onRename={() => setEditingId(conversation.id)}
      onDownload={() => void downloadConversation(conversation)}
      onPromote={() => void promoteConversation(conversation)}
      onSetFolder={(folder) => setConversationFolder(conversation.id, folder)}
      onSetLabels={(labels) => setConversationLabels(conversation.id, labels)}
      onSetProject={(projectID) => onMoveToProject(conversation.id, projectID ?? "")}
      isShared={Boolean(conversation.share_token)}
      onShare={() => void shareConversation(conversation).then((ok) => ok && flashCopied(conversation.id))}
      onCopyLink={() => void copyShareLink(conversation).then((ok) => ok && flashCopied(conversation.id))}
      onUnshare={() => void unshareConversation(conversation)}
      onSelect={() => onEnterSelectMode(conversation.id)}
      onArchive={() => void toggleArchive(conversation, true)}
      onDelete={() => setPendingDeleteConversation({ id: conversation.id, title: conversation.title })}
    />
  );

  const renderRow = (conversation: ConversationSummary) => (
    <ConvRow
      key={conversation.id}
      conversation={conversation}
      active={activeConversationId === conversation.id}
      focused={focusedConversationId === conversation.id}
      streaming={streamingConvs.has(conversation.id)}
      editing={editingId === conversation.id}
      selecting={selecting}
      checked={selectedIds.has(conversation.id)}
      copied={copiedId === conversation.id}
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

  const foldersSection =
    folders.length > 0 ? (
      <div className="mb-1">
        <SectionToggle icon="folder" label="Folders" open={foldersOpen} onToggle={() => setFoldersOpen((o) => !o)} />
        {foldersOpen
          ? folders.map((f) => (
              <button
                key={f.name}
                type="button"
                aria-pressed={filterFolder === f.name}
                onClick={() => setFilterFolder(filterFolder === f.name ? null : f.name)}
                className={[
                  "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.875rem] transition",
                  filterFolder === f.name
                    ? "bg-[var(--rail-active)] text-[var(--color-text-primary)]"
                    : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
                ].join(" ")}
              >
                <span className="min-w-0 flex-1 truncate text-left">{f.name}</span>
                <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                  {f.count}
                </span>
              </button>
            ))
          : null}
      </div>
    ) : null;

  // Projects section (#509 follow-up): the top MAX_RAIL_PROJECTS projects as
  // expandable groups; each project row is also the drop target for a
  // conversation drag. The header always renders — its + creates a project
  // (opens the modal straight on the new-project form), and each row's kebab
  // (where the chat count used to be) carries edit/rename/delete. The full
  // list stays reachable via the empty-state row and the modal itself.
  const projectsSection = (
    <div className="mb-1">
      <div className="flex items-center gap-1">
        <div className="min-w-0 flex-1">
          <SectionToggle
            icon="briefcase"
            label="Projects"
            open={projectsSectionOpen}
            onToggle={() => setProjectsSectionOpen((o) => !o)}
          />
        </div>
        <button
          type="button"
          aria-label="Create project"
          data-tip-top="Create project"
          className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={onCreateProject}
        >
          <Icon name="plus" className="size-4" />
        </button>
      </div>
      {projectsSectionOpen ? (
        projectTree.length === 0 ? (
          <button
            type="button"
            className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
            onClick={onCreateProject}
          >
            <Icon name="plus" className="size-3.5 shrink-0" />
            New project…
          </button>
        ) : (
          projectTree.map(({ project, chats }) => {
            const expanded = expandedProjects.has(project.id);
            const dropReady = dragOverProject === project.id;
            return (
              <div key={project.id}>
                {renamingProjectId === project.id ? (
                  <input
                    // Uncontrolled like the chat rows' rename input: mounts
                    // only while renaming, so defaultValue tracks the live
                    // name without a sync effect.
                    autoFocus
                    aria-label={`Rename project ${project.name}`}
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
                      dropReady ? "bg-[var(--rail-hover)] ring-1 ring-inset ring-[var(--color-accent)]" : "",
                    ].join(" ")}
                    onDragOver={(e) => {
                      if (!e.dataTransfer.types.includes(CONV_DRAG_MIME)) return;
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
                      setExpandedProjects((s) => new Set(s).add(project.id));
                    }}
                  >
                    <button
                      type="button"
                      aria-expanded={expanded}
                      aria-label={`Project ${project.name} (${chats.length} chats)`}
                      className={[
                        "flex w-full items-center gap-2 rounded-md py-1.5 pl-2 pr-9 text-[0.875rem] transition",
                        dropReady
                          ? "text-[var(--color-text-primary)]"
                          : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
                      ].join(" ")}
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
                        className={["size-3 shrink-0 transition", expanded ? "rotate-90" : ""].join(" ")}
                      />
                      <span className="min-w-0 flex-1 truncate text-left">{project.name}</span>
                    </button>
                    <div className="absolute inset-y-0 right-1 flex items-center">
                      <ProjectKebab
                        projectName={project.name}
                        isOwner={project.owner_email === userEmail}
                        onEdit={() => onEditProject(project.id)}
                        onRename={() => setRenamingProjectId(project.id)}
                        onDelete={() => onDeleteProject(project.id)}
                      />
                    </div>
                  </div>
                )}
                {expanded ? (
                  chats.length === 0 ? (
                    <p className="py-1 pl-7 pr-2 text-[0.78rem] text-[var(--color-text-muted)]">
                      No chats yet — drag one here.
                    </p>
                  ) : (
                    <div className="ml-3 border-l border-[var(--color-border)] pl-1">{chats.map(renderRow)}</div>
                  )
                ) : null}
              </div>
            );
          })
        )
      ) : null}
    </div>
  );

  const labelsSection =
    labelSummaries.length > 0 ? (
      <div className="mb-1">
        <SectionToggle icon="tag" label="Labels" open={labelsOpen} onToggle={() => setLabelsOpen((o) => !o)} />
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
                <span className="min-w-0 flex-1 truncate text-left">{l.name}</span>
                <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                  {l.count}
                </span>
              </button>
            ))
          : null}
      </div>
    ) : null;

  return (
    <NavRail
      activeView="chat"
      brandName={branding.app_name}
      sidebarOpen={sidebarOpen}
      setSidebarOpen={setSidebarOpen}
      collapse={collapse}
      account={{ email: userEmail, onSignOut }}
      footer={
        <div className={["grid gap-1 pt-1", railCollapsed ? "sm:hidden" : ""].join(" ")}>
          {updateAvailable ? (
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-md border border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 px-2 py-1.5 text-left text-[0.75rem] font-medium text-[var(--color-accent)] transition hover:bg-[var(--color-accent)]/20 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
              onClick={() => window.location.reload()}
              title="A newer version of the app has been deployed. Click to refresh and load it."
            >
              <Icon name="refresh" className="size-3.5 shrink-0" />
              Chat has been updated — click to refresh
            </button>
          ) : null}
          {conversations.some((c) => !c.pinned) ? (
            <button
              type="button"
              className="flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-left text-[0.75rem] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-danger)]"
              onClick={() => setConfirmBulkDelete(true)}
              title="Delete every unpinned conversation"
            >
              <Icon name="trash" className="size-3.5 shrink-0" />
              Delete all unpinned
            </button>
          ) : null}
        </div>
      }
    >
      {/* Projects section — ABOVE the New-chat row (the rail's primary nav
          block). Wide-collapsed (≥sm) it hides with the rest of the wide-only
          content; the max-height guard keeps a deep expanded tree from
          pushing New chat and the list off-screen. */}
      <div className={["max-h-[40vh] overflow-y-auto", railCollapsed ? "sm:hidden" : ""].join(" ")}>
        {projectsSection}
      </div>

      {/* New chat / sealed-chat row — collapsed (≥sm) it stacks as icon-only
          2.5rem buttons with data-tip labels, per the design's .rail-new-row. */}
      <div
        className={[
          "flex gap-1.5",
          railCollapsed ? "sm:flex-col sm:items-center sm:gap-[0.4rem]" : "",
        ].join(" ")}
      >
        {serverConfig.lockdownOnly ? (
          <button
            type="button"
            className={[
              "flex flex-1 items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]",
              railCollapsed ? "sm:size-10 sm:flex-none sm:gap-0 sm:p-0" : "",
            ].join(" ")}
            title="New chat — every chat on this server is sealed (sandboxed, vetted model only)"
            aria-label="New chat — every chat on this server is sealed (sandboxed, vetted model only)"
            data-tip={railCollapsed ? "New chat" : undefined}
            onClick={() => clearConversation({ lockdown: true })}
          >
            <Icon name="lock" className="size-4" />
            <span className={railCollapsed ? "sm:hidden" : ""}>New chat</span>
          </button>
        ) : (
          <button
            type="button"
            className={[
              "flex flex-1 items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]",
              railCollapsed ? "sm:size-10 sm:flex-none sm:gap-0 sm:p-0" : "",
            ].join(" ")}
            title="New chat"
            aria-label="New chat"
            data-tip={railCollapsed ? "New chat" : undefined}
            onClick={() => clearConversation()}
          >
            <Icon name="plus" className="size-4" />
            <span className={railCollapsed ? "sm:hidden" : ""}>New chat</span>
          </button>
        )}
        {serverConfig.lockdownAvailable && !serverConfig.lockdownOnly ? (
          <SealedNewChatButton onClick={() => clearConversation({ lockdown: true })} />
        ) : null}
      </div>

      {/* Projects (#509) — in the expanded rail (and the <sm drawer) the
          Projects SECTION in the list below is the entry point, so this
          standalone row only exists for the collapsed (≥sm) icon strip,
          where the list is hidden: an icon-only square with a data-tip,
          matching the New-chat row above. */}
      {railCollapsed ? (
        <button
          type="button"
          className="mt-1.5 hidden w-full items-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:flex sm:size-10 sm:justify-center sm:p-0"
          title="Projects"
          aria-label="Projects"
          data-tip="Projects"
          onClick={() => {
            setSidebarOpen(false);
            onOpenProjects();
          }}
        >
          <Icon name="briefcase" className="size-4" />
        </button>
      ) : null}

      {/* Search filter — the design's .chat-search: leading magnifier icon and
          a custom clear button (type="text" so the native search clear never
          doubles it). */}
      <div className={["relative mt-2 flex items-center", railCollapsed ? "sm:hidden" : ""].join(" ")}>
        <Icon
          name="search"
          className="pointer-events-none absolute left-[0.65rem] size-[0.95rem] text-[var(--color-text-muted)]"
        />
        <input
          ref={searchRef}
          type="search"
          value={sidebarQuery}
          onChange={(e) => setSidebarQuery(e.target.value)}
          placeholder="Search chats…"
          aria-label="Search chats"
          className="search-input-no-native-clear min-h-[2.2rem] w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-[0.4rem] pl-[2.1rem] pr-8 text-[0.85rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]"
        />
        {sidebarQuery ? (
          <button
            type="button"
            aria-label="Clear search"
            className="absolute right-[0.4rem] inline-flex size-6 items-center justify-center rounded-[var(--radius-pill)] text-[var(--color-text-muted)] transition before:absolute before:left-1/2 before:top-1/2 before:size-8 before:-translate-x-1/2 before:-translate-y-1/2 before:content-[''] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={() => setSidebarQuery("")}
          >
            <Icon name="close" className="size-[0.8rem]" />
          </button>
        ) : null}
      </div>

      {/* Active-filter chips */}
      {filterFolder || filterLabels.length > 0 ? (
        <div
          className={[
            "mt-2 flex items-center gap-1.5 motion-safe:animate-[filter-in_var(--motion-fast)_ease_both]",
            railCollapsed ? "sm:hidden" : "",
          ].join(" ")}
        >
          <div className="flex min-w-0 flex-1 flex-wrap gap-1.5">
            {filterFolder ? (
              <span className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.78rem] text-[var(--color-text-primary)]">
                <span className="text-[var(--color-text-muted)]">Folder:</span> {filterFolder}
                <button
                  type="button"
                  aria-label="Remove folder filter"
                  className="hit-area inline-flex size-4 items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                  onClick={() => setFilterFolder(null)}
                >
                  <Icon name="close" className="size-2.5" />
                </button>
              </span>
            ) : null}
            {filterLabels.map((l) => (
              <span
                key={l}
                className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.78rem] text-[var(--color-text-primary)]"
              >
                <span className="text-[var(--color-text-muted)]">Label:</span> {l}
                <button
                  type="button"
                  aria-label={`Remove label filter ${l}`}
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
            Clear
          </button>
        </div>
      ) : null}

      {/* Conversation list */}
      <div className={["mt-2 flex-1 overflow-y-auto", railCollapsed ? "sm:hidden" : ""].join(" ")}>
        {isLoadingHistory ? (
          <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : filtering ? (
          <>
            {filteredConversations.length === 0 ? (
              <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">
                {searching ? `No chats match “${sidebarQuery.trim()}”.` : "Nothing matches this filter."}
              </p>
            ) : (
              filteredConversations.map(renderRow)
            )}
            <div className="mt-3 border-t border-[var(--color-border)] pt-2 opacity-70 transition focus-within:opacity-100 hover:opacity-100">
              <p className="px-2 pb-1 text-[0.6rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">Refine</p>
              {foldersSection}
              {labelsSection}
            </div>
          </>
        ) : (
          <>
            {pinned.length > 0 ? (
              <div className="mb-1">
                <div className="flex items-center gap-1.5 px-2 py-1.5 text-[0.8rem] font-semibold text-[var(--color-text-secondary)]">
                  <Icon name="pin" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
                  Pinned
                </div>
                {pinned.map(renderRow)}
              </div>
            ) : null}
            {foldersSection}
            {labelsSection}
            <div className="mb-1">
              <div className="flex items-center gap-1.5 px-2 py-1.5 text-[0.8rem] font-semibold text-[var(--color-text-secondary)]">
                Chats
                <RecentInfoButton />
              </div>
              {recent.length === 0 ? (
                <p className="px-2 py-1.5 text-[0.82rem] text-[var(--color-text-muted)]">No saved chats yet.</p>
              ) : (
                recent.map(renderRow)
              )}
            </div>
          </>
        )}

        {/* Archived (collapsible) */}
        {archivedConversations.length > 0 ? (
          <div className="mt-3 border-t border-[var(--color-border)] pt-2">
            <button
              type="button"
              aria-expanded={showArchived}
              aria-label={`Archived conversations (${archivedConversations.length})`}
              className="flex w-full items-center gap-1.5 rounded-md px-2 py-1 text-[0.6875rem] font-medium text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)]"
              onClick={() => setShowArchived((v) => !v)}
            >
              <Icon name={showArchived ? "chevron-down" : "chevron-right"} className="size-3 shrink-0" />
              Archived ({archivedConversations.length})
            </button>
            {showArchived
              ? archivedConversations.map((conversation) => (
                  <div
                    key={conversation.id}
                    className={[
                      "group relative rounded-md transition",
                      activeConversationId === conversation.id ? "bg-[var(--rail-active)]" : "hover:bg-[var(--rail-hover)]",
                    ].join(" ")}
                  >
                    <button
                      type="button"
                      className="block w-full min-w-0 rounded-md py-1.5 pl-3 pr-20 text-left text-[0.8125rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-secondary)]"
                      onClick={() => void loadConversation(conversation.id)}
                    >
                      <span className="block truncate italic">{conversation.title}</span>
                    </button>
                    <div className="absolute inset-y-0 right-1 flex items-center gap-1 opacity-0 transition group-hover:opacity-100 group-focus-within:opacity-100">
                      <button
                        type="button"
                        aria-label={`Unarchive ${conversation.title}`}
                        title="Unarchive"
                        className="inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                        onClick={() => void toggleArchive(conversation, false)}
                      >
                        <svg
                          aria-hidden="true"
                          viewBox="0 0 24 24"
                          className="size-3.5"
                          fill="none"
                          stroke="currentColor"
                          strokeWidth={1.8}
                          strokeLinecap="round"
                          strokeLinejoin="round"
                        >
                          <rect x="3" y="4" width="18" height="4" rx="1" />
                          <path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" />
                          <path d="M12 18v-6" />
                          <path d="M9.5 14.5 12 12l2.5 2.5" />
                        </svg>
                      </button>
                      <button
                        type="button"
                        aria-label={`Delete ${conversation.title}`}
                        className="inline-flex size-10 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] sm:size-7"
                        onClick={() =>
                          setPendingDeleteConversation({ id: conversation.id, title: conversation.title })
                        }
                      >
                        <Icon name="trash" className="size-3.5" />
                      </button>
                    </div>
                  </div>
                ))
              : null}
          </div>
        ) : null}
      </div>
      {/* Multi-select bulk bar (#279, the design's .bulk-bar): pinned to the
          rail's foot below the conversation list — count + 1.9rem icon actions with data-tip-top tooltips. Move-to-folder / Add-label
          reuse the kebab's panels (including their inline create inputs) in
          menus that open above the bar. Actions disable at zero selected. */}
      {selecting ? (
        <>
          <div
            className={[
              "mt-2 flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-[0.4rem] pl-[0.65rem] pr-[0.3rem] motion-safe:animate-pop-up",
              railCollapsed ? "sm:hidden" : "",
            ].join(" ")}
          >
            <span className="min-w-0 flex-1 text-[0.8rem] font-medium tabular-nums text-[var(--color-text-primary)]">
              {selectedIds.size} selected
            </span>
            <div className="flex items-center gap-[0.15rem]">
              <button
                ref={bulkFolderRef}
                type="button"
                aria-label="Move to folder"
                aria-haspopup="menu"
                aria-expanded={bulkPanel === "folder"}
                data-tip-top="Move to folder"
                disabled={selectedIds.size === 0}
                className={BULK_BTN_CLASS}
                onClick={() => setBulkPanel((p) => (p === "folder" ? "none" : "folder"))}
              >
                <Icon name="folder" className="size-[0.95rem]" />
              </button>
              <button
                type="button"
                aria-label="Pin selected"
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
                aria-label="Add label"
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
                aria-label="Delete selected"
                data-tip-top="Delete"
                disabled={selectedIds.size === 0}
                className={[
                  BULK_BTN_CLASS,
                  "enabled:hover:bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)] enabled:hover:text-[var(--color-danger)]",
                ].join(" ")}
                onClick={onBulkDelete}
              >
                <Icon name="trash" className="size-[0.95rem]" />
              </button>
              <button
                type="button"
                aria-label="Exit selection"
                data-tip-top="Cancel"
                className={BULK_BTN_CLASS}
                onClick={onExitSelectMode}
              >
                <Icon name="close" className="size-[0.95rem]" />
              </button>
            </div>
            <Menu
              open={bulkPanel === "folder"}
              onClose={() => setBulkPanel("none")}
              anchorRef={bulkFolderRef}
              placement="top-end"
              label="Move selected to folder"
            >
              <FolderPanel
                folders={folders}
                onPick={(name) => {
                  onBulkMoveFolder(name);
                  setBulkPanel("none");
                }}
              />
            </Menu>
            <Menu
              open={bulkPanel === "labels"}
              onClose={() => setBulkPanel("none")}
              anchorRef={bulkLabelsRef}
              placement="top-end"
              label="Add label to selected"
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
              Selecting {selectedIds.size} conversations — large bulk deletes are permanent.
            </p>
          ) : null}
        </>
      ) : null}

    </NavRail>
  );
}
