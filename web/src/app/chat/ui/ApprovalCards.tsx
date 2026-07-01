"use client";

// Approval / email-preview / memory card cluster extracted from
// chat-experience.tsx (slice 4 of #169). These cards turn a pending
// Approval (send/preview email, run bash, suggest the advanced model) or a
// MemoryProposal into the inline action card the user resolves from the
// transcript. The cluster is self-contained: each card POSTs its decision to
// the conversation/memory REST endpoints and reports the resolved state back
// through callbacks — it never reaches into ChatExperience's mutable closure
// state. Moving it here is a pure relocation; behavior, markup, class names,
// and the data-testid / data-approval-id / data-tool attributes are
// byte-identical to the in-module originals.

import type { ReactNode } from "react";
import { useCallback, useEffect, useRef, useState } from "react";
import {
  ADVANCED_MODEL,
  labelForModel,
} from "@/app/lib/modelAliases";
import {
  type Approval,
  type ApprovalStatus,
  type MemoryProposal,
} from "./history";

// Preview viewport presets for the inline email preview. These mirror the
// widths real clients render at — 375px ≈ iPhone portrait, 700px is the
// canonical desktop email width (matches the design-system viewer we
// lifted this pattern from). Anything wider than ~800 is unrealistic for
// real email clients, so we don't expose it.
type PreviewViewport = "mobile" | "desktop";
const PREVIEW_WIDTHS: Record<PreviewViewport, number> = {
  mobile: 375,
  desktop: 700,
};

// "inbox" swaps the stage background (not the email body) to mimic how a
// given email reads against a light vs dark client chrome. We deliberately
// do NOT try to simulate Outlook/Gmail's forced color inversion — that
// needs same-origin iframe scripting, which conflicts with the sandbox
// attribute we rely on for safety.
type PreviewInbox = "light" | "dark";

// useApprovalCountdown ticks once a second while a pending approval carries a
// future expires_at (#225), returning the seconds remaining and whether the
// deadline has lapsed. The SERVER-side sweep is the real enforcement; this is
// the visual countdown plus an at-expiry disable so the user can't click Send
// on a card the server is about to auto-deny. expires_at from the server is the
// ground truth — local clock skew only shifts the displayed mm:ss slightly.
function useApprovalCountdown(
  expiresAt: number | undefined,
  status: ApprovalStatus,
): { remaining: number | null; expired: boolean } {
  const hasDeadline =
    typeof expiresAt === "number" && expiresAt > 0 && status === "pending";
  const [remaining, setRemaining] = useState<number | null>(null);
  // Mirrors BulkDeleteConfirmModal's lint-clean countdown: read the clock and
  // call setState only inside the interval callback (never synchronously in the
  // effect body, never during render). The server-sent expires_at is the source
  // of truth, so a brief local clock skew only shifts the displayed m:ss. The
  // sub-second poll makes the first tick appear promptly.
  useEffect(() => {
    if (!hasDeadline) return undefined;
    const tick = () =>
      setRemaining(Math.max(0, expiresAt! - Math.floor(Date.now() / 1000)));
    const id = window.setInterval(tick, 250);
    return () => window.clearInterval(id);
  }, [hasDeadline, expiresAt]);
  // Mask the (possibly stale) stored value to null whenever there's no live
  // deadline, so a resolved card never shows a leftover countdown.
  const active = hasDeadline ? remaining : null;
  return { remaining: active, expired: active !== null && active <= 0 };
}

function formatCountdown(totalSeconds: number): string {
  const m = Math.floor(totalSeconds / 60);
  const s = totalSeconds % 60;
  return `${m}:${s.toString().padStart(2, "0")}`;
}

// ApprovalCountdown renders the inline "Auto-denying in m:ss" line (or the
// timed-out notice once the deadline passes). Renders nothing when the approval
// has no deadline (remaining === null), preserving the prior no-timeout UI.
function ApprovalCountdown({
  remaining,
  expired,
}: {
  remaining: number | null;
  expired: boolean;
}) {
  if (remaining === null) return null;
  return (
    <div
      data-testid="approval-countdown"
      className="flex items-center gap-1.5 text-[0.72rem]"
      style={{ color: expired ? "var(--color-danger)" : "var(--color-text-muted)" }}
    >
      {expired ? (
        <span>⏱ Timed out — the action was not taken.</span>
      ) : (
        <span>
          ⏱ Auto-denying in <span className="font-mono">{formatCountdown(remaining)}</span>
        </span>
      )}
    </div>
  );
}

