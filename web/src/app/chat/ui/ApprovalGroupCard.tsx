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
// A call whose tool declares a narrowable list argument
// (docs/APPROVAL-NARROWING.md) shows its readable rows with their own
// checkboxes: unticked rows are left out of that call (the request names the
// ticked values under `narrow`), and a call with every row unticked is
// declined.
//
// The countdown is the earliest deadline among the calls. Each call keeps its
// own seat badge. Once decided, a call leaves the group and shows its own
// outcome card (running, applied, declined …), so the executing and resolved
// states are the individual cards' states.

import { useContext, useState } from "react";
import {
  type Approval,
  type ApprovalOutcomePayload,
  approvalStatusFromOutcome,
  parseApprovalNarrowed,
  resultCardFromOutcome,
} from "./history";
import { approvedValuesFor, selectableKeys, selectedCountText, type RowSelection } from "./approvalNarrowing";
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
import { ConnectorNamesContext } from "./approvalToolLabel";

type GroupResult = {
  approval_id: string;
  decision: "approve" | "decline";
  status_code: number;
  result?: ApprovalOutcomePayload & { resume?: boolean };
  error?: string;
};

function RawArgs({ approval }: { approval: Approval }) {
  const names = useContext(ConnectorNamesContext);
  const { server } = actionLabel(approval.tool, approval.mcpServer, approval.mcpAccount, names);
  const args = Array.isArray(approval.summary.args) ? approval.summary.args : [];
  const raw = approval.summary.raw ?? "";
  return (
    <>
      <div className="mb-1 text-[0.72rem] text-[var(--color-card-text-muted)]">
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
              <span className="text-[var(--color-card-text-muted)]">{row.key}: </span>
              <span className="break-all">{row.value}</span>
            </div>
          ))}
        </div>
      ) : raw ? (
        <pre
          className="max-h-40 min-w-0 max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-surface-inset)] p-2 text-[0.75rem] text-[var(--color-text-primary)]"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {raw}
        </pre>
      ) : null}
    </>
  );
}

