"use client";

// Small presentational chips and badges extracted from chat-experience.tsx
// (slice 4 of #169). These are the glanceable, self-contained bits of chrome
// the chat surface scatters around the transcript and composer — model-row
// badges, the copy-to-clipboard button, the context-usage ring, the per-turn
// and conversation-total cost chips, the compaction banner, and the pending-
// attachment chip. None of them touch the ChatExperience component's mutable
// closure state; each takes plain props (and a couple of callbacks) so moving
// them here is a pure relocation. Behavior, markup, and class names are
// byte-identical to the in-module originals.

import type { ReactNode } from "react";
import { useEffect, useMemo, useRef, useState } from "react";
import { type ModelTier } from "@/app/lib/modelAliases";
import {
  formatContextUsage,
  shouldShowDegradationCaption,
  type ContextUsage,
} from "@/app/lib/contextUsage";
import {
  cachedPercent,
  conversationTotals,
  shortModelName,
  type Message,
  type PythonStream,
  type ToolCall,
  type TurnSummary,
} from "./history";
import { Icon } from "./Icon";
import { formatBytes, formatDuration, formatTokens, formatUsd } from "./formatters";
import { TeamGlyph } from "./ShareGlyphs";

// PendingAttachment describes a file the user has picked to send with the
// next turn. We carry the live File object for display (chip name/size
// before upload) and, once the upload finishes, the server-returned path
// the /api/chat body needs to include so chat-server can hand it to the
// agent. "uploading" / "done" / "error" reflects the per-file state when
// multiple files are in flight.
export type PendingAttachment = {
  clientId: string;
  file: File;
  status: "pending" | "uploading" | "done" | "error";
  name: string;
  size: number;
  mime: string;
  // Populated once the server accepts the upload.
  serverPath?: string;
  errorMessage?: string;
};

// ModelValidationBadge renders a small pill next to non-tier rows in the
// model picker. "tested" means we've validated end-to-end with our tools
// + system prompt; "experimental" means it should still work but no
// guarantees. Three-tier visual hierarchy with the recommended pill on
// tier rows: solid accent fill > translucent accent (tested) > muted
// outline (experimental).
export function ModelValidationBadge({ tier }: { tier: ModelTier }) {
  const tone =
    tier === "tested"
      ? "border-[var(--color-accent)]/40 bg-[var(--color-accent)]/10 text-[var(--color-accent)]"
      : "border-[var(--color-border-strong)] text-[var(--color-text-muted)]";
  return (
    <span
      className={`shrink-0 rounded-full border px-1.5 py-0 text-[0.6rem] font-medium leading-4 tabular-nums ${tone}`}
    >
      {tier}
    </span>
  );
}

// "✨ new" pill for entries listed on OpenRouter within the last
// NEW_MODEL_WINDOW_DAYS. Uses --color-success (green) so it sits
// alongside the accent-fill `recommended` pill without competing for
// the same visual slot.
export function NewModelBadge() {
  return (
    <span
      className="shrink-0 rounded-full border border-[var(--color-success-border)] bg-[var(--color-success)]/15 px-1.5 py-0 text-[0.6rem] font-semibold leading-4 tabular-nums text-[var(--color-success)]"
      title="Listed on OpenRouter within the last two weeks"
    >
      ✨ new
    </span>
  );
}

export function CopyButton({
  text,
  title = "Copy reply to clipboard",
  variant = "default",
}: {
  text: string;
  title?: string;
  variant?: "default" | "compact";
}) {
  const [state, setState] = useState<"idle" | "ok" | "err">("idle");
  const resetRef = useRef<number | null>(null);

  useEffect(() => {
    return () => {
      if (resetRef.current) window.clearTimeout(resetRef.current);
    };
  }, []);

  const onClick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setState("ok");
    } catch {
      setState("err");
    }
    resetRef.current = window.setTimeout(() => setState("idle"), 1500);
  };

  return (
    <button
      type="button"
      onClick={() => void onClick()}
      className={
        variant === "compact"
          ? "assistant-markdown-pre-copy"
          : "touch-target text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
      }
      title={title}
    >
      {state === "idle" ? "Copy" : state === "ok" ? "Copied" : "Copy failed"}
    </button>
  );
}

