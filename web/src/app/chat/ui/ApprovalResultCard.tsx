"use client";

// The completed card read from the call's own result
// (docs/APPROVAL-RESULT-CARDS.md). A bundle that lists a critical tool in
// agent_policy.critical_tool_result_cards returns a `result_card` next to
// the tool's result; the server checks it strictly and serves it on a
// settled success only, and parseApprovalResultCardData (history.ts) checks
// it again. This module renders it the way the designer's completed card
// reads: a one-line outcome in the card's tone, a row per record with its
// ids, status and link, plain failure wording, "Retry failed (N)", and the
// pending card's records, the raw call and its raw result under Details.
//
// Fleet owns the status words and tones; the bundle owns the titles, notes
// and ids. Every string renders as React text.

import type { ReactNode } from "react";
import { useId, useMemo, useState } from "react";
import type {
  Approval,
  ApprovalResultCardData,
  ApprovalResultCardItem,
  ApprovalResultItemStatus,
  ApprovalResultOutcome,
} from "./history";
import { Icon } from "./Icon";
import {
  CARD_ITEMS_VISIBLE,
  CardRows,
  DetailsPanel,
  DetailsToggle,
  FlagBadge,
  RecordBox,
  RecordLink,
} from "./ApprovalReadableCard";

type Tone = { border: string; bg: string; fg: string; icon: string | null };

// One tone per outcome: success, warning or danger, and the accent for a
// mixed plan. The text stays the theme's primary text; the tone colours the
// border, a light wash, and the icon.
const OUTCOME_TONE: Record<ApprovalResultOutcome, Tone> = {
  done: { border: "var(--color-success-border)", bg: "var(--color-success)", fg: "var(--color-success)", icon: "check" },
  partial: { border: "var(--color-border-strong)", bg: "var(--color-accent)", fg: "var(--color-text-secondary)", icon: null },
  failed: { border: "var(--color-danger-border)", bg: "var(--color-danger)", fg: "var(--color-danger)", icon: "close" },
  mismatch: { border: "var(--color-warning-border)", bg: "var(--color-warning)", fg: "var(--color-warning)", icon: "warning" },
  unclear: { border: "var(--color-warning-border)", bg: "var(--color-warning)", fg: "var(--color-warning)", icon: "clock" },
};

// The words fleet puts on a record's status chip, and the lead-in of its note.
export const RESULT_STATUS_LABEL: Record<ApprovalResultItemStatus, string> = {
  done: "Done",
  failed: "Not applied",
  mismatch: "Check didn't match",
  unclear: "Outcome unclear",
  skipped: "Already done",
  not_run: "Not run",
};

// The icon each status shows (the bundled sprite, web/public/icons); a
// never-run record shows an empty dashed ring instead.
const STATUS_ICON: Record<ApprovalResultItemStatus, string | null> = {
  done: "check",
  failed: "close",
  mismatch: "warning",
  unclear: "clock",
  skipped: "check",
  not_run: null,
};

function StatusGlyph({ status, size = "size-3" }: { status: ApprovalResultItemStatus; size?: string }) {
  const name = STATUS_ICON[status];
  return name ? <Icon name={name} className={`${size} shrink-0`} /> : null;
}

function statusColor(status: ApprovalResultItemStatus): string {
  switch (status) {
    case "done":
    case "skipped":
      return "var(--color-success)";
    case "failed":
      return "var(--color-danger)";
    case "mismatch":
    case "unclear":
      return "var(--color-warning)";
    default:
      return "var(--color-text-muted)";
  }
}

// retryFailedPrompt is the user turn "Retry failed (N)" sends. It names the
// approved call and the bundle's retryable keys, and asks for a NEW approval:
// the button never runs anything itself.
export function retryFailedPrompt(approval: Approval, keys: string[]): string {
  const n = keys.length;
  const what = `${n} failed item${n === 1 ? "" : "s"}`;
  const list = keys.map((k) => JSON.stringify(k)).join(", ");
  return `Retry the ${what} from the approved ${approval.tool} call (approval_id=${approval.id}): ${list}. Retry only ${n === 1 ? "this one" : "these"}, and stage the retry for my approval.`;
}

function StatusChip({ status }: { status: ApprovalResultItemStatus }) {
  const color = statusColor(status);
  return (
    <span
      data-testid="approval-result-status"
      data-status={status}
      className="inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 text-[0.68rem] font-medium"
      style={{ borderColor: `color-mix(in srgb, ${color} 45%, transparent)`, color }}
    >
      <StatusGlyph status={status} />
      {RESULT_STATUS_LABEL[status]}
    </span>
  );
}

// Note renders a record's note, led by fleet's words for a failure so the
// state reads the same on every bundle: "Not applied: …".
function Note({ item }: { item: ApprovalResultCardItem }) {
  if (!item.note) return null;
  const lead =
    item.status === "failed" ? "Not applied:" : item.status === "mismatch" ? "Applied, but the read-back didn't match:" : "";
  return (
    <p data-testid="approval-result-note" className="mt-1 min-w-0 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
      {lead ? (
        <span className="font-semibold" style={{ color: statusColor(item.status) }}>
          {lead}{" "}
        </span>
      ) : null}
      {item.note}
    </p>
  );
}

