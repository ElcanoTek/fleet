"use client";

// What a teammate sees when they open a chat someone shared with the team
// (Item C4, ADR-0057).
//
// Same rendering as a public share link, different door: membership of the
// team plus the owner's per-chat opt-in, instead of a capability URL. It is
// read-only by construction — there is no composer, and no write path exists
// for a non-owner — but a read-only view that ends in nothing is a dead end,
// so where the composer would be there is exactly one forward action:
// **Branch to continue in your own chat**. The branch is a copy the reader
// owns from the first byte, filed back into the same project, private until
// they share it, and unaffected if the original is later unshared or deleted.
//
// Files (B19): sharing a chat shares its OUTPUTS — the files the agent
// presented in its replies — minus any the owner unchecked. The snapshot's
// `files` lists every output with its shared flag; shared ones render as live
// downloads through the team-files route (images inline from it), the rest as
// locked names, and uploads as plain names. Nothing is fetched from the
// owner's workspace route. The transcript still excludes tool calls, tool
// results and reasoning — the same filter the public snapshot applies.
//
// The view is LIVE: the owner may keep working, so it re-reads the snapshot
// on a short interval while the tab is visible (and on returning to it).

import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { conversationApiUrl } from "@/app/lib/conversationApiUrl";
import { Icon } from "./Icon";
import { LockGlyph, TeamGlyph } from "./ShareGlyphs";
import { CopyButton } from "./ChatChips";
import { useChatToast } from "./ChatToasts";
import {
  ReadOnlyTranscript,
  toBubbles,
  type RawEntry,
} from "./ReadOnlyTranscript";
import { ViewerBranchBanner } from "./TeamBranchBanners";
import {
  ownerFirstName,
  teamFileUrl,
  type OutputFile,
  type ViewerBranch,
} from "./teamSharing";

// The assistant markdown pipeline is lazy-loaded here for the same reason the
// live transcript lazy-loads it (react-markdown + micromark, ~43 KiB): this
// viewer is part of the chat bundle, and importing it eagerly would put that
// cost back on every first paint. Raw text with preserved whitespace stands in
// while the chunk loads.
const AssistantMarkdown = lazy(() => import("./AssistantContent"));

// How often the open view re-reads the owner's chat. Short enough that a
// teammate watching sees the owner's next reply land without reloading; the
// read is one small JSON document, and it stops while the tab is hidden.
export const TEAM_VIEW_POLL_MS = 12_000;

// Mirrors store.TeamSharedConversation's serialized shape. The owner's
// persona, model and lockdown are deliberately NOT sent — the fork's settings
// are decided server-side from the parent row, so the viewer has no use for
// them. `files`, project and viewer_branch are optional so an older server
// (transcript-only team views) still renders: without `files` every workspace
// reference is withheld exactly as before.
type TeamChatSnapshot = {
  id: string;
  owner_email: string;
  title: string;
  team_id: string;
  updated_at: number;
  messages: RawEntry[];
  project_id?: string;
  project_name?: string;
  files?: OutputFile[];
  /** `files` holds only the most recent referenced outputs; older references render locked. */
  files_truncated?: boolean;
  viewer_branch?: ViewerBranch | null;
};

// The toast names what actually came along: a text-only chat, one whose
// outputs the owner all unticked, or a copy that failed brings no files, and
// saying "shared files came with it" then would send the reader looking for
// files that do not exist.
function branchedToast(copiedFiles: number): string {
  if (copiedFiles <= 0) return "Branched into your own chat. No files came with it.";
  if (copiedFiles === 1) return "Branched into your own chat. 1 shared file came with it.";
  return `Branched into your own chat. ${copiedFiles} shared files came with it.`;
}