// ── Context-usage ring ───────────────────────────────────────────────────
//
// Replaces the plain "compact this chat" icon button next to the
// composer. Reads as a tiny progress ring: empty when the context is
// cold, fills as the latest turn's prompt_tokens grows toward the
// model's context_length, and color-shifts through three severity
// bands (muted → accent → danger). The whole ring is the click
// target — it opens the same compact-confirm modal the old icon
// button did, so users get visual context AND a one-tap way to act on
// it from the same control.
//
// Pre-first-turn (no usage data) we fall back to the old summarize
// icon so the control still functions, just without the meter — the
// catalog might not have loaded yet, or the model's context_length
// is missing from the listing.
export function ContextRing({
  usage,
  isSummarizing,
  disabled,
  onClick,
}: {
  usage: ContextUsage | null;
  isSummarizing: boolean;
  disabled: boolean;
  onClick: () => void;
}) {
  // SVG ring math per the unified-shell design's .tool-btn.ring: the ring
  // hugs the button's edge (inset 1px, r=14 in a 32-unit viewBox) around a
  // centered compress icon. Two strokes on the same circle: the design's
  // .ring-track (accent at 26%) plus a colored progress arc whose length =
  // circumference * fraction. Stroke ends rounded so low fills don't read
  // as a sharp splinter.
  const r = 14;
  const circumference = 2 * Math.PI * r;
  const fraction = usage?.fraction ?? 0;
  const cappedFraction = Math.min(1, fraction);
  const dashFilled = circumference * cappedFraction;
  const dashEmpty = circumference - dashFilled;

  // Color follows the live severity bands (lib/contextUsage.ts: warn ≥70%,
  // danger ≥90%) with the design's color ramp: accent → warning → danger.
  const arcColor =
    usage?.severity === "danger"
      ? "var(--color-danger)"
      : usage?.severity === "warn"
        ? "var(--color-warning)"
        : "var(--color-accent)";

  // Same defensive clamp as formatContextUsage: if fraction > 1 we
  // show "100%+" instead of an alarming impossible number. See the
  // doc in lib/contextUsage.ts for the catalog-staleness / legacy-
  // summary cases this guards against.
  const pct =
    usage?.fraction !== undefined
      ? usage.fraction > 1
        ? "100%+"
        : `${Math.max(0, Math.round(usage.fraction * 100))}%`
      : null;
  const titleText = (() => {
    if (isSummarizing) return "Compacting conversation…";
    if (!usage) return "Compact this conversation — replace earlier turns with a short summary so the next turn fits in the model's context window and stays cheap.";
    return `${formatContextUsage(usage)} — click to compact and free up context.`;
  })();

  return (
    <button
      type="button"
      aria-label={isSummarizing ? "Compacting conversation…" : pct !== null ? `Context ${pct} full — click to compact` : "Compact this conversation"}
      data-tip-top={isSummarizing ? "Compacting conversation…" : pct !== null ? `Compact conversation — ${pct} full` : "Compact conversation"}
      title={titleText}
      disabled={disabled}
      onClick={onClick}
      className="relative inline-flex h-[1.95rem] w-[1.95rem] shrink-0 items-center justify-center rounded-[var(--radius-pill)] text-[var(--color-text-secondary)] transition hover:bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-40"
    >
      {/* The design's .ring-svg: hugs the round button (inset 1px), with
          the percent conveyed by the arc + aria-label/tip, never text. */}
      <svg
        viewBox="0 0 32 32"
        fill="none"
        aria-hidden="true"
        className="pointer-events-none absolute inset-[1px] h-[calc(100%-2px)] w-[calc(100%-2px)]"
      >
        {/* Track — full circle at 26% accent (the design's .ring-track).
            Always rendered, even before the first turn summary lands, so
            the user doesn't see an icon flicker into a ring when the
            meter first arrives. Empty ring = "no usage data yet," not a
            different control. */}
        <circle
          cx="16"
          cy="16"
          r={r}
          stroke="color-mix(in srgb, var(--color-accent) 26%, transparent)"
          strokeWidth="2"
        />
        {/* Progress arc — rotated -90deg so the fill starts at the */}
        {/* top of the circle and walks clockwise. Length is 0 when */}
        {/* usage is null, which renders as nothing on top of the   */}
        {/* track.                                                  */}
        {usage ? (
          <circle
            cx="16"
            cy="16"
            r={r}
            stroke={arcColor}
            strokeWidth="2"
            strokeLinecap="round"
            strokeDasharray={`${dashFilled} ${dashEmpty}`}
            transform="rotate(-90 16 16)"
            style={{ transition: "stroke-dasharray 240ms ease, stroke 240ms ease" }}
          />
        ) : null}
      </svg>
      <Icon name="compress" className="size-3.5" />
    </button>
  );
}

