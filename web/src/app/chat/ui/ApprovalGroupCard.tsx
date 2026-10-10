"use client";

// The grouped approval card (docs/GROUPED-APPROVALS.md). When one turn stages
// two or more cards for tools the bundle lists in
// agent_policy.critical_tool_group_approval, they render as this one card:
// "N actions to approve", one row per call (its readable card when the bundle
// declares a describer, else its arguments), a checkbox per row, and three
// buttons:
//
//   - Approve all (k): one POST to the group endpoint that approves the
//     checked calls and declines the unchecked ones. The server checks the
//     whole decision before claiming anything, then decides each call exactly
//     as its own card would (its own claim, budget and outcome).
//   - One at a time: falls back to today's individual cards.
//   - Cancel all: declines every call.
//
// The countdown is the earliest deadline among the calls. Each call keeps its
// own seat badge. Once decided, a call leaves the group and shows its own
// outcome card (running, applied, declined …), so the executing and resolved
// states are the individual cards' states.

import { useState } from "react";
import {
  type Approval,
  type ApprovalOutcomePayload,
  approvalStatusFromOutcome,
} from "./history";
import {
  ApprovalCountdown,
  ApprovalSeatBadge,
  ApprovalSubmitError,
  actionLabel,
  describeSubmitFailure,
  useApprovalCountdown,
} from "./ApprovalCards";
import { ApprovalCardDetails, ApprovalReadableBody } from "./ApprovalReadableCard";
import { conversationApiPath } from "@/app/lib/conversationApiUrl";
import { earliestExpiry } from "./approvalGroups";

type GroupResult = {
  approval_id: string;
  decision: "approve" | "decline";
  status_code: number;
  result?: ApprovalOutcomePayload & { resume?: boolean };
  error?: string;
};

function RawArgs({ approval }: { approval: Approval }) {
  const { server } = actionLabel(approval.tool);
  const args = Array.isArray(approval.summary.args) ? approval.summary.args : [];
  const raw = approval.summary.raw ?? "";
  return (
    <>
      <div className="mb-1 text-[0.72rem] text-[var(--color-text-muted)]">
        {server ? (
          <>
            via <span className="font-mono">{server}</span>
            <span className="mx-1">·</span>
          </>
        ) : null}
        <span className="font-mono">{approval.tool}</span>
      </div>
      {args.length > 0 ? (
        <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
          {args.map((row) => (
            <div key={row.key} className="min-w-0">
              <span className="text-[var(--color-text-muted)]">{row.key}: </span>
              <span className="break-all">{row.value}</span>
            </div>
          ))}
        </div>
      ) : raw ? (
        <pre
          className="max-h-40 min-w-0 max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-overlay-strong)] p-2 text-[0.75rem] text-[var(--color-text-primary)]"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {raw}
        </pre>
      ) : null}
    </>
  );
}

function rowTitle(approval: Approval): string {
  if (approval.card) return approval.card.title;
  return `Run "${actionLabel(approval.tool).action}"`;
}