function Ids({ item }: { item: ApprovalResultCardItem }) {
  if (!item.id && !item.internalId && !item.link) return null;
  return (
    <div className="mt-0.5 flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-0.5">
      {item.id ? (
        <span data-testid="approval-result-id" className="break-all font-mono text-[0.75rem] text-[var(--color-text-muted)]">
          {item.id}
        </span>
      ) : null}
      {item.internalId ? (
        <span data-testid="approval-result-internal-id" className="break-all text-[0.72rem] text-[var(--color-text-muted)]">
          internal <span className="font-mono">{item.internalId}</span>
        </span>
      ) : null}
      {item.link ? <RecordLink link={item.link} linkLabel={item.linkLabel} /> : null}
    </div>
  );
}

// ResultRow is one record of a several-record result: status icon, name,
// ids and link, its note.
function ResultRow({ item }: { item: ApprovalResultCardItem }) {
  return (
    <li data-testid="approval-result-item" data-status={item.status} className="flex min-w-0 gap-2.5 border-t border-[var(--color-border-subtle)] py-2 first:border-t-0">
      <span
        aria-hidden
        className={`mt-0.5 inline-flex size-5 shrink-0 items-center justify-center rounded-full ${item.status === "not_run" ? "border border-dashed border-[var(--color-text-muted)]" : ""}`}
        style={{ color: statusColor(item.status), background: item.status === "not_run" ? undefined : `color-mix(in srgb, ${statusColor(item.status)} 14%, transparent)` }}
      >
        <StatusGlyph status={item.status} />
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 flex-wrap items-baseline justify-between gap-x-2 gap-y-1">
          <span className={`min-w-0 break-words [overflow-wrap:anywhere] text-[0.82rem] font-medium ${item.status === "not_run" ? "text-[var(--color-text-muted)]" : "text-[var(--color-text-primary)]"}`}>
            {item.label}
          </span>
          <StatusChip status={item.status} />
        </div>
        <Ids item={item} />
        {item.flags && item.flags.length > 0 ? (
          <div className="mt-1 flex flex-wrap gap-1">
            {item.flags.map((f, i) => (
              <FlagBadge key={`${f.code}-${i}`} code={f.code} label={f.label} />
            ))}
          </div>
        ) : null}
        <Note item={item} />
        <CardRows changes={item.changes} settings={item.settings} />
      </div>
    </li>
  );
}

// ResultRecords lists the records. One record reads in its inner box; more
// than CARD_ITEMS_VISIBLE fold the done ones into one "N done · View all" row
// so the failures stay in view.
function ResultRecords({ items }: { items: ApprovalResultCardItem[] }) {
  const [showDone, setShowDone] = useState(false);
  const fold = items.length > CARD_ITEMS_VISIBLE;
  const done = useMemo(() => items.filter((it) => it.status === "done" || it.status === "skipped"), [items]);
  if (items.length === 0) return null;
  if (items.length === 1) {
    const it = items[0];
    return (
      <div className="mt-2 grid min-w-0 gap-2">
        <RecordBox testId="approval-result-item" label={it.label} flags={it.flags}>
          <div className="mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <StatusChip status={it.status} />
          </div>
          <Ids item={it} />
          <Note item={it} />
        </RecordBox>
        <CardRows changes={it.changes} settings={it.settings} />
      </div>
    );
  }
  const shown = fold && !showDone ? items.filter((it) => !(it.status === "done" || it.status === "skipped")) : items;
  return (
    <div className="mt-2 min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-1)] px-3">
      {fold && !showDone && done.length > 0 ? (
        <div data-testid="approval-result-done-summary" className="flex min-w-0 items-center justify-between gap-3 py-2 text-[0.82rem]">
          <span className="min-w-0">
            <span aria-hidden className="mr-2 inline-flex align-[-2px]" style={{ color: "var(--color-success)" }}>
              <Icon name="check" className="size-3.5" />
            </span>
            <span className="font-semibold text-[var(--color-text-primary)]">
              {done.length} {done.length === 1 ? "record" : "records"}
            </span>{" "}
            <span className="text-[var(--color-text-secondary)]">done</span>
          </span>
          <button
            type="button"
            data-testid="approval-result-view-all"
            className="shrink-0 text-[0.75rem] font-medium text-[var(--color-accent)] hover:underline"
            onClick={() => setShowDone(true)}
          >
            View all
          </button>
        </div>
      ) : null}
      <ul className={`min-w-0 ${fold && !showDone && done.length > 0 ? "border-t border-[var(--color-border-subtle)]" : ""}`}>
        {shown.map((it, i) => (
          <ResultRow key={`${it.key ?? it.id ?? ""}-${i}`} item={it} />
        ))}
      </ul>
    </div>
  );
}

