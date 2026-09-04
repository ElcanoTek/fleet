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
// What is NOT here is as deliberate as what is: attachments and generated
// files in the owner's workspace are not reachable through a team share (a
// shared conversation ABOUT a report must not hand out the report), and the
// transcript excludes tool calls, tool results and reasoning — the same filter
// the public snapshot applies, for the same reason.

import { lazy, Suspense, useEffect, useState } from "react";
import { Icon } from "./Icon";
import { TeamGlyph } from "./ShareGlyphs";
import { CopyButton } from "./ChatChips";
import {
  ReadOnlyTranscript,
  toBubbles,
  type RawEntry,
} from "./ReadOnlyTranscript";

// The assistant markdown pipeline is lazy-loaded here for the same reason the
// live transcript lazy-loads it (react-markdown + micromark, ~43 KiB): this
// viewer is part of the chat bundle, and importing it eagerly would put that
// cost back on every first paint. Raw text with preserved whitespace stands in
// while the chunk loads.
const AssistantMarkdown = lazy(() => import("./AssistantContent"));

// One transcript renderer, shared with the public share view — see
// ReadOnlyTranscript. The markdown pipeline is the parameter, because this
// door lazy-loads it and that one does not.


// Mirrors store.TeamSharedConversation's serialized shape exactly. The owner's
// persona, model, project and lockdown are deliberately NOT sent — the fork's
// settings are decided server-side from the parent row, so the viewer has no
// use for them.
type TeamChatSnapshot = {
  id: string;
  owner_email: string;
  title: string;
  team_id: string;
  updated_at: number;
  messages: RawEntry[];
};

function shortName(email: string): string {
  const at = email.indexOf("@");
  return at > 0 ? email.slice(0, at) : email;
}

export function TeamChatViewer({
  conversationId,
  onBack,
  onBranched,
}: {
  conversationId: string;
  onBack: () => void;
  // Called with the NEW conversation id once a branch is created — the parent
  // opens it, so the reader lands in their own chat rather than back here.
  onBranched: (newConversationId: string) => void;
}) {
  const [snapshot, setSnapshot] = useState<TeamChatSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [branching, setBranching] = useState(false);

  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      void (async () => {
        try {
          const res = await fetch(
            `/api/conversations/${encodeURIComponent(conversationId)}/team-view`,
            { cache: "no-store" },
          );
          if (!res.ok) {
            if (!cancelled)
              setError(
                res.status === 404
                  ? "This chat isn’t shared with your team any more."
                  : `Couldn’t load this chat (HTTP ${res.status}).`,
              );
            return;
          }
          const data = (await res.json()) as TeamChatSnapshot;
          if (!cancelled) setSnapshot(data);
        } catch {
          if (!cancelled) setError("Couldn’t reach the server.");
        }
      })();
    });
    return () => {
      cancelled = true;
    };
  }, [conversationId]);

  const bubbles = snapshot ? toBubbles(snapshot.messages) : [];
  // Branch from the LAST message: "continue from here" is what the CTA
  // promises, and the whole transcript is what the reader just read.
  const branchPoint = [...bubbles].reverse().find((b) => b.lastId)?.lastId ?? 0;

  const branch = async () => {
    if (!snapshot || branching || !branchPoint) return;
    setBranching(true);
    setError(null);
    try {
      const res = await fetch(
        `/api/conversations/${encodeURIComponent(conversationId)}/branch`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            branch_point_message_id: branchPoint,
            title: `${snapshot.title || "Shared chat"} (from ${shortName(snapshot.owner_email)})`,
          }),
        },
      );
      if (!res.ok) {
        setError(`Couldn’t branch this chat (HTTP ${res.status}).`);
        return;
      }
      const created = (await res.json()) as { id?: string };
      if (created.id) onBranched(created.id);
    } catch {
      setError("Couldn’t branch this chat — network error.");
    } finally {
      setBranching(false);
    }
  };

  return (
    <div
      className="min-h-0 flex-1 overflow-y-auto px-4 pb-8 pt-4 sm:px-8"
      data-testid="team-chat-viewer"
    >
      <div className="mx-auto max-w-3xl">
        <div className="mb-4 flex items-center gap-3">
          <button
            type="button"
            aria-label="Back"
            className="inline-flex size-8 shrink-0 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            onClick={onBack}
          >
            <Icon name="arrow-right" className="size-4 rotate-180" />
          </button>
          <h1 className="min-w-0 flex-1 truncate text-[1.2rem] font-semibold text-[var(--color-text-primary)]">
            {snapshot?.title || "Shared chat"}
          </h1>
        </div>

        {snapshot ? (
          <p className="mb-5 flex flex-wrap items-center gap-1.5 rounded-[0.75rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.78rem] text-[var(--color-text-secondary)]">
            <TeamGlyph className="size-3.5 shrink-0 text-[var(--color-accent)]" />
            <span>
              <strong className="font-medium text-[var(--color-text-primary)]">
                {shortName(snapshot.owner_email)}
              </strong>
              ’s conversation · shared with{" "}
              {snapshot.team_id || "your team"} · read-only
            </span>
          </p>
        ) : null}

        {/* Only the LOAD failure belongs up here — a branch failure renders
            beside the button that caused it (see the sticky bar below), which
            on any transcript longer than a viewport is the only place the
            reader is looking. */}
        {error && !snapshot ? (
          <p className="mb-4 rounded-[0.75rem] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.8rem] text-[var(--color-danger)]">
            {error}
          </p>
        ) : null}

        {!snapshot && !error ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : null}

        <ReadOnlyTranscript
          bubbles={bubbles}
          audience="team"
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
            {/* Legibility over the scrolling transcript comes from the SAME
                treatment the live composer uses (--sticky-fade, see the
                composer section in chat-experience.tsx): a soft gradient that
                starts fully transparent 4rem above the CTA and reaches the
                page background behind it, bleeding past the reading column's
                padding so there is no edge anywhere. The flat panel this
                replaces drew a hard border and an opaque plate across the
                transcript. The token is theme-swapped in globals.css, so each
                theme fades to its own --color-bg.

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
            {/* Opaque control (--color-surface-1 is a solid colour in both
                themes) with a soft shadow, so the button reads as a control
                sitting above the page rather than a panel cut into it.
                Size, placement and label are unchanged. */}
            <button
              type="button"
              disabled={branching}
              className="relative flex w-full items-center justify-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-4 py-3 text-[0.9rem] font-medium text-[var(--color-text-primary)] shadow-[var(--shadow-md)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60"
              onClick={() => void branch()}
            >
              <Icon name="plus" className="size-4 shrink-0" />
              {branching ? "Branching…" : "Branch to continue in your own chat"}
            </button>
            <p className="relative mt-1.5 text-center text-[0.7rem] text-[var(--color-text-muted)]">
              You get your own copy in this project — private until you share
              it. {shortName(snapshot.owner_email)}’s chat is unchanged.
            </p>
          </div>
        ) : null}
      </div>
    </div>
  );
}
