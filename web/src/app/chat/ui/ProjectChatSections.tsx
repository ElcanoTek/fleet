"use client";

import { useRef, useState, type RefObject } from "react";
import { Menu, MenuItem, MenuSeparator } from "@/app/shared/ui/Menu";
import { ConfirmDialog, NameChip } from "./ConfirmDialog";
import { Icon } from "./Icon";
import { ShareGlyph, TeamGlyph } from "./ShareGlyphs";
import { stripMarkdown } from "./formatters";
import { pillBase } from "./ProjectVisibilityPill";
import { primaryButtonClass } from "./ProjectGettingStarted";
import {
  fetchConversationOutputs,
  formatDay,
  plural,
  type ViewerBranch,
} from "./teamSharing";

// The project home's two chat lists (Fleet Projects sharing, decisions #18–
// #21):
//
//   Your chats — the caller's own chats in the project. Every row carries a
//   visibility pill ("Only you" / the team's name) that is ALSO the quick
//   switch between the two, with "More sharing options…" for the full share
//   dialog. Switching to the team shares all outputs (the fast path — the
//   dialog's checklist and Sources are where files are adjusted). Switching
//   back to Only you asks first, with the shared-file count, like Stop
//   sharing does everywhere else: it ends teammates' access.
//
//   Shared by your team — teammates' chats shared with the team. A row ALWAYS
//   opens the owner's live chat read-only, never the viewer's branch; a viewer
//   who has branched it sees "You branched this" instead of "Read, branch".

export type ProjectChatEntry = {
  id: string;
  title: string;
  updated_at: number;
  preview?: string;
  share_token?: string;
  team_visible?: boolean;
};

export type TeamChatEntry = {
  id: string;
  title: string;
  user_email: string;
  updated_at: number;
  viewer_branch?: ViewerBranch | null;
};

const sectionClass =
  "rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-2";

const countClass =
  "rounded-full bg-[var(--color-overlay-soft)] px-1.5 font-[family-name:var(--font-code)] text-[0.65rem] text-[var(--color-text-muted)]";

// ── New chat (button + menu) ────────────────────────────────────────────────

function NewChatButton({
  teamName,
  menuOpen,
  onMenuOpenChange,
  onNewChat,
  anchorRef,
}: {
  // The project's team; "" = personal project → no menu, one click.
  teamName: string;
  menuOpen: boolean;
  onMenuOpenChange: (open: boolean) => void;
  onNewChat: (shared: boolean) => void;
  anchorRef: RefObject<HTMLButtonElement | null>;
}) {
  return (
    <>
      <button
        ref={anchorRef}
        type="button"
        aria-haspopup={teamName ? "menu" : undefined}
        aria-expanded={teamName ? menuOpen : undefined}
        className={primaryButtonClass}
        onClick={() => {
          if (teamName) onMenuOpenChange(!menuOpen);
          else onNewChat(false);
        }}
      >
        <Icon name="plus" className="size-3.5" />
        New chat
        {teamName ? <Icon name="chevron-down" className="size-3" /> : null}
      </button>
      {teamName ? (
        <Menu
          open={menuOpen}
          onClose={() => onMenuOpenChange(false)}
          anchorRef={anchorRef}
          placement="bottom-end"
          label="New chat"
          className="min-w-[16rem]"
        >
          <MenuItem
            icon={<Icon name="lock" className="size-4" />}
            description="Private until you share it"
            onClick={() => {
              onMenuOpenChange(false);
              onNewChat(false);
            }}
          >
            Only you
          </MenuItem>
          <MenuItem
            icon={<TeamGlyph className="size-4" />}
            description={`${teamName} can read it and branch it`}
            onClick={() => {
              onMenuOpenChange(false);
              onNewChat(true);
            }}
          >
            Shared with {teamName}
          </MenuItem>
        </Menu>
      ) : null}
    </>
  );
}

// ── Row visibility pill ─────────────────────────────────────────────────────