// ApprovalResultCardView is the settled card for a call whose result carried
// an outcome card. `approvedTitle` is the pending card's title (the context
// the person approved); `approvedRecords`, `toolDetails` and `rawResult` sit
// under Details. A notify record (`recorded`) says it ran without asking and
// keeps its record text in view.
export function ApprovalResultCardView({
  approval,
  result,
  approvedTitle,
  approvedRecords,
  toolDetails,
  recorded,
  onRetryFailed,
}: {
  approval: Approval;
  result: ApprovalResultCardData;
  approvedTitle?: string;
  approvedRecords?: ReactNode;
  toolDetails?: ReactNode;
  recorded: boolean;
  // Resolves false when the retry turn was not accepted (a model error, an
  // upload or network failure): the button then offers itself again.
  onRetryFailed?: (approval: Approval, keys: string[]) => void | Promise<void | boolean>;
}) {
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [retry, setRetry] = useState<"idle" | "sending" | "sent">("idle");
  const detailsId = useId();
  // A notify record is information, not a decision: muted chrome, like
  // every record card, whatever the outcome.
  const tone: Tone = recorded
    ? { border: "var(--color-border-strong)", bg: "var(--color-text-muted)", fg: "var(--color-text-secondary)", icon: "book" }
    : OUTCOME_TONE[result.outcome];
  // A notify record never offers a retry: its tool runs without an approval,
  // so "Retry failed" would ask for a call that runs straight away (the
  // server leaves the retry out of a record's card too).
  const retryKeys = recorded ? [] : (result.retryKeys ?? []);
  const title = recorded ? `${result.title} · ran without asking` : result.title;
  return (
    <div
      data-approval-id={approval.id}
      data-tool={approval.tool}
      data-testid="generic-action-card"
      data-result-outcome={result.outcome}
      className="rounded-[var(--radius-lg)] border px-4 py-3 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)]"
      style={{ borderColor: tone.border, background: `color-mix(in srgb, ${tone.bg} 7%, var(--color-overlay-soft))` }}
    >
      <div data-testid="approval-result-card" className="min-w-0">
        <div className="flex min-w-0 items-start gap-2.5">
          {tone.icon ? (
            <span
              aria-hidden
              className="mt-0.5 inline-flex size-6 shrink-0 items-center justify-center rounded-full"
              style={{ color: tone.fg, background: `color-mix(in srgb, ${tone.bg} 16%, transparent)` }}
            >
              <Icon name={tone.icon} className="size-3.5" />
            </span>
          ) : null}
          <div className="min-w-0 flex-1">
            <div data-testid="generic-action-title" className="min-w-0 break-words [overflow-wrap:anywhere] text-[0.9rem] font-semibold">
              {title}
            </div>
            {approvedTitle ? (
              <div data-testid="approval-result-approved" className="min-w-0 break-words text-[0.75rem] text-[var(--color-text-muted)]">
                {recorded ? approvedTitle : `Approved: ${approvedTitle}`}
              </div>
            ) : null}
          </div>
        </div>
        {result.subtitle ? (
          <p className="mt-1.5 min-w-0 break-words text-[0.8rem] text-[var(--color-text-secondary)]">{result.subtitle}</p>
        ) : null}
        <ResultRecords items={result.items} />
        {result.footer ? (
          <p className="mt-1.5 min-w-0 break-words text-[0.72rem] text-[var(--color-text-muted)]">{result.footer}</p>
        ) : null}
        {recorded && approval.resultText ? (
          <p data-testid="approval-record-note" className="mt-1.5 min-w-0 break-words text-[0.72rem] text-[var(--color-text-muted)]">
            {approval.resultText}
          </p>
        ) : null}
        <div className="mt-3 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2">
          {retryKeys.length > 0 && onRetryFailed ? (
            <button
              type="button"
              data-testid="approval-retry-failed"
              disabled={retry !== "idle"}
              className="rounded-full bg-[var(--color-primary)] px-3.5 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary)] transition hover:opacity-90 disabled:opacity-50"
              onClick={async () => {
                setRetry("sending");
                let accepted: void | boolean = undefined;
                try {
                  accepted = await onRetryFailed(approval, retryKeys);
                } catch {
                  accepted = false;
                }
                setRetry(accepted === false ? "idle" : "sent");
              }}
            >
              {retry === "sent" ? "Retry asked" : retry === "sending" ? "Asking…" : `Retry failed (${retryKeys.length})`}
            </button>
          ) : null}
          <DetailsToggle open={detailsOpen} onToggle={() => setDetailsOpen((v) => !v)} controls={detailsId} />
        </div>
        <DetailsPanel id={detailsId} open={detailsOpen}>
          {approvedRecords}
          {toolDetails}
          {!recorded && approval.resultText ? (
            <pre
              data-testid="approval-result"
              className="mt-2 max-h-60 min-w-0 max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 text-[0.72rem] leading-[1.45] text-[var(--color-text-muted)]"
              style={{ fontFamily: "var(--font-code)" }}
            >
              {approval.resultText}
            </pre>
          ) : null}
        </DetailsPanel>
      </div>
    </div>
  );
}