export function ApprovalGroupCard({
  groupId,
  approvals,
  conversationId,
  onResolved,
  onResumeExpected,
  onOneAtATime,
}: {
  groupId: string;
  // The group's pending, not-yet-running cards, in staging order.
  approvals: Approval[];
  conversationId: string;
  onResolved: (next: Approval) => void;
  onResumeExpected?: (executing: boolean) => void;
  onOneAtATime: () => void;
}) {
  // Unchecked ids. Every call starts checked, and a call that joins the group
  // while the card is open arrives checked too.
  const [unchecked, setUnchecked] = useState<ReadonlySet<string>>(() => new Set());
  const [submitting, setSubmitting] = useState<"approve" | "cancel" | null>(null);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const expiresAt = earliestExpiry(approvals);
  const countdown = useApprovalCountdown(expiresAt, "pending", false);

  const checkedIds = approvals.filter((a) => !unchecked.has(a.id)).map((a) => a.id);
  const uncheckedIds = approvals.filter((a) => unchecked.has(a.id)).map((a) => a.id);

  const decide = async (approve: string[], decline: string[]) => {
    if (submitting || !conversationId) return;
    setSubmitting(approve.length > 0 ? "approve" : "cancel");
    setSubmitError(null);
    try {
      const url = conversationApiPath(conversationId, "approval-groups", groupId);
      if (!url) {
        // Both ids are server-minted UUIDs; anything else is never sent.
        setSubmitError("This approval group has an invalid id and can't be sent.");
        return;
      }
      const response = await fetch(url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ approve, decline }),
      });
      if (!response.ok) {
        // Nothing was decided: the server refuses the whole decision before
        // it claims any call. Every card stays pending.
        setSubmitError(await describeSubmitFailure(response));
        return;
      }
      const data = (await response.json()) as { results?: GroupResult[]; resume?: boolean };
      const byId = new Map(approvals.map((a) => [a.id, a]));
      const failures: string[] = [];
      let anyExecuting = false;
      for (const r of data.results ?? []) {
        const approval = byId.get(r.approval_id);
        if (!approval) continue;
        if (r.status_code !== 200 || !r.result) {
          // This call was not decided by the request (a shutdown in
          // progress, say): it stays pending and can be decided again.
          if (r.error) failures.push(`${rowTitle(approval)}: ${r.error}`);
          continue;
        }
        const next = approvalStatusFromOutcome(r.result);
        if (next === null) {
          anyExecuting = true;
          onResolved({ ...approval, status: "pending", executing: true, resultText: r.result.result_text });
        } else {
          onResolved({ ...approval, status: next, resultText: r.result.result_text, executing: false });
        }
      }
      if (data.resume === true) onResumeExpected?.(anyExecuting);
      if (failures.length > 0) {
        setSubmitError(
          `${failures.length === 1 ? "One action was" : `${failures.length} actions were`} not decided (${failures.join("; ")}).`,
        );
      }
    } catch (err) {
      setSubmitError(
        `Couldn't reach the server (${err instanceof Error ? err.message : "request failed"}).`,
      );
    } finally {
      setSubmitting(null);
    }
  };

  const disabled = submitting !== null || countdown.expired;

  return (
    <div
      data-testid="approval-group-card"
      data-approval-group={groupId}
      className="rounded-[var(--radius-lg)] border bg-[color-mix(in_srgb,var(--color-overlay-soft)_55%,transparent)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={{ borderColor: "var(--color-accent)", color: "var(--color-text-primary)" }}
    >
      <div className="mb-2 flex min-w-0 items-center gap-2">
        <span aria-hidden>🛠️</span>
        <span data-testid="approval-group-title" className="min-w-0 break-words font-medium">
          {approvals.length} actions to approve
        </span>
      </div>

      <ul className="grid min-w-0 gap-2">
        {approvals.map((a) => {
          const checked = !unchecked.has(a.id);
          return (
            <li
              key={a.id}
              data-testid="approval-group-row"
              data-approval-id={a.id}
              data-tool={a.tool}
              className="min-w-0 rounded-md border border-[var(--color-border-subtle)] px-2 py-1.5"
            >
              <label className="flex min-w-0 items-start gap-2">
                <input
                  type="checkbox"
                  data-testid="approval-group-check"
                  checked={checked}
                  disabled={submitting !== null}
                  onChange={(e) => {
                    const on = e.target.checked;
                    setUnchecked((prev) => {
                      const next = new Set(prev);
                      if (on) next.delete(a.id);
                      else next.add(a.id);
                      return next;
                    });
                  }}
                  className="mt-1 size-3.5 shrink-0 accent-[var(--color-primary)]"
                />
                <span className="min-w-0 break-words font-medium">{rowTitle(a)}</span>
              </label>
              <div className="mt-1 min-w-0 pl-5">
                {a.card ? (
                  <>
                    <ApprovalReadableBody card={a.card} />
                    <ApprovalCardDetails>
                      <RawArgs approval={a} />
                    </ApprovalCardDetails>
                  </>
                ) : (
                  <RawArgs approval={a} />
                )}
                <div className="mt-1">
                  <ApprovalSeatBadge server={a.mcpServer} account={a.mcpAccount} verb="Runs as" />
                </div>
              </div>
            </li>
          );
        })}
      </ul>

      <div className="mt-3 flex flex-col gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            data-testid="approval-group-approve"
            className="rounded-full bg-[var(--color-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary)] transition hover:opacity-90 disabled:opacity-50"
            disabled={disabled || checkedIds.length === 0}
            onClick={() => void decide(checkedIds, uncheckedIds)}
          >
            {submitting === "approve" ? "Running…" : `Approve all (${checkedIds.length})`}
          </button>
          <button
            type="button"
            data-testid="approval-group-one-at-a-time"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={submitting !== null}
            onClick={onOneAtATime}
          >
            One at a time
          </button>
          <button
            type="button"
            data-testid="approval-group-cancel"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={disabled}
            onClick={() => void decide([], approvals.map((a) => a.id))}
          >
            {submitting === "cancel" ? "Cancelling…" : "Cancel all"}
          </button>
        </div>
        {uncheckedIds.length > 0 && checkedIds.length > 0 ? (
          <p className="text-[0.72rem] text-[var(--color-text-muted)]" data-testid="approval-group-unchecked-note">
            {uncheckedIds.length === 1 ? "The unchecked action is" : `The ${uncheckedIds.length} unchecked actions are`}{" "}
            declined.
          </p>
        ) : null}
        <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
        {countdown.expired ? (
          <p className="text-[0.72rem] text-[var(--color-text-muted)]">
            The earliest of these actions timed out. Use One at a time to see each one.
          </p>
        ) : null}
        <ApprovalSubmitError message={submitError} />
      </div>
    </div>
  );
}