// ── Turn summary chip ────────────────────────────────────────────────────
//
// Compact, one-line footer under the assistant reply showing what the
// turn cost, how long it took, and how many tools ran. Deliberately
// understated — this is a trust signal ("yes, you spent 3 cents"), not
// the hero content.

export function TurnSummaryChip({
  summary,
  toolCalls,
  pythonStreams,
}: {
  summary: TurnSummary;
  toolCalls: ToolCall[];
  pythonStreams: PythonStream[];
}) {
  const parts: ReactNode[] = [];
  const executionCount = toolCalls.length + pythonStreams.length;
  parts.push(<span key="cost">{formatUsd(summary.costUsd)}</span>);
  if (summary.durationMs > 0) parts.push(<span key="duration">{formatDuration(summary.durationMs)}</span>);
  if (executionCount > 0) {
    parts.push(
      <span key="execution">
        {toolCalls.length > 0 ? `${toolCalls.length} tool${toolCalls.length === 1 ? "" : "s"}` : null}
        {toolCalls.length > 0 && pythonStreams.length > 0 ? " + " : null}
        {pythonStreams.length > 0 ? `${pythonStreams.length} log${pythonStreams.length === 1 ? "" : "s"}` : null}
      </span>,
    );
  }
  const tokens = summary.promptTokens + summary.completionTokens;
  if (tokens > 0) parts.push(<span key="tokens">{formatTokens(tokens)} tokens</span>);
  const pct = cachedPercent(summary);
  if (pct !== null) parts.push(<span key="cached">{pct}% cached</span>);
  const modelLabel = shortModelName(summary.model);
  if (modelLabel) parts.push(<span key="model">{modelLabel}</span>);

  return (
    <p className="text-[0.7rem] text-[var(--color-text-muted)] tabular-nums self-start break-words">
      {summary.cancelled ? "stopped · " : ""}
      {parts.map((part, index) => (
        <span key={index}>
          {index > 0 ? " · " : null}
          {part}
        </span>
      ))}
    </p>
  );
}

/**
 * SummaryBanner renders a compaction marker — the message inserted by
 * the user-initiated "summarize and continue" flow. Reads as a
 * distinct phase boundary, not a regular assistant turn:
 *
 *   - Bordered, slightly accented background so the eye lands on it.
 *   - Caption naming the model that produced the summary + token cost
 *     (folded into ConversationTotalsChip elsewhere; surfaced here
 *     only as model attribution).
 *   - Toggle to expand/collapse the pre-summary scroll.
 *   - Re-summarize button — replace semantics on the backend means a
 *     new call swaps this banner's content for a fresher summary.
 *
 * Earlier turns above the banner are dimmed (handled in the parent
 * loop) so users grok they remain in the transcript for reference but
 * are no longer in the model's live context.
 */