export function TeamChatViewer({
  conversationId,
  onBack,
  onBranched,
  onOpenProject,
  onOpenBranch,
}: {
  conversationId: string;
  onBack: () => void;
  // Called with the NEW conversation id once a branch is created — the parent
  // opens it, so the reader lands in their own chat rather than back here.
  onBranched: (newConversationId: string) => void;
  // The breadcrumb: open the chat's project home.
  onOpenProject?: (projectId: string) => void;
  // B21's "Open your branch".
  onOpenBranch?: (conversationId: string) => void;
}) {
  const [snapshot, setSnapshot] = useState<TeamChatSnapshot | null>(null);
  // Two errors, two places: a LOAD failure replaces the transcript, a branch
  // failure sits beside the branch button. Kept apart so a successful poll
  // after a failed first load can clear the one without wiping the other.
  const [loadError, setLoadError] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [branching, setBranching] = useState(false);
  const { notify } = useChatToast();

  // One read of the snapshot. `quiet` is the live refresh: a transient
  // failure keeps what is on screen rather than replacing the transcript
  // with an error, but a 404 means the chat stopped being shared with this
  // reader, and showing the old transcript after that would be a lie.
  //
  // Reads can overlap (the poll, a visibility refresh, the first load), and
  // responses can land out of order. Each read takes a generation number and
  // only the LATEST read's outcome — snapshot, 404 or error — is applied, so
  // an older response can never repaint a transcript (or resurrect one after
  // a newer 404) over a newer one.
  const loadGen = useRef(0);
  // The ETag of the snapshot on screen. Polls send it as If-None-Match and a
  // 304 keeps the snapshot as is — an unchanged chat costs the server a light
  // version read instead of a full transcript load. Cleared with the
  // snapshot (a 404), so the next read after that is unconditional.
  const etagRef = useRef<string | null>(null);
  // Whether a snapshot is on screen. A quiet read only hides its failure
  // when there is something to keep showing: a poll that supersedes the
  // still-pending first load (and so makes that load's outcome stale) must
  // report its own failure, or the reader is left on "Loading…" for good.
  const hasSnapshot = useRef(false);
  const load = useCallback(
    async (quiet: boolean, isCancelled: () => boolean) => {
      const gen = ++loadGen.current;
      const stale = () => isCancelled() || gen !== loadGen.current;
      try {
        const url = conversationApiUrl(conversationId, "/team-view");
        if (!url) {
          if (!stale()) setLoadError("This chat link has an invalid id.");
          return;
        }
        const etag = etagRef.current;
        const res = await fetch(url, {
          cache: "no-store",
          headers: etag ? { "If-None-Match": etag } : undefined,
        });
        if (stale()) return;
        if (res.status === 304) {
          // Unchanged since the snapshot on screen; a 304 also means the
          // read succeeded, so an earlier transient failure is retired.
          setLoadError(null);
          return;
        }
        if (!res.ok) {
          if (res.status === 404) {
            etagRef.current = null;
            hasSnapshot.current = false;
            setSnapshot(null);
            setLoadError("This chat isn’t shared with your team anymore.");
          } else if (!quiet || !hasSnapshot.current) {
            setLoadError(`Couldn’t load this chat (HTTP ${res.status}).`);
          }
          return;
        }
        const data = (await res.json()) as TeamChatSnapshot;
        if (!stale()) {
          // Accepting a snapshot retires any earlier load failure: a first
          // load that failed and a poll that then succeeded must not leave
          // the stale "Couldn’t load" message on screen.
          setLoadError(null);
          setSnapshot(data);
          hasSnapshot.current = true;
          etagRef.current = res.headers.get("ETag");
        }
      } catch {
        if ((!quiet || !hasSnapshot.current) && !stale()) setLoadError("Couldn’t reach the server.");
      }
    },
    [conversationId],
  );

  useEffect(() => {
    let cancelled = false;
    const isCancelled = () => cancelled;
    // A different chat (or a remount) starts unconditional.
    etagRef.current = null;
    hasSnapshot.current = false;
    queueMicrotask(() => void load(false, isCancelled));
    const visible = () =>
      typeof document === "undefined" || document.visibilityState === "visible";
    const timer = window.setInterval(() => {
      if (visible()) void load(true, isCancelled);
    }, TEAM_VIEW_POLL_MS);
    const onVisibility = () => {
      if (visible()) void load(true, isCancelled);
    };
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [load]);

  const bubbles = snapshot ? toBubbles(snapshot.messages) : [];
  // Branch from the LAST message: "continue from here" is what the CTA
  // promises, and the whole transcript is what the reader just read.
  const branchPoint = [...bubbles].reverse().find((b) => b.lastId)?.lastId ?? 0;

  const files = snapshot?.files;
  const sharedFiles = useMemo(() => {
    if (!files) return undefined;
    // Each URL carries the file's revision (modified time and size), so a
    // file the owner overwrites at the same path gets a NEW url on the next
    // poll: an inline image re-requests its bytes instead of keeping the old
    // ones under an unchanged src. The route ignores the query.
    const rev = new Map(files.map((f) => [f.path, `${f.modified_at}-${f.size}`]));
    return {
      shared: new Set(files.filter((f) => f.shared).map((f) => f.path)),
      fileUrl: (path: string) => {
        const v = rev.get(path);
        const url = teamFileUrl(conversationId, path);
        return v ? `${url}?v=${encodeURIComponent(v)}` : url;
      },
    };
  }, [files, conversationId]);

  const branch = async () => {
    if (!snapshot || branching || !branchPoint) return;
    const branchUrl = conversationApiUrl(conversationId, "/branch");
    if (!branchUrl) return;
    setBranching(true);
    setError(null);
    try {
      const res = await fetch(branchUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          branch_point_message_id: branchPoint,
          title: `${snapshot.title || "Shared chat"} (branch)`,
        }),
      });
      if (!res.ok) {
        setError(`Couldn’t branch this chat (HTTP ${res.status}).`);
        return;
      }
      const created = (await res.json()) as {
        id?: string;
        branch_origin?: { copied_files?: unknown[] } | null;
      };
      if (created.id) {
        notify({
          message: branchedToast(
            created.branch_origin?.copied_files?.length ?? 0,
          ),
        });
        onBranched(created.id);
      }
    } catch {
      setError("Couldn’t branch this chat — network error.");
    } finally {
      setBranching(false);
    }
  };

  const owner = snapshot?.owner_email ?? "";
  const projectId = snapshot?.project_id;
  const projectName = snapshot?.project_name;

  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4 sm:px-8"
      data-testid="team-chat-viewer"
    >
      <div className="mx-auto max-w-3xl">
        <div className="mb-3 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
          <button
            type="button"
            aria-label="Back"
            className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={onBack}
          >
            <Icon name="arrow-right" className="size-4 rotate-180" />
          </button>
          {projectId && projectName ? (
            <>
              <button
                type="button"
                className="min-w-0 max-w-[40%] truncate rounded-md px-1.5 py-1 text-[0.8125rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                onClick={() => onOpenProject?.(projectId)}
              >
                {projectName}
              </button>
              <span aria-hidden="true" className="text-[var(--color-text-muted)]">
                ›
              </span>
            </>
          ) : null}
          <h1 className="min-w-0 flex-1 truncate text-[1.2rem] font-semibold text-[var(--color-text-primary)]">
            {snapshot?.title || "Shared chat"}
          </h1>
          {snapshot ? (
            <span
              data-testid="team-view-shared-by"
              className="inline-flex h-[1.625rem] max-w-full shrink-0 items-center gap-1.5 truncate rounded-full border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] px-2.5 text-[0.75rem] text-[var(--color-text-primary)]"
            >
              <TeamGlyph className="size-3 shrink-0" />
              <span className="truncate">Shared by {owner}</span>
            </span>
          ) : null}
        </div>

        {snapshot?.viewer_branch ? (
          <div className="mb-5">
            <ViewerBranchBanner
              branch={snapshot.viewer_branch}
              ownerEmail={owner}
              onOpenBranch={(id) => onOpenBranch?.(id)}
            />
          </div>
        ) : null}

        {/* Only the LOAD failure belongs up here — a branch failure renders
            beside the button that caused it (see the sticky bar below), which
            on any transcript longer than a viewport is the only place the
            reader is looking. */}
        {loadError && !snapshot ? (
          <p className="mb-4 rounded-[0.75rem] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.8rem] text-[var(--color-danger)]">
            {loadError}
          </p>
        ) : null}

        {!snapshot && !loadError ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : null}

        <ReadOnlyTranscript
          bubbles={bubbles}
          audience="team"
          sharedFiles={sharedFiles}
          renderAssistant={(text) => (
            <Suspense fallback={<div className="whitespace-pre-wrap">{text}</div>}>
              <AssistantMarkdown content={text} />
            </Suspense>
          )}
          actions={(b) => <CopyButton text={b.text} />}
        />

        {snapshot && bubbles.length === 0 ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">
            This conversation has no messages to show.
          </p>
        ) : null}

        {/* Where the composer would be. A read-only view must not read as a
            dead end: this is the one thing a reader can do with someone
            else's chat, and it needs no permission from them. */}
        {snapshot && branchPoint ? (
          <div className="sticky bottom-0 z-10 mt-8 pb-2 pt-4" data-testid="team-branch-cta">
            {/* This CTA owns its legibility treatment: a soft gradient that
                starts fully transparent 4rem above the CTA and reaches the
                page background behind it, bleeding past the reading column's
                padding so there is no edge anywhere. The token is
                theme-swapped in globals.css, so each theme fades to its own
                --color-bg. Keep it inside this CTA; the ordinary chat
                transcript/composer has no fade overlay.

                The `image:` hint is load-bearing: --sticky-fade is a gradient,
                and the un-hinted arbitrary-value form emits background-color,
                which drops gradient values (same note as Composer.tsx). */}
            <div
              aria-hidden="true"
              className="pointer-events-none absolute -left-4 -right-4 -top-16 bottom-0 bg-[image:var(--sticky-fade)] sm:-left-8 sm:-right-8"
            />
            {error ? (
              <p
                role="alert"
                className="relative mb-2 rounded-[0.75rem] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.8rem] text-[var(--color-danger)]"
              >
                {error}
              </p>
            ) : null}
            {/* B19: the read-only line where the composer would be, and the
                one forward action. An opaque bar (--color-surface-1 is solid
                in both themes) with a dashed edge, so it reads as "not a
                composer" while still sitting above the transcript. */}
            <div className="relative flex flex-wrap items-center gap-3 rounded-[var(--radius-xl)] border border-dashed border-[var(--color-border-strong)] bg-[var(--color-surface-1)] py-2.5 pl-4 pr-2.5 shadow-[var(--shadow-md)]">
              <span className="flex min-w-[13rem] flex-1 items-center gap-2 text-[0.8125rem] text-[var(--color-text-secondary)]">
                <LockGlyph className="size-3.5 shrink-0" />
                <span>Read-only. This is {ownerFirstName(owner)}’s chat.</span>
              </span>
              <button
                type="button"
                disabled={branching}
                className="inline-flex h-[2.375rem] items-center justify-center gap-2 rounded-full bg-[var(--color-primary)] px-[1.125rem] text-[0.8125rem] font-semibold text-[var(--color-on-primary)] transition hover:bg-[var(--color-primary-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60"
                onClick={() => void branch()}
              >
                {branching
                  ? "Branching…"
                  : snapshot.viewer_branch
                    ? "Branch again"
                    : "Branch to continue in your own chat"}
              </button>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}
