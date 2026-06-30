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
import type { Dispatch, ReactNode, RefObject, SetStateAction } from "react";
import type { ClientBranding } from "@/app/lib/useClientConfig";
import { NavRail } from "@/app/shared/ui/NavRail";
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
  recentUnfiled,
  type FolderSummary,
  type LabelSummary,
} from "./conversationOrganization";
import type { ConversationSummary, PendingDeleteConversation, ServerConfig } from "./chat-experience";

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
          className="ml-0.5 inline-flex size-3.5 items-center justify-center rounded-full text-current opacity-70 transition hover:bg-white/20 hover:opacity-100"
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
          <span className="text-[0.625rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
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
  onPin,
  onRename,
  onDownload,
  onSetFolder,
  onSetLabels,
  onShare,
  onCopyLink,
  onUnshare,
  isShared,
  onArchive,
  onDelete,
}: {
  conversation: ConversationSummary;
  folders: FolderSummary[];
  allLabelNames: string[];
  onPin: () => void;
  onRename: () => void;
  onDownload: () => void;
  onSetFolder: (folder: string | null) => void;
  onSetLabels: (labels: string[]) => void;
  onShare: () => void;
  onCopyLink: () => void;
  onUnshare: () => void;
  isShared: boolean;
  onArchive: () => void;
  onDelete: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [flyout, setFlyout] = useState<null | "folder" | "labels">(null);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  // The menu item that opened the active flyout — the flyout anchors to it and
  // focus returns here on Escape. Captured from the click's currentTarget.
  const flyoutAnchorRef = useRef<HTMLElement | null>(null);
  const close = () => {
    setOpen(false);
    setFlyout(null);
  };
  const toggleFlyout = (which: "folder" | "labels", el: HTMLElement) => {
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
          "pointer-events-auto inline-flex size-[1.8rem] items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none",
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
        flyoutLabel={flyout === "folder" ? "Add to folder" : "Labels"}
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
        {isShared ? (
          <>
            <MenuItem
              icon={<ShareGlyph className="size-4" />}
              onClick={() => {
                onCopyLink();
                close();
              }}
            >
              Copy share link
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
}: {
  conversation: ConversationSummary;
  active: boolean;
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
}) {
  const labels = conversation.labels ?? [];
  const shown = labels.slice(0, 2);
  const extra = labels.length - shown.length;

  return (
    <div
      className={[
        "group relative rounded-md transition",
        active ? "bg-[var(--rail-active)]" : "hover:bg-[var(--rail-hover)]",
        checked ? "ring-1 ring-inset ring-[var(--color-accent)]/50" : "",
      ].join(" ")}
    >
      <button
        type="button"
        aria-label={checked ? `Deselect ${conversation.title}` : `Select ${conversation.title}`}
        aria-pressed={checked}
        className={[
          "absolute left-0.5 top-1/2 z-10 inline-flex size-5 -translate-y-1/2 items-center justify-center rounded border text-[0.75rem] transition sm:size-4",
          checked
            ? "border-[var(--color-accent)] bg-[var(--color-accent)] text-white opacity-100"
            : "border-[var(--color-border-strong)] text-transparent opacity-0 hover:border-[var(--color-accent)] group-hover:opacity-100 group-focus-within:opacity-100",
          selecting ? "opacity-100" : "",
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          onToggleSelect();
        }}
      >
        {checked ? "✓" : ""}
      </button>

      {editing ? (
        <input
          // Uncontrolled: remounts each time editing starts (it only renders
          // while editing), so defaultValue tracks the live title without a
          // sync effect. autoFocus + select-on-focus mirror the prior behavior.
          autoFocus
          aria-label={`Rename ${conversation.title}`}
          className="mx-7 my-1 w-[calc(100%-3.5rem)] rounded-[0.4rem] border border-[var(--color-accent)] bg-[var(--color-surface-1)] px-2 py-1 text-[0.8125rem] text-[var(--color-text-primary)] outline-none"
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
          className={[
            "block w-full min-w-0 rounded-md py-1.5 pl-7 pr-10 text-left text-[0.8125rem] transition",
            active
              ? "text-[var(--color-text-primary)]"
              : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
          ].join(" ")}
          onClick={onOpen}
          title={conversation.title}
        >
          <span className="flex min-w-0 items-center gap-1.5">
            {streaming ? (
              <span
                aria-label="Working"
                title="Working…"
                className="inline-block size-1.5 shrink-0 animate-pulse rounded-full bg-[var(--color-accent)]"
              />
            ) : null}
            {conversation.lockdown ? <Icon name="lock" className="size-3 shrink-0 text-[var(--color-accent)]" /> : null}
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
          {labels.length > 0 ? (
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

      {!editing ? (
        <div className="absolute inset-y-0 right-1 flex items-center">{kebab}</div>
      ) : null}
    </div>
  );
}

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
      className="flex w-full items-center gap-1.5 rounded-md px-2 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
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
  loadConversation,
  streamingConvs,
  togglePin,
  toggleArchive,
  renameConversation,
  downloadConversation,
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
  selectedIds,
  onToggleSelection,
  onSelectAllVisible,
  onClearSelection,
  onBulkDelete,
  onBulkPin,
  onBulkUnpin,
  onBulkMoveFolder,
  onBulkAddLabel,
}: {
  sidebarOpen: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
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
  loadConversation: (conversationId: string, options?: { preserveScroll?: boolean }) => Promise<void>;
  streamingConvs: Set<string>;
  togglePin: (conversation: ConversationSummary) => Promise<void>;
  toggleArchive: (conversation: ConversationSummary, archived: boolean) => Promise<void>;
  renameConversation: (conversationId: string, nextTitle: string) => Promise<boolean>;
  downloadConversation: (conversation: ConversationSummary) => Promise<void>;
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
  selectedIds: Set<string>;
  onToggleSelection: (id: string) => void;
  onSelectAllVisible: () => void;
  onClearSelection: () => void;
  onBulkDelete: () => void;
  onBulkPin: () => void;
  onBulkUnpin: () => void;
  onBulkMoveFolder: (folder: string) => void;
  onBulkAddLabel: (label: string) => void;
}) {
  const [editingId, setEditingId] = useState<string | null>(null);
  const [foldersOpen, setFoldersOpen] = useState(true);
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

  const folders = deriveFolders(conversations);
  const labelSummaries: LabelSummary[] = deriveLabels(conversations);
  const allLabelNames = labelSummaries.map((l) => l.name);
  const pinned = pinnedUnfiled(conversations);
  const recent = recentUnfiled(conversations);
  const filtering = computeIsFiltering({ folder: filterFolder, labels: filterLabels, query: sidebarQuery });
  const searching = sidebarQuery.trim().length > 0;

  const selecting = selectedIds.size > 0;
  const largeSelection = selectedIds.size > 50;

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
      onPin={() => void togglePin(conversation)}
      onRename={() => setEditingId(conversation.id)}
      onDownload={() => void downloadConversation(conversation)}
      onSetFolder={(folder) => setConversationFolder(conversation.id, folder)}
      onSetLabels={(labels) => setConversationLabels(conversation.id, labels)}
      isShared={Boolean(conversation.share_token)}
      onShare={() => void shareConversation(conversation).then((ok) => ok && flashCopied(conversation.id))}
      onCopyLink={() => void copyShareLink(conversation).then((ok) => ok && flashCopied(conversation.id))}
      onUnshare={() => void unshareConversation(conversation)}
      onArchive={() => void toggleArchive(conversation, true)}
      onDelete={() => setPendingDeleteConversation({ id: conversation.id, title: conversation.title })}
    />
  );

  const renderRow = (conversation: ConversationSummary) => (
    <ConvRow
      key={conversation.id}
      conversation={conversation}
      active={activeConversationId === conversation.id}
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
                  "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.8125rem] transition",
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
                  "flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-[0.8125rem] transition",
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
      account={{ email: userEmail, onSignOut }}
      footer={
        <div className="grid gap-1 pt-1">
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
      {/* New chat / sealed-chat row */}
      <div className="flex gap-1.5">
        {serverConfig.lockdownOnly ? (
          <button
            type="button"
            className="flex flex-1 items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]"
            title="New chat — every chat on this server is sealed (sandboxed, vetted model only)"
            aria-label="New chat — every chat on this server is sealed (sandboxed, vetted model only)"
            onClick={() => clearConversation({ lockdown: true })}
          >
            <Icon name="lock" className="size-4 text-[var(--color-accent)]" /> New chat
          </button>
        ) : (
          <button
            type="button"
            className="flex flex-1 items-center justify-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[0.8125rem] font-semibold text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]"
            title="New chat"
            aria-label="New chat"
            onClick={() => clearConversation()}
          >
            <Icon name="plus" className="size-4" /> New chat
          </button>
        )}
        {serverConfig.lockdownAvailable && !serverConfig.lockdownOnly ? (
          <button
            type="button"
            className="inline-flex size-10 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)]"
            title="New sealed chat — sandboxed, vetted model, nothing leaves"
            aria-label="New sealed chat — sandboxed, vetted model, nothing leaves"
            onClick={() => clearConversation({ lockdown: true })}
          >
            <Icon name="lock" className="size-4 text-[var(--color-accent)]" />
          </button>
        ) : null}
      </div>

      {/* Search filter */}
      <div className="mt-2">
        <input
          ref={searchRef}
          type="search"
          value={sidebarQuery}
          onChange={(e) => setSidebarQuery(e.target.value)}
          placeholder="Search chats…"
          aria-label="Search chats"
          className="w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2.5 py-1.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus:border-[var(--color-accent)]"
        />
      </div>

      {/* Active-filter chips */}
      {filterFolder || filterLabels.length > 0 ? (
        <div className="mt-2 flex items-center gap-1.5">
          <div className="flex min-w-0 flex-1 flex-wrap gap-1.5">
            {filterFolder ? (
              <span className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.72rem] text-[var(--color-text-primary)]">
                <span className="text-[var(--color-text-muted)]">Folder:</span> {filterFolder}
                <button
                  type="button"
                  aria-label="Remove folder filter"
                  className="inline-flex size-4 items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
                  onClick={() => setFilterFolder(null)}
                >
                  <Icon name="close" className="size-2.5" />
                </button>
              </span>
            ) : null}
            {filterLabels.map((l) => (
              <span
                key={l}
                className="inline-flex items-center gap-1 rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] py-0.5 pl-2 pr-1 text-[0.72rem] text-[var(--color-text-primary)]"
              >
                <span className="text-[var(--color-text-muted)]">Label:</span> {l}
                <button
                  type="button"
                  aria-label={`Remove label filter ${l}`}
                  className="inline-flex size-4 items-center justify-center rounded-full text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
                  onClick={() => toggleLabelFilter(l)}
                >
                  <Icon name="close" className="size-2.5" />
                </button>
              </span>
            ))}
          </div>
          <button
            type="button"
            className="shrink-0 rounded px-1.5 py-0.5 text-[0.72rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
            onClick={clearFilters}
          >
            Clear
          </button>
        </div>
      ) : null}

      {/* Multi-select bulk action bar (#279) — reuses the kebab's folder/label surfaces. */}
      {selecting ? (
        <div className="mt-2 rounded-md border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-2">
          <div className="flex items-center gap-1.5">
            <span className="text-[0.75rem] font-medium text-[var(--color-text-secondary)]">
              {selectedIds.size} selected
            </span>
            <button
              type="button"
              className="ml-auto text-[0.6875rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
              onClick={onSelectAllVisible}
            >
              Select all
            </button>
            <button
              type="button"
              className="text-[0.6875rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)]"
              onClick={onClearSelection}
            >
              Clear
            </button>
          </div>
          <div className="mt-1.5 flex flex-wrap gap-1">
            <button
              type="button"
              className="rounded bg-[var(--color-danger)] px-2 py-1 text-[0.6875rem] font-medium text-white transition hover:opacity-90"
              onClick={onBulkDelete}
            >
              Delete {selectedIds.size}
            </button>
            <button
              type="button"
              className="rounded border border-[var(--color-border-strong)] px-2 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)]"
              onClick={onBulkPin}
            >
              Pin
            </button>
            <button
              type="button"
              className="rounded border border-[var(--color-border-strong)] px-2 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)]"
              onClick={onBulkUnpin}
            >
              Unpin
            </button>
            <button
              ref={bulkFolderRef}
              type="button"
              aria-haspopup="menu"
              aria-expanded={bulkPanel === "folder"}
              className="rounded border border-[var(--color-border-strong)] px-2 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)]"
              onClick={() => setBulkPanel((p) => (p === "folder" ? "none" : "folder"))}
            >
              Move to folder
            </button>
            <button
              ref={bulkLabelsRef}
              type="button"
              aria-haspopup="menu"
              aria-expanded={bulkPanel === "labels"}
              className="rounded border border-[var(--color-border-strong)] px-2 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)]"
              onClick={() => setBulkPanel((p) => (p === "labels" ? "none" : "labels"))}
            >
              Add label
            </button>
          </div>
          {largeSelection ? (
            <p className="mt-1.5 text-[0.6875rem] text-[var(--color-danger)]">
              Selecting {selectedIds.size} conversations — large bulk deletes are permanent.
            </p>
          ) : null}
          <Menu
            open={bulkPanel === "folder"}
            onClose={() => setBulkPanel("none")}
            anchorRef={bulkFolderRef}
            placement="bottom-end"
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
            placement="bottom-end"
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
      ) : null}

      {/* Conversation list */}
      <div className="mt-2 flex-1 overflow-y-auto">
        {isLoadingHistory ? (
          <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : filtering ? (
          <>
            {filteredConversations.length === 0 ? (
              <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">
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
                <div className="flex items-center gap-1.5 px-2 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-text-secondary)]">
                  <Icon name="pin" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
                  Pinned
                </div>
                {pinned.map(renderRow)}
              </div>
            ) : null}
            {foldersSection}
            {labelsSection}
            <div className="mb-1">
              <div className="px-2 py-1.5 text-[0.8125rem] font-semibold text-[var(--color-text-secondary)]">Recent</div>
              {recent.length === 0 ? (
                <p className="px-2 py-1.5 text-[0.8125rem] text-[var(--color-text-muted)]">No saved chats yet.</p>
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
    </NavRail>
  );
}