function rowTitle(approval: Approval, names: ReadonlyMap<string, string>): string {
  if (approval.card) return approval.card.title;
  return `Run "${actionLabel(approval.tool, approval.mcpServer, approval.mcpAccount, names).action}"`;
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
  const names = useContext(ConnectorNamesContext);
  const [unchecked, setUnchecked] = useState<ReadonlySet<string>>(() => new Set());
  // Per call, the rows of a narrowable card the person unticked.
  const [leftOut, setLeftOut] = useState<ReadonlyMap<string, ReadonlySet<string>>>(() => new Map());
  const [submitting, setSubmitting] = useState<"approve" | "cancel" | null>(null);
  const [submitError, setSubmitError] = useState<string | null>(null);
  const expiresAt = earliestExpiry(approvals);
  const countdown = useApprovalCountdown(expiresAt, "pending", false);

  // A narrowable call's approved values (null for a call approved whole).
  const approvedOf = (a: Approval): string[] | null => {
    if (!a.narrowable || selectableKeys(a).size === 0) return null;
    return approvedValuesFor(a.narrowable, leftOut.get(a.id) ?? new Set());
  };
  // A checked call with every row unticked is declined, like an unchecked one.
  const goes = (a: Approval) => !unchecked.has(a.id) && approvedOf(a)?.length !== 0;
  const checkedIds = approvals.filter(goes).map((a) => a.id);
  const uncheckedIds = approvals.filter((a) => !goes(a)).map((a) => a.id);
  const narrow: Record<string, string[]> = {};
  // The argument each narrowed card showed: the server refuses a narrowing of
  // any other argument (a policy that changed after a restart).
  const narrowArgs: Record<string, string> = {};
  for (const a of approvals) {
    const approved = approvedOf(a);
    if (goes(a) && approved && a.narrowable && approved.length < a.narrowable.values.length) {
      narrow[a.id] = approved;
      narrowArgs[a.id] = a.narrowable.arg;
    }
  }
  const selectionFor = (a: Approval): RowSelection | undefined => {
    if (!a.narrowable) return undefined;
    const keys = selectableKeys(a);
    if (keys.size === 0) return undefined;
    const out = leftOut.get(a.id) ?? new Set<string>();
    return {
      selectable: (key) => key !== undefined && keys.has(key),
      isLeftOut: (key) => key !== undefined && out.has(key),
      toggle: (key) =>
        setLeftOut((prev) => {
          const next = new Map(prev);
          const set = new Set(next.get(a.id) ?? []);
          if (set.has(key)) set.delete(key);
          else set.add(key);
          next.set(a.id, set);
          return next;
        }),
      disabled: submitting !== null || unchecked.has(a.id),
    };
  };

  const decide = async (
    approve: string[],
    decline: string[],
    narrowed: Record<string, string[]> = {},
    narrowedArgs: Record<string, string> = {},
  ) => {
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
        body: JSON.stringify(
          Object.keys(narrowed).length > 0
            ? { approve, decline, narrow: narrowed, narrow_args: narrowedArgs }
            : { approve, decline },
        ),
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
          // progress, say): it stays pending and can be decided again. The
          // reason rides the card too, because once the others settle the
          // group may no longer hold it and this card unmounts.
          if (r.error) {
            failures.push(`${rowTitle(approval, names)}: ${r.error}`);
            onResolved({ ...approval, decisionError: `Couldn't submit your decision: ${r.error}` });
          }
          continue;
        }
        const next = approvalStatusFromOutcome(r.result);
        if (next === null) {
          anyExecuting = true;
          onResolved({
            ...approval,
            status: "pending",
            executing: true,
            resultText: r.result.result_text,
            narrowed: parseApprovalNarrowed(r.result.narrowed),
            stoppable: r.result.stoppable === true || approval.stoppable,
            stopping: r.result.stopping === true || undefined,
          });
        } else {
          onResolved({
            ...approval,
            status: next,
            resultText: r.result.result_text,
            resultCard: resultCardFromOutcome(r.result),
            executing: false,
            narrowed: parseApprovalNarrowed(r.result.narrowed),
            // A stop recorded while the group request waited (docs/APPROVAL-STOP.md).
            stopped: r.result.stopped === true || undefined,
          });
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
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
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
          const selection = selectionFor(a);
          const approved = approvedOf(a);
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
                <span className="min-w-0 break-words font-medium">{rowTitle(a, names)}</span>
              </label>
              <div className="mt-1 min-w-0 pl-5">
                {selection && approved && a.narrowable ? (
                  <p
                    data-testid="approval-group-narrow-count"
                    aria-live="polite"
                    className="text-[0.72rem] font-medium text-[var(--color-text-secondary)]"
                  >
                    {selectedCountText(approved.length, a.narrowable.values.length)}
                  </p>
                ) : null}
                {a.card ? (
                  <>
                    <ApprovalReadableBody card={a.card} selection={selection} />
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
            className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
            disabled={disabled || checkedIds.length === 0}
            onClick={() => void decide(checkedIds, uncheckedIds, narrow, narrowArgs)}
          >
            {submitting === "approve" ? "Running…" : `Approve all (${checkedIds.length})`}
          </button>
          <button
            type="button"
            data-testid="approval-group-one-at-a-time"
            className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={submitting !== null}
            onClick={onOneAtATime}
          >
            One at a time
          </button>
          <button
            type="button"
            data-testid="approval-group-cancel"
            className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={disabled}
            onClick={() => void decide([], approvals.map((a) => a.id))}
          >
            {submitting === "cancel" ? "Cancelling…" : "Cancel all"}
          </button>
        </div>
        {uncheckedIds.length > 0 && checkedIds.length > 0 ? (
          <p className="text-[0.72rem] text-[var(--color-card-text-muted)]" data-testid="approval-group-unchecked-note">
            {uncheckedIds.length === 1 ? "The unchecked action is" : `The ${uncheckedIds.length} unchecked actions are`}{" "}
            declined.
          </p>
        ) : null}
        <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
        {countdown.expired ? (
          <div className="flex flex-wrap items-center gap-2">
            <p className="text-[0.72rem] text-[var(--color-card-text-muted)]">
              The earliest of these actions timed out. Use One at a time to see each one.
            </p>
            <button
              type="button"
              data-testid="approval-group-expired-one-at-a-time"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-2.5 py-1 text-[0.72rem] font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-bg-muted)]"
              onClick={onOneAtATime}
            >
              Review one at a time
            </button>
          </div>
        ) : null}
        <ApprovalSubmitError message={submitError} />
      </div>
    </div>
  );
}