export function SummaryBanner({
  message,
  collapsedRangeCount,
  summaryExpanded,
  onToggleExpand,
  onResummarize,
  isSummarizing,
}: {
  message: Message;
  collapsedRangeCount: number;
  summaryExpanded: boolean;
  onToggleExpand: () => void;
  onResummarize: () => void;
  isSummarizing: boolean;
}) {
  const meta = message.summaryMeta;
  return (
    <div className="rounded-[0.85rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] p-3 sm:p-4">
      <div className="flex items-center justify-between gap-2 pb-2">
        <span className="inline-flex items-center gap-1.5 text-[0.72rem] font-medium uppercase tracking-wide text-[var(--color-text-muted)]">
          <Icon name="summarize" className="size-3.5" aria-hidden="true" />
          Conversation summary
        </span>
        <span className="flex items-center gap-1">
          {collapsedRangeCount > 0 ? (
            <button
              type="button"
              className="rounded-full px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={onToggleExpand}
              aria-expanded={summaryExpanded}
            >
              {summaryExpanded ? `Collapse earlier ${collapsedRangeCount} turn${collapsedRangeCount === 1 ? "" : "s"}` : `Show earlier ${collapsedRangeCount} turn${collapsedRangeCount === 1 ? "" : "s"}`}
            </button>
          ) : null}
          <button
            type="button"
            className="rounded-full px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-50"
            onClick={onResummarize}
            disabled={isSummarizing}
            title="Compact again against the current scroll. Replace semantics: this banner's text is swapped, not chained."
          >
            {isSummarizing ? "Compacting…" : "Compact again"}
          </button>
        </span>
      </div>
      <div className="whitespace-pre-wrap text-[0.86rem] leading-[1.55] text-[var(--color-text-primary)] sm:text-[0.92rem]">
        {message.content}
      </div>
      {meta?.model || (meta?.costUsd ?? 0) > 0 ? (
        <p className="mt-2 text-[0.65rem] text-[var(--color-text-muted)] tabular-nums">
          {meta?.model ? <>Compacted by <span className="font-medium">{shortModelName(meta.model)}</span></> : null}
          {(meta?.costUsd ?? 0) > 0 ? <> · {meta!.costUsd! < 0.01 ? `$${meta!.costUsd!.toFixed(4)}` : `$${meta!.costUsd!.toFixed(2)}`}</> : null}
          {meta?.promptTokens ? <> · {meta.promptTokens.toLocaleString()} prompt tokens condensed</> : null}
        </p>
      ) : null}
    </div>
  );
}

/**
 * ConversationTotalsChip sits above the composer and answers the "is caching
 * actually paying off overall, and what has this chat cost me?" question in
 * one glanceable line. Hidden until at least one turn has completed.
 *
 * Rendered in the same muted micro-type as the per-turn chip so the two read
 * as a set rather than competing signals. On mobile we drop the "turns"
 * suffix if there's no room — cost + cache% are the load-bearing numbers.
 */
export function ConversationTotalsChip({
  messages,
  usage,
}: {
  messages: Message[];
  // Provided by the parent so both the stats panel and the composer
  // ring read from the same computation — no duplicate memos, no
  // chance of the two surfaces disagreeing.
  usage: ContextUsage | null;
}) {
  const summaries = useMemo(
    () =>
      messages
        .map((m) => m.summary)
        .filter((s): s is TurnSummary => !!s),
    [messages],
  );

  if (summaries.length === 0) return null;
  const totals = conversationTotals(summaries);
  if (totals.turns === 0) return null;

  const parts: string[] = [];
  parts.push(`Σ ${formatUsd(totals.costUsd)}`);
  if (totals.cachedPercent !== null) parts.push(`${totals.cachedPercent}% cached`);
  parts.push(`${totals.turns} turn${totals.turns === 1 ? "" : "s"}`);
  if (usage) parts.push(formatContextUsage(usage));

  // Only the context-usage cell color-shifts; the rest stays muted so
  // the bar reads as a single chip with one variable bit.
  const contextColor =
    usage?.severity === "danger"
      ? "var(--color-danger)"
      : usage?.severity === "warn"
        ? "var(--color-warning)"
        : "var(--color-text-muted)";

  return (
    <div className="flex flex-col items-end gap-0.5">
      <p
        className="text-[0.7rem] text-[var(--color-text-muted)] tabular-nums text-right"
        aria-label="Conversation totals"
        title="Cumulative cost, token-weighted cache-hit rate, and context-window usage across this chat."
      >
        {parts.slice(0, -1).join(" · ")}
        {usage ? (
          <>
            {parts.length > 1 ? " · " : ""}
            <span style={{ color: contextColor }}>{parts[parts.length - 1]}</span>
          </>
        ) : (
          parts[parts.length - 1]
        )}
      </p>
      {shouldShowDegradationCaption(usage) ? (
        <p
          className="text-[0.65rem] text-[var(--color-text-muted)] text-right italic"
          role="note"
        >
          Long chats may produce shallower analysis — context fills, recall fades.
        </p>
      ) : null}
    </div>
  );
}

