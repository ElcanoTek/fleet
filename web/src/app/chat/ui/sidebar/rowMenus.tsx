import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef, useState } from "react";
import type { RefObject } from "react";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { Icon } from "../Icon";
import { addLabel as addLabelTo } from "../conversationOrganization";
import type { ConversationSummary } from "../chat-experience";
import type { Project } from "../ProjectsModal";
import { ShareGlyph, TeamGlyph } from "../ShareGlyphs";
import { teamLinkUrl } from "../teamSharing";
import { copyText } from "@/app/shared/lib/clipboard";
import { KeepAtTopItem, StartPageItem } from "@/app/shared/sidebar/SidebarMenuItems";
import { usePortalTip } from "./usePortalTip";
import { KEBAB_CLASS, ROW_ACTIONS_REVEAL } from "./classes";
import { LabelsPanel, ProjectPanel } from "./panels";

// The sidebar's ⋯ menus: a project's, the person's own chat's and a
// teammate's chat's, and the button that opens them.

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

export const caret = (
  <Icon name="chevron-right" className="size-3.5 text-[var(--color-text-muted)]" />
);

export function KebabButton({
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
export function ProjectKebab({
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
export function ConversationKebab({
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
export function teamChatLink(c: ConversationSummary): string | null {
  if (c.team_visible) return teamLinkUrl(c.id);
  if (c.multiplayer && typeof window !== "undefined") {
    return `${window.location.origin}/chat?c=${encodeURIComponent(c.id)}`;
  }
  return null;
}


export function TeamChatKebab({
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
