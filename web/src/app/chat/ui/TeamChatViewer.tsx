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

// The assistant markdown pipeline is lazy-loaded here for the same reason the
// live transcript lazy-loads it (react-markdown + micromark, ~43 KiB): this
// viewer is part of the chat bundle, and importing it eagerly would put that
// cost back on every first paint. Raw text with preserved whitespace stands in
// while the chunk loads.
const AssistantMarkdown = lazy(() => import("./AssistantContent"));

type RawEntry = { id?: number; role: string; type: string; content: unknown };

export type TeamChatSnapshot = {
  id: string;
  owner_email: string;
  title: string;
  persona: string;
  model: string;
  team_id: string;
  project_id?: string;
  created_at: number;
  updated_at: number;
  messages: RawEntry[];
};

type Bubble = { role: "user" | "assistant"; text: string; lastId?: number };

// toBubbles flattens the stored history entries into a clean user/assistant
// text thread, merging consecutive same-role text (an assistant reply can land
// as several text entries within one turn). The last merged entry's persisted
// id is kept: that is the branch point a reader forks from.
function toBubbles(entries: RawEntry[]): Bubble[] {
  const out: Bubble[] = [];
  for (const e of entries ?? []) {
    if (e.type !== "text" || (e.role !== "user" && e.role !== "assistant")) continue;
    const text = String((e.content as { text?: string } | null)?.text ?? "");
    if (!text) continue;
    const last = out[out.length - 1];
    if (last && last.role === e.role) {
      last.text += text;
      if (e.id) last.lastId = e.id;
    } else {
      out.push({ role: e.role, text, lastId: e.id });
    }
  }
  return out;
}

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

        {error ? (
          <p className="mb-4 rounded-[0.75rem] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-[0.8rem] text-[var(--color-danger)]">
            {error}
          </p>
        ) : null}

        {!snapshot && !error ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : null}

        <div className="flex flex-col gap-5">
          {bubbles.map((b, i) =>
            b.role === "user" ? (
              <div key={i} className="flex justify-end">
                <div className="max-w-[85%] whitespace-pre-wrap rounded-[1rem] bg-[var(--color-overlay-strong)] px-4 py-2.5 text-[0.9375rem] leading-[1.55]">
                  {b.text}
                </div>
              </div>
            ) : (
              <div key={i} className="assistant-markdown max-w-full text-[0.9375rem] leading-[1.6]">
                <Suspense fallback={<div className="whitespace-pre-wrap">{b.text}</div>}>
                  <AssistantMarkdown content={b.text} />
                </Suspense>
                <div className="mt-2 flex items-center gap-3 text-[0.7rem]">
                  <CopyButton text={b.text} />
                </div>
              </div>
            ),
          )}
        </div>

        {snapshot && bubbles.length === 0 ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">
            This conversation has no messages to show.
          </p>
        ) : null}

        {/* Where the composer would be. A read-only view must not read as a
            dead end: this is the one thing a reader can do with someone
            else's chat, and it needs no permission from them. */}
        {snapshot && branchPoint ? (
          <div className="sticky bottom-0 mt-8 border-t border-[var(--color-border)] bg-[var(--color-surface-1)] pb-2 pt-4">
            <button
              type="button"
              disabled={branching}
              className="flex w-full items-center justify-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-4 py-3 text-[0.9rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-60"
              onClick={() => void branch()}
            >
              <Icon name="plus" className="size-4 shrink-0" />
              {branching ? "Branching…" : "Branch to continue in your own chat"}
            </button>
            <p className="mt-1.5 text-center text-[0.7rem] text-[var(--color-text-muted)]">
              You get your own copy in this project — private until you share
              it. {shortName(snapshot.owner_email)}’s chat is unchanged.
            </p>
          </div>
        ) : null}
      </div>
    </div>
  );
}