function VisibilityPill({
  chat,
  teamName,
  onShare,
  onRequestUnshare,
  onMoreOptions,
}: {
  chat: ProjectChatEntry;
  teamName: string;
  onShare: () => void;
  onRequestUnshare: () => void;
  onMoreOptions: () => void;
}) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLButtonElement | null>(null);
  const shared = Boolean(chat.team_visible);
  const label = shared ? teamName : "Only you";
  if (!teamName) {
    // A personal project's chats cannot be team-shared; the pill states the
    // fact without offering a switch that cannot work.
    return (
      <span
        className={`${pillBase} border-[var(--color-border)] text-[var(--color-text-secondary)]`}
      >
        <Icon name="lock" className="size-2.5" />
        Only you
      </span>
    );
  }
  const close = () => setOpen(false);
  return (
    <>
      <button
        ref={ref}
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Who can see this chat: ${label}`}
        className={[
          pillBase,
          "transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
          shared
            ? "border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-text-primary)]"
            : "border-[var(--color-border)] text-[var(--color-text-secondary)]",
        ].join(" ")}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((o) => !o);
        }}
      >
        {shared ? (
          <TeamGlyph className="size-3" />
        ) : (
          <Icon name="lock" className="size-2.5" />
        )}
        {label}
        <Icon name="chevron-down" className="size-2.5" />
      </button>
      <Menu
        open={open}
        onClose={close}
        anchorRef={ref}
        placement="bottom-end"
        label="Who can see this chat"
        className="min-w-[16rem]"
      >
        <MenuItem
          icon={<Icon name="lock" className="size-4" />}
          description="Private until you share it"
          trailing={!shared ? <Icon name="check" className="size-3.5" /> : undefined}
          onClick={() => {
            close();
            if (shared) onRequestUnshare();
          }}
        >
          Only you
        </MenuItem>
        <MenuItem
          icon={<TeamGlyph className="size-4" />}
          description="Teammates can read and branch it"
          trailing={shared ? <Icon name="check" className="size-3.5" /> : undefined}
          onClick={() => {
            close();
            if (!shared) onShare();
          }}
        >
          {teamName}
        </MenuItem>
        <MenuSeparator />
        <MenuItem
          icon={<ShareGlyph className="size-4" />}
          description="Public link for people outside your team"
          onClick={() => {
            close();
            onMoreOptions();
          }}
        >
          More sharing options…
        </MenuItem>
      </Menu>
    </>
  );
}

// ── Your chats ──────────────────────────────────────────────────────────────

export function YourChatsSection({
  chats,
  visibleChats,
  query,
  teamName,
  showEmptyText,
  newChatMenuOpen,
  onNewChatMenuOpenChange,
  newChatAnchorRef,
  onNewChat,
  onOpenChat,
  onShare,
  onUnshare,
  onMoreOptions,
}: {
  chats: ProjectChatEntry[];
  visibleChats: ProjectChatEntry[];
  query: string;
  // The project's team; "" = personal project.
  teamName: string;
  // False while the getting-started card is up — the card replaces the
  // empty-state paragraph.
  showEmptyText: boolean;
  newChatMenuOpen: boolean;
  onNewChatMenuOpenChange: (open: boolean) => void;
  newChatAnchorRef: RefObject<HTMLButtonElement | null>;
  onNewChat: (shared: boolean) => void;
  onOpenChat: (id: string) => void;
  onShare: (chat: ProjectChatEntry) => void;
  // Called after the owner confirmed; `sharedFiles` is the count quoted.
  onUnshare: (chat: ProjectChatEntry) => void;
  onMoreOptions: (id: string) => void;
}) {
  const sharedCount = chats.filter((c) => c.team_visible).length;
  // The confirm before a chat goes back to Only you: the chat, and how many
  // shared files stop with it (null = still counting; undefined = unknown).
  // `req` identifies the opening: a count only settles the confirm it was
  // read for, so a slow read from an earlier opening (Keep sharing, then the
  // same pill again) can neither settle a fresh confirm early nor overwrite
  // the newer count.
  const [unshare, setUnshare] = useState<{
    chat: ProjectChatEntry;
    files: number | null | undefined;
    req: object;
  } | null>(null);

  const requestUnshare = (chat: ProjectChatEntry) => {
    const req = {};
    setUnshare({ chat, files: null, req });
    void fetchConversationOutputs(chat.id).then(
      (o) =>
        setUnshare((cur) =>
          cur && cur.req === req ? { ...cur, files: o.shared_count } : cur,
        ),
      () =>
        setUnshare((cur) =>
          cur && cur.req === req ? { ...cur, files: undefined } : cur,
        ),
    );
  };

  return (
    <section aria-label="Your chats" className={sectionClass}>
      <div className="flex flex-wrap items-center gap-2 px-2 pb-1.5 pt-1">
        <Icon name="message" className="size-4 shrink-0 text-[var(--color-accent)]" />
        <h2 className="m-0 text-[0.88rem] font-semibold text-[var(--color-text-primary)]">
          Your chats
        </h2>
        {chats.length > 0 ? <span className={countClass}>{chats.length}</span> : null}
        {teamName && chats.length > 0 ? (
          <span className="text-[0.74rem] text-[var(--color-text-muted)]">
            {sharedCount} of {chats.length} shared with {teamName}
          </span>
        ) : null}
        <span className="flex-1" />
        <NewChatButton
          teamName={teamName}
          menuOpen={newChatMenuOpen}
          onMenuOpenChange={onNewChatMenuOpenChange}
          onNewChat={onNewChat}
          anchorRef={newChatAnchorRef}
        />
      </div>
      {chats.length === 0 ? (
        showEmptyText ? (
          <p className="m-0 px-2 py-2 text-[0.85rem] leading-[1.6] text-[var(--color-text-muted)]">
            {teamName
              ? "No chats yet. Start one with New chat, or move one here from the sidebar. New chats can only be seen by you until you share them."
              : "No chats yet. Start one with New chat, or move one here from the sidebar."}
          </p>
        ) : null
      ) : visibleChats.length === 0 ? (
        <p className="m-0 px-2 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
          No chats of yours match “{query}”.
        </p>
      ) : (
        <div className="flex flex-col gap-0.5">
          {visibleChats.map((c) => (
            <div
              key={c.id}
              data-testid="project-chat-row"
              className="flex w-full items-start gap-2 rounded-[var(--radius-md)] pr-2 transition hover:bg-[var(--color-overlay-soft)]"
            >
              <button
                type="button"
                className="flex min-w-0 flex-1 items-start gap-3 rounded-[var(--radius-md)] px-2 py-2.5 text-left focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                onClick={() => onOpenChat(c.id)}
              >
                <Icon
                  name="message"
                  className="mt-0.5 size-4 shrink-0 text-[var(--color-text-muted)]"
                />
                <span className="min-w-0 flex-1">
                  <span className="flex min-w-0 items-center gap-1.5">
                    <span className="truncate text-[0.9rem] text-[var(--color-text-primary)]">
                      {c.title || "Untitled"}
                    </span>
                    {c.share_token ? (
                      <span
                        title="Shared by link — anyone with the link"
                        aria-label="Shared by link — anyone with the link"
                        className="shrink-0 text-[var(--color-accent)]"
                      >
                        <ShareGlyph className="size-3" />
                      </span>
                    ) : null}
                  </span>
                  {c.preview ? (
                    <span className="mt-0.5 line-clamp-2 block text-[0.78rem] leading-snug text-[var(--color-text-muted)]">
                      {stripMarkdown(c.preview)}
                    </span>
                  ) : null}
                </span>
              </button>
              <span className="flex shrink-0 items-center gap-2 pt-2.5">
                <VisibilityPill
                  chat={c}
                  teamName={teamName}
                  onShare={() => onShare(c)}
                  onRequestUnshare={() => requestUnshare(c)}
                  onMoreOptions={() => onMoreOptions(c.id)}
                />
                <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                  {formatDay(c.updated_at)}
                </span>
              </span>
            </div>
          ))}
        </div>
      )}
      {unshare ? (
        <ConfirmDialog
          title={`Stop sharing “${unshare.chat.title || "Untitled"}” with ${teamName}?`}
          titleContent={
            <>
              Stop sharing with{" "}
              <NameChip icon={<TeamGlyph className="size-3 shrink-0" />} suffix="?">
                {teamName}
              </NameChip>
            </>
          }
          confirmLabel="Stop sharing"
          cancelLabel="Keep sharing"
          confirmTone="danger"
          busy={unshare.files === null}
          testId="row-unshare-confirm"
          onCancel={() => setUnshare(null)}
          onConfirm={() => {
            const chat = unshare.chat;
            setUnshare(null);
            onUnshare(chat);
          }}
        >
          <p className="m-0">
            “{unshare.chat.title || "Untitled"}” goes back to Only you.{" "}
            {unshare.files === null
              ? "Counting its shared files…"
              : unshare.files === undefined
                ? "Its shared files will stop being shared too."
                : unshare.files > 0
                  ? `${plural(unshare.files, "shared file", "shared files")} will stop being shared too.`
                  : "It has no shared files."}
          </p>
          <p className="m-0">Teammates who branched it keep their copies.</p>
        </ConfirmDialog>
      ) : null}
    </section>
  );
}

// ── Shared by your team ─────────────────────────────────────────────────────

export function TeamChatsSection({
  teamChats,
  visibleTeamChats,
  error,
  query,
  onOpenTeamChat,
}: {
  teamChats: TeamChatEntry[] | null;
  visibleTeamChats: TeamChatEntry[];
  error: string | null;
  query: string;
  onOpenTeamChat: (id: string) => void;
}) {
  return (
    <section aria-label="Shared by your team" className={`${sectionClass} mt-4`}>
      <div className="flex items-center gap-2 px-2 pb-1.5 pt-1">
        <TeamGlyph className="size-4 shrink-0 text-[var(--color-accent)]" />
        <h2 className="m-0 text-[0.88rem] font-semibold text-[var(--color-text-primary)]">
          Shared by your team
        </h2>
        {teamChats && teamChats.length > 0 ? (
          <span className={countClass}>{teamChats.length}</span>
        ) : null}
      </div>
      {error ? (
        <p
          role="alert"
          className="mx-2 mb-2 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-2 py-1.5 text-[0.72rem] leading-[1.5] text-[var(--color-danger)]"
        >
          {error}
        </p>
      ) : null}
      {teamChats === null ? (
        <p className="m-0 px-2 py-2 text-[0.85rem] text-[var(--color-text-muted)]">Loading…</p>
      ) : error ? null : teamChats.length === 0 ? (
        <p className="m-0 px-2 py-2 text-[0.85rem] leading-[1.6] text-[var(--color-text-muted)]">
          Nothing from teammates yet. Chats they share show up here.
        </p>
      ) : visibleTeamChats.length === 0 ? (
        <p className="m-0 px-2 py-2 text-[0.85rem] text-[var(--color-text-muted)]">
          No shared chats match “{query}”.
        </p>
      ) : (
        <div className="flex flex-col gap-0.5">
          {visibleTeamChats.map((c) => (
            <button
              key={c.id}
              type="button"
              data-testid="team-chat-row"
              className="flex w-full items-center gap-3 rounded-[var(--radius-md)] px-2 py-2 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={() => onOpenTeamChat(c.id)}
            >
              <span
                aria-hidden="true"
                className="grid size-7 shrink-0 place-items-center rounded-full bg-[var(--color-overlay-soft)] text-[0.72rem] font-semibold text-[var(--color-text-secondary)]"
              >
                {(c.user_email[0] ?? "?").toUpperCase()}
              </span>
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[0.9rem] text-[var(--color-text-primary)]">
                  {c.title || "Untitled"}
                </span>
                <span className="block truncate text-[0.75rem] text-[var(--color-text-muted)]">
                  {c.user_email}
                </span>
              </span>
              <span
                className={`${pillBase} border-[var(--color-border)] text-[var(--color-text-secondary)]`}
              >
                {c.viewer_branch ? "You branched this" : "Read, branch"}
              </span>
              <span className="shrink-0 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                {formatDay(c.updated_at)}
              </span>
            </button>
          ))}
        </div>
      )}
    </section>
  );
}