// PendingAttachmentChip renders a single attachment chip in the composer's
// pending tray. Image attachments get a small inline thumbnail preview built
// from a blob URL of the local File so the user can see what's about to be
// sent — useful confirmation when an image is being attached for vision
// input. Non-image attachments fall back to the original paperclip chip.
export function PendingAttachmentChip({
  attachment,
  onRemove,
  removalDisabled,
}: {
  attachment: PendingAttachment;
  onRemove: () => void;
  removalDisabled: boolean;
}) {
  const isImage = isImageMimeOrName(attachment.mime, attachment.name);
  // Compute the blob URL synchronously via useMemo so we don't trigger a
  // cascading re-render on first paint. The URL is owned by this component
  // for as long as the attachment chip is mounted; the cleanup-effect below
  // revokes it when the component unmounts or the underlying File changes
  // (which happens when the user removes the chip and adds a new one).
  const previewUrl = useMemo(
    () => (isImage ? URL.createObjectURL(attachment.file) : null),
    [isImage, attachment.file],
  );
  useEffect(() => {
    if (!previewUrl) return;
    return () => URL.revokeObjectURL(previewUrl);
  }, [previewUrl]);

  return (
    <span
      className={[
        "inline-flex items-center gap-1.5 rounded-full border px-2 py-1 text-[0.72rem]",
        attachment.status === "error"
          ? "border-[var(--color-danger)] text-[var(--color-danger)]"
          : "border-[var(--color-border-strong)] text-[var(--color-text-secondary)]",
        attachment.status === "uploading" ? "opacity-60" : "",
      ].join(" ")}
      title={`${attachment.name} (${formatBytes(attachment.size)})`}
    >
      {isImage && previewUrl ? (
        // eslint-disable-next-line @next/next/no-img-element
        <img
          src={previewUrl}
          alt=""
          className="size-4 rounded-sm object-cover"
          draggable={false}
        />
      ) : (
        <Icon name="paperclip" className="size-3" />
      )}
      <span className="max-w-[14rem] truncate">{attachment.name}</span>
      <span className="text-[0.65rem] text-[var(--color-text-muted)]">
        {formatBytes(attachment.size)}
      </span>
      <button
        type="button"
        aria-label={`Remove ${attachment.name}`}
        className="hit-area touch-target-hit text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        disabled={removalDisabled}
        onClick={onRemove}
      >
        <Icon name="close" className="size-3" />
      </button>
    </span>
  );
}

// ── "Shared with team" chip (chat header) ────────────────────────────────
//
// The same pill the project home puts beside a team-shared project's name,
// reused on the one chat surface that stays visible while you type.
//
// The rail row and the project-home row both badge a team-shared chat, and
// both of those are gone exactly when it matters: the rail collapses, and
// under 900px it is a drawer. So a member composing in a shared chat had no
// way to see that what they were typing would be readable by their team
// without leaving the chat. It is a real button, because the thing a user
// wants when they notice the badge is the dialog that controls it.
//
// The audience is NAMED, never inferred (ADR-0057): the team the chat is
// stamped with, falling back to "your team" when the caller hasn't loaded a
// team name. The label collapses to the glyph on a phone, the same way the
// Lockdown badge next door does; the accessible name carries the audience at
// every width.
export function TeamSharedChip({
  audience,
  onClick,
}: {
  audience?: string;
  onClick: () => void;
}) {
  const label = `Shared with ${audience || "your team"}`;
  return (
    <button
      type="button"
      aria-label={`${label} — open sharing`}
      title={`${label} — read-only. Click to change sharing.`}
      data-testid="chat-header-team-shared-chip"
      onClick={onClick}
      className="inline-flex shrink-0 items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-0.5 text-[0.7rem] text-[var(--color-text-muted)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
    >
      <TeamGlyph className="size-3 shrink-0" />
      <span className="hidden sm:inline">Shared with team</span>
    </button>
  );
}

// isImageMimeOrName returns true when an attachment looks like an image based
// on its declared MIME (preferred) or filename extension (fallback for clients
// that omit the Content-Type — e.g. some drag-and-drop sources). Mirrors the
// allow list the Go side uses in tools.IsImageMIME / tools.ImageMIMEFromName.
function isImageMimeOrName(mime: string, name: string): boolean {
  const m = mime.trim().toLowerCase();
  if (m === "image/png" || m === "image/jpeg" || m === "image/jpg" || m === "image/gif" || m === "image/webp") {
    return true;
  }
  const idx = name.lastIndexOf(".");
  if (idx < 0) return false;
  const ext = name.slice(idx).toLowerCase();
  return ext === ".png" || ext === ".jpg" || ext === ".jpeg" || ext === ".gif" || ext === ".webp";
}