export function ApprovalCard({
  approval,
  conversationId,
  onResolved,
  onModelSwitched,
  onSwitchAndRetry,
}: {
  approval: Approval;
  conversationId: string;
  onResolved: (next: Approval) => void;
  // suggest_advanced_model only: callback fired after the server confirms
  // the conversation has been pinned to a new model. Lets the caller sync
  // its local selectedModel state without a refetch.
  onModelSwitched?: (model: string) => void;
  // suggest_advanced_model only: fired AFTER onModelSwitched when the user
  // picked "Switch & retry". The caller is expected to re-submit the
  // prior user turn under the newly-pinned model.
  onSwitchAndRetry?: () => void | Promise<void>;
}) {
  const [submitting, setSubmitting] = useState<"send" | "cancel" | null>(null);
  // Both card kinds auto-expand: preview because seeing the render IS
  // the feature, send because users were missing the Send button when
  // the card landed below an already-expanded preview iframe and the
  // collapsed send card looked like just another header. Visual
  // differentiation (border style + tint) now does the disambiguation.
  const [expanded, setExpanded] = useState(true);
  const [viewport, setViewport] = useState<PreviewViewport>("desktop");
  const [inbox, setInbox] = useState<PreviewInbox>("light");
  const [showRaw, setShowRaw] = useState(false);
  // Batch approval (#300): when checked, the decision applies to every
  // subsequent call to this tool in the conversation (approve-all / deny-all)
  // so the agent isn't gated per call.
  const [applyAll, setApplyAll] = useState(false);

  // Approval timeout countdown (#225). Called unconditionally before the
  // bash/suggest early-returns to satisfy the rules of hooks; the bash card
  // runs its own countdown, and the suggest_advanced_model nudge intentionally
  // shows none.
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status);

  const resolve = async (approved: boolean) => {
    if (submitting || approval.status !== "pending" || !conversationId) return;
    setSubmitting(approved ? "send" : "cancel");
    try {
      const response = await fetch(
        `/api/conversations/${conversationId}/approvals/${approval.id}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ approved, scope: applyAll ? "session" : "once" }),
        },
      );
      if (!response.ok) {
        onResolved({ ...approval, status: "failed", resultText: await response.text() });
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        result_text?: string;
        is_err?: boolean;
      };
      onResolved({
        ...approval,
        status: data.is_err ? "failed" : data.status,
        resultText: data.result_text,
      });
    } catch (err) {
      onResolved({
        ...approval,
        status: "failed",
        resultText: err instanceof Error ? err.message : "Request failed.",
      });
    } finally {
      setSubmitting(null);
    }
  };

  if (approval.tool === "bash") {
    return (
      <BashApprovalCard
        approval={approval}
        submitting={submitting}
        onResolve={resolve}
      />
    );
  }

  if (approval.tool === "suggest_advanced_model") {
    return (
      <SuggestAdvancedModelCard
        approval={approval}
        conversationId={conversationId}
        onResolved={onResolved}
        onModelSwitched={onModelSwitched}
        onSwitchAndRetry={onSwitchAndRetry}
      />
    );
  }

  if (approval.tool === "schedule_task") {
    return (
      <ScheduleTaskCard
        approval={approval}
        submitting={submitting}
        onResolve={resolve}
      />
    );
  }

  const recipients = toRecipientList(approval.summary.to, approval.summary.cc, approval.summary.bcc);
  const subject = approval.summary.subject ?? "(no subject)";
  const from = approval.summary.from ?? "";
  const preview = approval.summary.preview ?? "";
  // Full body comes through on the new summary.content field; older
  // sessions that pre-date it will fall back to the truncated preview.
  const fullContent = approval.summary.content ?? preview;
  const isHtml = (approval.summary.content_type ?? "").toLowerCase().startsWith("text/html");
  const contentOverflow = approval.summary.content_overflow ?? false;
  const hasBody = fullContent.length > 0;

  // preview_email uses the same card chrome but means "look, don't
  // send." Auto-expand ON FIRST RENDER (the whole point of the card)
  // but still let the user collapse. expanded defaults to true
  // via the initializer below.
  const isPreviewOnly = approval.tool === "preview_email";
  const effectiveExpanded = expanded;

  // Pending preview and pending send used to share the same accent border,
  // which made stacked cards visually indistinguishable. They now diverge:
  // preview reads as a quiet draft frame (muted border, dashed via the
  // className below, no background tint); send reads as an action card
  // (accent border + faint accent-tinted background).
  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? isPreviewOnly
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : isPreviewOnly
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
            : {
                borderColor: "var(--color-accent)",
                color: "var(--color-text-primary)",
                background:
                  "color-mix(in srgb, var(--color-accent) 14%, var(--color-overlay-soft))",
              };

  // Lead the title with an uppercase intent tag so a skimmer's eye
  // distinguishes draft cards from action cards before reading the rest.
  // The pending verb-phrase is what every existing e2e test asserts on
  // (e.g. /Send this email\?/i) — substring matches still pass.
  const title = isPreviewOnly
    ? approval.status === "pending"
      ? "DRAFT · Email preview (not sent)"
      : approval.status === "approved" || approval.status === "rejected"
        ? "Draft dismissed"
        : "Preview failed"
    : approval.status === "pending"
      ? "ACTION REQUIRED · Send this email?"
      : approval.status === "approved"
        ? "Email sent ✓"
        : approval.status === "rejected"
          ? "Send cancelled"
          : "Send failed";

  // Pending preview gets a dashed border to signal "draft / not real /
  // sketch." Pending send keeps the solid border (the default `border`
  // class) so the two cards are distinguishable at a glance even before
  // a user reads the title. Resolved states fall back to solid for both
  // — once a card is finalized the draft/action distinction no longer
  // matters and the success/danger color tells the story.
  const isPendingPreview = isPreviewOnly && approval.status === "pending";
  const borderStyleClass = isPendingPreview ? " border-dashed" : "";
  // No tint override needed for pending send — the inline `background`
  // in statusStyle replaces the default. For all other states keep the
  // existing soft overlay.
  const bgClass = approval.status === "pending" && !isPreviewOnly
    ? ""
    : " bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)]";

  return (
    <div
      data-approval-id={approval.id}
      className={`rounded-[0.95rem] border px-3 py-2.5 text-[0.8125rem] leading-[1.5]${borderStyleClass}${bgClass}`}
      style={statusStyle}
    >
      <div className="mb-2 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <span aria-hidden>{isPreviewOnly ? "👁" : "📤"}</span>
          <span className="font-medium">{title}</span>
        </div>
        {hasBody ? (
          <button
            type="button"
            className="text-[0.72rem] underline text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
            onClick={() => setExpanded((v) => !v)}
          >
            {/*
             * Send cards never use the word "preview" in the toggle —
             * a user hunting for the Send button used to mistake the
             * collapsed send card's "Preview email" toggle for the
             * preview card itself. Preview cards keep the original
             * "preview" wording since that's literally what they are.
             */}
            {isPreviewOnly
              ? expanded ? "Hide preview" : "Show preview"
              : expanded ? "Hide email" : "Show email"}
          </button>
        ) : null}
      </div>

      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {recipients ? <div><span className="text-[var(--color-text-muted)]">To: </span>{recipients}</div> : null}
        <div><span className="text-[var(--color-text-muted)]">Subject: </span>{subject}</div>
        {from ? <div><span className="text-[var(--color-text-muted)]">From: </span>{from}</div> : null}
      </div>

      {effectiveExpanded && hasBody ? (
        <EmailPreview
          html={fullContent}
          isHtml={isHtml}
          viewport={viewport}
          inbox={inbox}
          showRaw={showRaw}
          contentOverflow={contentOverflow}
          onViewportChange={setViewport}
          onInboxChange={setInbox}
          onShowRawChange={setShowRaw}
        />
      ) : null}

      {approval.status === "pending" ? (
        isPreviewOnly ? (
          <div className="mt-3 flex items-center gap-2">
            <button
              type="button"
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => void resolve(true)}
            >
              {submitting === "send" ? "Dismissing…" : "Dismiss"}
            </button>
          </div>
        ) : (
          <div className="mt-3 flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <button
                type="button"
                className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
                disabled={submitting !== null || countdown.expired}
                onClick={() => void resolve(true)}
              >
                {submitting === "send" ? "Sending…" : applyAll ? "Send + allow all" : "Send"}
              </button>
              <button
                type="button"
                className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
                disabled={submitting !== null}
                onClick={() => void resolve(false)}
              >
                {submitting === "cancel" ? "Cancelling…" : applyAll ? "Deny + block all" : "Cancel"}
              </button>
            </div>
            <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
            {/* Batch approval (#300): pre-approve/deny the rest of this tool's
                calls for the conversation so the agent isn't gated per call. */}
            <label className="flex items-center gap-1.5 text-[0.72rem] text-[var(--color-text-muted)]">
              <input
                type="checkbox"
                data-testid="approval-apply-all"
                checked={applyAll}
                disabled={submitting !== null}
                onChange={(e) => setApplyAll(e.target.checked)}
                className="size-3.5 accent-[var(--color-primary)]"
              />
              Apply my choice to all {approval.tool.replace(/^mcp_[^_]+_/, "")} calls in this chat
            </label>
          </div>
        )
      ) : approval.resultText ? (
        <p className="mt-2 text-[0.72rem] text-[var(--color-text-muted)]">{approval.resultText}</p>
      ) : null}
    </div>
  );
}

function BashApprovalCard({
  approval,
  submitting,
  onResolve,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  onResolve: (approved: boolean) => void;
}) {
  const command = approval.summary.command ?? approval.summary.preview ?? "";
  const workingDir = approval.summary.working_dir ?? "";
  const timeoutSec = approval.summary.timeout_seconds ?? 0;
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status);

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  return (
    <div
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>⚡</span>
        <span className="font-medium">
          {approval.status === "pending"
            ? "Run this shell command?"
            : approval.status === "approved"
              ? "Command executed"
              : approval.status === "rejected"
                ? "Command declined"
                : "Command failed"}
        </span>
      </div>

      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 font-mono text-[0.78rem] text-[var(--color-text-primary)]">
        {command}
      </pre>

      {(workingDir || timeoutSec) ? (
        <div className="mt-1 grid gap-0.5 text-[0.72rem] text-[var(--color-text-muted)]">
          {workingDir ? <div>cwd: {workingDir}</div> : null}
          {timeoutSec ? <div>timeout: {timeoutSec}s</div> : null}
        </div>
      ) : null}

      {approval.status === "pending" ? (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true)}
            >
              {submitting === "send" ? "Running…" : "Approve & run"}
            </button>
            <button
              type="button"
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : "Cancel"}
            </button>
          </div>
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
        </div>
      ) : approval.resultText ? (
        <pre className="mt-2 max-h-60 overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 font-mono text-[0.72rem] text-[var(--color-text-muted)]">
          {approval.resultText}
        </pre>
      ) : null}
    </div>
  );
}

// ScheduleTaskCard renders the approval card for a schedule_task call (#239):
// the user reviews the task name, prompt preview, schedule, and (for recurring
// tasks) the estimated run frequency, then Approves or Cancels. On approval the
// server creates the orchestrator task in-process; nothing is created until the
// user clicks Approve. Mirrors BashApprovalCard's chrome — no apply-all checkbox,
// since each scheduled task is a distinct, deliberate action.
function ScheduleTaskCard({
  approval,
  submitting,
  onResolve,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  onResolve: (approved: boolean) => void;
}) {
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status);
  const s = approval.summary;
  const name = (s.name ?? "").trim();
  const promptPreview = s.prompt_preview ?? "";

  // Render the schedule as a single human-readable line.
  const scheduleLine = s.recurring
    ? `Recurring · cron ${s.cron ?? ""}`
    : s.run_at
      ? `One-time · ${formatRunAt(s.run_at)}`
      : "Runs as soon as a worker is free";
  const frequencyLine =
    s.recurring && typeof s.runs_per_month === "number"
      ? `≈ ${s.runs_per_month >= 1000 ? "1000+" : s.runs_per_month} run${s.runs_per_month === 1 ? "" : "s"} / month`
      : null;

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const title =
    approval.status === "pending"
      ? "ACTION REQUIRED · Schedule this task?"
      : approval.status === "approved"
        ? "Task scheduled ✓"
        : approval.status === "rejected"
          ? "Scheduling cancelled"
          : "Scheduling failed";

  return (
    <div
      data-approval-id={approval.id}
      data-tool="schedule_task"
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>🗓️</span>
        <span className="font-medium">{title}</span>
      </div>

      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {name ? (
          <div>
            <span className="text-[var(--color-text-muted)]">Name: </span>
            {name}
          </div>
        ) : null}
        <div>
          <span className="text-[var(--color-text-muted)]">Schedule: </span>
          {scheduleLine}
        </div>
        {frequencyLine ? (
          <div>
            <span className="text-[var(--color-text-muted)]">Frequency: </span>
            {frequencyLine}
          </div>
        ) : null}
        {s.model ? (
          <div>
            <span className="text-[var(--color-text-muted)]">Model: </span>
            {s.model}
          </div>
        ) : null}
        {s.allow_network ? (
          <div>
            <span className="text-[var(--color-text-muted)]">Network: </span>
            egress allowed
          </div>
        ) : null}
      </div>

      {promptPreview ? (
        <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-[var(--color-overlay-strong)] p-2 text-[0.75rem] text-[var(--color-text-primary)]">
          {promptPreview}
        </pre>
      ) : null}

      {approval.status === "pending" ? (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true)}
            >
              {submitting === "send" ? "Scheduling…" : "Approve & schedule"}
            </button>
            <button
              type="button"
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : "Cancel"}
            </button>
          </div>
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
        </div>
      ) : approval.resultText ? (
        <p className="mt-2 whitespace-pre-wrap text-[0.72rem] text-[var(--color-text-muted)]">
          {approval.resultText}
        </p>
      ) : null}
    </div>
  );
}

// formatRunAt renders an ISO-8601 instant as a readable local datetime for the
// schedule_task card, falling back to the raw string if it doesn't parse.
function formatRunAt(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}

// SuggestAdvancedModelCard renders an inline nudge when the agent
// detects it's stuck on a workload that the advanced tier would handle
// better. Three actions:
//   - Switch & retry (default): pin the conversation to advanced and
//     immediately re-submit the prior user turn under the new model.
//   - Just switch: pin to advanced; the user will compose the next
//     prompt themselves.
//   - Dismiss: close the card. The server records a rejection so the
//     gate's user-turn cooldown applies before another suggestion.
//
// The server is authoritative on the recommended slug
// (agent.AdvancedModelSlug) — we don't accept it from the agent.
type SuggestAction = "switch_and_retry" | "switch_only" | "dismiss";

function SuggestAdvancedModelCard({
  approval,
  conversationId,
  onResolved,
  onModelSwitched,
  onSwitchAndRetry,
}: {
  approval: Approval;
  conversationId: string;
  onResolved: (next: Approval) => void;
  onModelSwitched?: (model: string) => void;
  onSwitchAndRetry?: () => void | Promise<void>;
}) {
  const [pending, setPending] = useState<SuggestAction | null>(null);

  const reason = approval.summary.reason ?? "Advanced mode would handle this better.";
  const recommendedSlug = approval.summary.recommend_model ?? ADVANCED_MODEL;
  const recommendedLabel = labelForModel(recommendedSlug);

  const submit = async (action: SuggestAction) => {
    if (pending || approval.status !== "pending" || !conversationId) return;
    setPending(action);
    try {
      const response = await fetch(
        `/api/conversations/${conversationId}/approvals/${approval.id}`,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({
            approved: action !== "dismiss",
            action,
          }),
        },
      );
      if (!response.ok) {
        onResolved({ ...approval, status: "failed", resultText: await response.text() });
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        action?: SuggestAction;
        model?: string;
        result_text?: string;
      };
      onResolved({
        ...approval,
        status: data.status,
        resultText: data.result_text,
      });
      if (data.status === "approved" && data.model) {
        onModelSwitched?.(data.model);
        if (action === "switch_and_retry") {
          await onSwitchAndRetry?.();
        }
      }
    } catch (err) {
      onResolved({
        ...approval,
        status: "failed",
        resultText: err instanceof Error ? err.message : "Request failed.",
      });
    } finally {
      setPending(null);
    }
  };

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const title =
    approval.status === "pending"
      ? `Try ${recommendedLabel} for the rest of this chat?`
      : approval.status === "approved"
        ? `Switched to ${recommendedLabel}`
        : approval.status === "rejected"
          ? "Suggestion dismissed"
          : "Suggestion failed";

  return (
    <div
      data-approval-id={approval.id}
      data-tool="suggest_advanced_model"
      className="rounded-[0.95rem] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>✨</span>
        <span className="font-medium">{title}</span>
      </div>
      <p className="text-[0.78rem] text-[var(--color-text-secondary)]">{reason}</p>

      {approval.status === "pending" ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <button
            type="button"
            className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-white transition hover:opacity-90 disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_and_retry")}
          >
            {pending === "switch_and_retry" ? "Switching…" : "Switch & retry"}
          </button>
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_only")}
          >
            {pending === "switch_only" ? "Switching…" : "Just switch"}
          </button>
          <button
            type="button"
            className="rounded-full border border-transparent px-3 py-1.5 text-[0.75rem] text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("dismiss")}
          >
            {pending === "dismiss" ? "Dismissing…" : "Dismiss"}
          </button>
        </div>
      ) : approval.resultText ? (
        <p className="mt-2 text-[0.72rem] text-[var(--color-text-muted)]">{approval.resultText}</p>
      ) : null}
    </div>
  );
}

// EmailPreview renders a staged email exactly as SendGrid will ship it,
// inside a sandboxed iframe so no third-party markup can touch the host
// page. The mobile/desktop toggle is a fixed-width container (not CSS
// zoom) so CSS media queries in the email body fire the same way real
// mail clients evaluate them. The inbox light/dark swap just changes
// the stage background around the iframe — we intentionally don't try
// to fake client-side color inversion (which requires same-origin
// scripting and conflicts with sandboxing).
function EmailPreview({
  html,
  isHtml,
  viewport,
  inbox,
  showRaw,
  contentOverflow,
  onViewportChange,
  onInboxChange,
  onShowRawChange,
}: {
  html: string;
  isHtml: boolean;
  viewport: PreviewViewport;
  inbox: PreviewInbox;
  showRaw: boolean;
  contentOverflow: boolean;
  onViewportChange: (v: PreviewViewport) => void;
  onInboxChange: (i: PreviewInbox) => void;
  onShowRawChange: (v: boolean) => void;
}) {
  const viewportPx = PREVIEW_WIDTHS[viewport];
  const stageBg = inbox === "dark" ? "#121212" : "#f4f6fb";
  const iframeRef = useRef<HTMLIFrameElement | null>(null);
  // Track the viewport width so the Desktop button can be hidden on
  // phones — there's no point showing a 600px inbox width on a 360px
  // screen, it'll just look like a shrunken blob.
  const [windowWidth, setWindowWidth] = useState(
    typeof window === "undefined" ? 1024 : window.innerWidth,
  );
  useEffect(() => {
    const handler = () => setWindowWidth(window.innerWidth);
    window.addEventListener("resize", handler);
    return () => window.removeEventListener("resize", handler);
  }, []);
  // Reserve a bit of room for padding around the preview stage.
  const canShowDesktop = windowWidth >= PREVIEW_WIDTHS.desktop + 40;
  // If the user picked Desktop on a wide window and then resized the
  // window narrow, fall back to Mobile so nothing looks broken.
  useEffect(() => {
    if (!canShowDesktop && viewport === "desktop") {
      onViewportChange("mobile");
    }
  }, [canShowDesktop, viewport, onViewportChange]);

  // If the payload was clipped server-side, warn the user. Rendering a
  // partial HTML document can cut off mid-tag and look broken; still
  // better than hiding the preview entirely.
  const overflowNotice = contentOverflow
    ? "Body exceeded the 1 MiB preview cap — the tail is truncated. The full email will still be sent."
    : null;

  // Dark-mode color inversion. Real email clients (Outlook dark,
  // Gmail dark, Apple Mail dark) forcibly swap light backgrounds for
  // dark ones and bump text contrast. The preview can replicate that
  // by poking at the iframe's DOM post-load — which requires
  // sandbox="allow-same-origin" (no scripts still, so LLM HTML stays
  // inert). Color maps mirror the flag repo's email-preview.html so
  // emails rendered in both places look identical.
  // Stabilized with useCallback so the effect below can list it as a real
  // dependency (honest deps, no suppression) and the iframe onLoad handler
  // gets a stable identity. It only reads `inbox` plus the stable iframe ref
  // and module-level color maps, so `[inbox]` is the complete dep set.
  const applyInboxMode = useCallback(() => {
    const frame = iframeRef.current;
    if (!frame) return;
    try {
      const doc = frame.contentDocument;
      if (!doc) return;
      // Always start from original before applying a swap so
      // toggling back and forth is clean.
      const els = doc.querySelectorAll<HTMLElement>("[data-ep-orig]");
      els.forEach((el) => {
        const orig = el.getAttribute("data-ep-orig") || "";
        el.setAttribute("style", orig);
      });
      if (inbox !== "dark") {
        return;
      }
      doc.querySelectorAll<HTMLElement>("body, body *").forEach((el) => {
        if (!el.getAttribute("data-ep-orig")) {
          el.setAttribute("data-ep-orig", el.getAttribute("style") || "");
        }
        const cs = doc.defaultView?.getComputedStyle(el);
        if (!cs) return;
        const swapBG = DARK_BG_MAP[normRgb(cs.backgroundColor)];
        if (swapBG) el.style.setProperty("background-color", swapBG, "important");
        const swapText = DARK_TEXT_MAP[normRgb(cs.color)];
        if (swapText) el.style.setProperty("color", swapText, "important");
        const swapBorder = DARK_BORDER_MAP[normRgb(cs.borderTopColor)];
        if (swapBorder) {
          el.style.setProperty("border-top-color", swapBorder, "important");
          el.style.setProperty("border-right-color", swapBorder, "important");
          el.style.setProperty("border-bottom-color", swapBorder, "important");
          el.style.setProperty("border-left-color", swapBorder, "important");
        }
      });
    } catch {
      // Cross-origin or transient load state — ignore, next onLoad retries.
    }
  }, [inbox]);
  // Re-apply when the inbox mode flips or the HTML payload changes (the
  // iframe reloads, so the previous swap is gone). `html` stays a dep
  // because applyInboxMode itself doesn't read it — it's the reload trigger.
  useEffect(() => {
    applyInboxMode();
  }, [applyInboxMode, html]);

  if (!isHtml) {
    // Plain-text emails: stick with the simple monospaced view and
    // skip the viewport toolbar. Still wrap in a bordered box so it
    // visually matches the approval card styling.
    return (
      <div className="mt-2 min-w-0 max-w-full">
        {overflowNotice ? (
          <p className="mb-1 text-[0.7rem] text-[var(--color-text-muted)]">{overflowNotice}</p>
        ) : null}
        <pre
          className="max-h-[16rem] min-w-0 max-w-full overflow-auto rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.75rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {html}
        </pre>
      </div>
    );
  }

  return (
    <div className="mt-2 min-w-0 max-w-full overflow-hidden rounded-[0.6rem] border border-[var(--color-border)]">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.7rem]">
        <div className="flex flex-wrap items-center gap-2">
          <SegGroup label="Inbox">
            <SegButton active={inbox === "light"} onClick={() => onInboxChange("light")}>Light</SegButton>
            <SegButton active={inbox === "dark"} onClick={() => onInboxChange("dark")}>Dark</SegButton>
          </SegGroup>
          {canShowDesktop ? (
            <SegGroup label="Size">
              <SegButton active={viewport === "mobile"} onClick={() => onViewportChange("mobile")}>Mobile</SegButton>
              <SegButton active={viewport === "desktop"} onClick={() => onViewportChange("desktop")}>Desktop</SegButton>
            </SegGroup>
          ) : null}
        </div>
        <button
          type="button"
          className="underline text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
          onClick={() => onShowRawChange(!showRaw)}
        >
          {showRaw ? "Hide source" : "Show source"}
        </button>
      </div>

      {overflowNotice ? (
        <p className="border-b border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.7rem] text-[var(--color-text-muted)]">
          {overflowNotice}
        </p>
      ) : null}

      <div
        className="flex justify-center px-2 py-3 transition-colors"
        style={{ background: stageBg }}
      >
        <iframe
          ref={iframeRef}
          onLoad={applyInboxMode}
          key={`${viewport}`}
          title="Email preview"
          /* allow-same-origin lets the parent touch the iframe DOM to
             apply dark-mode color swaps. No allow-scripts → arbitrary
             LLM HTML still can't execute JS. */
          sandbox="allow-same-origin"
          srcDoc={html}
          className="rounded-[0.4rem] border border-black/10 bg-white transition-[width] duration-200"
          style={{ width: `${viewportPx}px`, maxWidth: "100%", height: "28rem" }}
        />
      </div>

      {showRaw ? (
        <pre
          className="max-h-[14rem] min-w-0 max-w-full overflow-auto border-t border-[var(--color-border)] bg-[var(--color-overlay-strong)] px-2 py-1.5 text-[0.72rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {html}
        </pre>
      ) : null}
    </div>
  );
}

// Dark-mode color maps ported from flag's email-preview.html. Keys are
// normalized rgb() strings — lowercased, whitespace-stripped — because
// the browser's getComputedStyle always returns colors in that form.
// Values are the tuned dark-mode hex substitutes; contrast ratios were
// picked in flag to hit WCAG AA against the new backgrounds.
const DARK_BG_MAP: Record<string, string> = {
  "rgb(244,246,251)": "#1e1e2e",
  "rgb(250,250,250)": "#2a2a3c",
  "rgb(238,243,255)": "#252538",
  "rgb(255,247,232)": "#2e2518",
  "rgb(239,250,243)": "#1a2e20",
  "rgb(255,255,255)": "#2a2a3c",
  "rgb(98,98,160)": "#5A5494",
};
const DARK_TEXT_MAP: Record<string, string> = {
  "rgb(20,24,36)": "#e0e0e0",
  "rgb(51,65,95)": "#c0c8d8",
  "rgb(88,111,124)": "#9aa8b4",
  "rgb(92,106,135)": "#9aa8b4",
  "rgb(143,90,18)": "#f0a040",
  "rgb(28,90,51)": "#4ADE80",
  "rgb(124,31,31)": "#FF6B6B",
};
const DARK_BORDER_MAP: Record<string, string> = {
  "rgb(215,222,238)": "#3a3a50",
  "rgb(230,235,245)": "#3a3a50",
  "rgb(240,215,175)": "#5a4520",
  "rgb(205,237,216)": "#2a5035",
};

function normRgb(c: string): string {
  return c.toLowerCase().replace(/\s+/g, "");
}

function SegGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="inline-flex items-center gap-1.5">
      <span className="text-[0.68rem] uppercase tracking-wide text-[var(--color-text-muted)]">{label}</span>
      <div className="inline-flex items-center gap-0.5 rounded-full border border-[var(--color-border-strong)] p-0.5">
        {children}
      </div>
    </div>
  );
}

function SegButton({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      className={[
        "rounded-full px-2 py-0.5 text-[0.68rem] transition",
        active
          ? "bg-[var(--color-text-primary)] text-[var(--color-surface-1)]"
          : "text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]",
      ].join(" ")}
      onClick={onClick}
    >
      {children}
    </button>
  );
}

function toRecipientList(...groups: Array<string | string[] | undefined>): string {
  const all: string[] = [];
  for (const g of groups) {
    if (!g) continue;
    if (Array.isArray(g)) all.push(...g.filter(Boolean));
    else all.push(g);
  }
  return all.join(", ");
}

// ── Memory proposal card ─────────────────────────────────────────────────

export function MemoryProposalCard({
  proposal,
  onResolved,
}: {
  proposal: MemoryProposal;
  onResolved: (next: MemoryProposal) => void;
}) {
  const [submitting, setSubmitting] = useState<"save" | "dismiss" | null>(null);

  const resolve = async (save: boolean) => {
    if (submitting || proposal.status !== "pending") return;
    setSubmitting(save ? "save" : "dismiss");
    try {
      if (save) {
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}/accept`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
        });
        if (!response.ok) {
          onResolved({ ...proposal, status: "dismissed" });
          return;
        }
        onResolved({ ...proposal, status: "saved" });
      } else {
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}`, {
          method: "DELETE",
        });
        if (!response.ok) {
          onResolved({ ...proposal, status: "dismissed" });
          return;
        }
        onResolved({ ...proposal, status: "dismissed" });
      }
    } catch {
      onResolved({ ...proposal, status: "dismissed" });
    } finally {
      setSubmitting(null);
    }
  };

  return (
    <div className="rounded-[0.9rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] p-3">
      <div className="mb-2 flex items-center gap-2 text-[0.75rem] font-medium text-[var(--color-text-primary)]">
        <span className="inline-flex size-5 items-center justify-center rounded-full bg-[var(--color-accent)] text-[0.65rem] text-[var(--color-surface-1)]">
          M
        </span>
        Save this memory?
        {proposal.kind && proposal.kind !== "fact" ? (
          <span className="rounded-full border border-[var(--color-border)] px-1.5 py-0.5 text-[0.65rem] font-normal text-[var(--color-text-muted)]">
            {proposal.kind}
          </span>
        ) : null}
      </div>
      <p className="mb-3 whitespace-pre-wrap text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
        {proposal.content}
      </p>
      {proposal.status === "pending" ? (
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="rounded-full bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(true)}
          >
            {submitting === "save" ? "Saving..." : "Save"}
          </button>
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(false)}
          >
            {submitting === "dismiss" ? "Dismissing..." : "Don't save"}
          </button>
        </div>
      ) : (
        <div className="text-[0.75rem] text-[var(--color-text-muted)]">
          {proposal.status === "saved" ? "Saved to memories." : "Dismissed."}
        </div>
      )}
    </div>
  );
}
