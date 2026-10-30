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
import { useCallback, useContext, useEffect, useId, useRef, useState } from "react";
import {
  currentAdvancedModel,
  labelForModel,
} from "@/app/lib/modelAliases";
import {
  type Approval,
  type ApprovalProgress,
  type ApprovalStatus,
  type MemoryProposal,
  approvalIsExecuting,
  approvalProgressText,
  type ApprovalOutcomePayload,
  approvalStatusFromOutcome,
  parseApprovalNarrowed,
  resultCardFromOutcome,
} from "./history";
import {
  leftOutOf,
  narrowedApproveLabel,
  narrowedSummary,
  selectedCountText,
  useNarrowing,
} from "./approvalNarrowing";
import { applyAllText, approvalToolLabel, ConnectorNamesContext, knownConnectorLabel, mcpToolServer } from "./approvalToolLabel";
import { Icon } from "./Icon";
import { useApprovalProgressPoll } from "./useApprovalProgressPoll";
import { EmailSendResult, parseEmailSendPayload } from "./ToolChips";
import {
  ApprovalCardDetails,
  ApprovalCardItems,
  ApprovalReadableBody,
  DetailsPanel,
  DetailsToggle,
  EyebrowCountdown,
  ReadablePendingRecords,
} from "./ApprovalReadableCard";
import { ApprovalResultCardView } from "./ApprovalResultCard";
import { conversationApiPath, conversationApprovalApiUrl } from "@/app/lib/conversationApiUrl";
import { Segmented } from "@/app/shared/ui/Segmented";
import { StatusChip } from "@/app/shared/ui/StatusChip";

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
export function useApprovalCountdown(
  expiresAt: number | undefined,
  status: ApprovalStatus,
  executing?: boolean,
): { remaining: number | null; expired: boolean } {
  const hasDeadline =
    typeof expiresAt === "number" && expiresAt > 0 && status === "pending" && !executing;
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

// ApprovalSeatBadge names the credential account an approved MCP call will run
// under (#167). An approval card outlives the turn that staged it, and the
// server reopens that turn's {server, account} seat to execute it — so when the
// turn was on a named account, the user is told which one BEFORE they click
// Send. The default seat renders nothing: there is no ambiguity to resolve.
// The verb defaults to the email card's "Sending as"; the generic action card
// passes "Runs as" since not every critical tool sends anything.
export function ApprovalSeatBadge({ server, account, verb = "Sending as" }: {
  server?: string;
  account?: string;
  verb?: string;
}) {
  const names = useContext(ConnectorNamesContext);
  if (!server || !account) return null;
  // The connector by its human name when one is known ("on Index
  // Exchange"), as the apply-all checkbox names it; the id, as code,
  // otherwise.
  const named = knownConnectorLabel(server, names);
  return (
    <div
      data-testid="approval-seat"
      className="flex items-center gap-1.5 text-[0.72rem]"
      style={{ color: "var(--color-card-text-muted)" }}
    >
      <span>
        {verb} <span className="font-mono">{account}</span> on{" "}
        {named ? named : <span className="font-mono">{server}</span>}
      </span>
    </div>
  );
}

// ApplyAllText is the batch-approval checkbox's label: the tool in words
// ("every Index Exchange “Execute plan” request"), never its raw id.
function ApplyAllText({ approval }: { approval: Approval }) {
  const names = useContext(ConnectorNamesContext);
  return <>{applyAllText(approvalToolLabel(approval.tool, approval.mcpServer, names, approval.mcpAccount))}</>;
}

// isEmailApprovalTool gates the email-shaped card. It used to be the implicit
// FALLBACK for every tool without a tailored card, so a bundle-declared pages
// deploy rendered as "Send this email?" / "Email sent ✓" — exactly the wrong
// copy on the one card class whose job is saying what is about to happen.
// Non-email tools now get GenericActionCard below.
function isEmailApprovalTool(tool: string): boolean {
  return tool === "preview_email" || tool === "send_email" || tool.endsWith("_send_email");
}

// isTimedOutApproval recognizes an auto-denied card by the server's stable
// "Approval timed out" result prefix (shared with the expiry sweep and the
// expired-click resolution) so the card can offer Ask again.
function isTimedOutApproval(approval: Approval): boolean {
  return approval.status === "rejected" && (approval.resultText ?? "").startsWith("Approval timed out");
}

// actionLabel splits an mcp_<server>_<tool> name into a humanized action
// ("Deploy page") and its server ("pages") for the generic card's header.
// Native tools have no server half and humanize whole.
export function actionLabel(
  tool: string,
  mcpServer?: string,
  mcpAccount?: string,
  names?: ReadonlyMap<string, string>,
): { action: string; server?: string } {
  // The action in words comes from the same label the apply-all checkbox
  // uses (approvalToolLabel), so a card names its tool one way. `server` is
  // the raw connector id, for the code-styled call line; mcpAccount keeps a
  // named seat's id out of the action. `names` (the chat's connectors)
  // splits an older row with no mcpServer the way the checkbox does.
  const server = mcpToolServer(tool, mcpServer, names);
  const { action } = approvalToolLabel(tool, server, names, mcpAccount);
  return server ? { action, server } : { action };
}

// askAgainPrompt is the canned user turn the Ask-again affordance submits: a
// timed-out approval's only recovery used to be composing "please try that
// again" by hand — the agent must re-stage (the old card's claim is spent and
// its args may be stale), so one click asks it to.
export function askAgainPrompt(approval: Approval, names?: ReadonlyMap<string, string>): string {
  const { action, server } = actionLabel(approval.tool, approval.mcpServer, approval.mcpAccount, names);
  const what = server ? `${action.toLowerCase()} (${approval.tool})` : action.toLowerCase();
  return `The approval card for ${what} timed out before I could act on it. Please stage it again — I'm ready to review it now.`;
}

// AskAgainButton renders the one-click recovery on a timed-out card. Kept a
// quiet secondary control: the card already explains what happened.
function AskAgainButton({ approval, onAskAgain }: {
  approval: Approval;
  onAskAgain?: (approval: Approval) => void;
}) {
  if (!onAskAgain) return null;
  return (
    <div className="mt-2">
      <button
        type="button"
        data-testid="approval-ask-again"
        className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)]"
        onClick={() => onAskAgain(approval)}
      >
        Ask again
      </button>
    </div>
  );
}

// ApprovalCountdown renders the inline "Auto-denying in m:ss" line (or the
// timed-out notice once the deadline passes). Renders nothing when the approval
// has no deadline (remaining === null), preserving the prior no-timeout UI.
export function ApprovalCountdown({
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
      style={{ color: expired ? "var(--color-danger-text)" : "var(--color-card-text-muted)" }}
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

// ApprovalSubmitError renders the reason the last Approve/Deny click did not
// reach the server — a non-2xx or a dropped connection. The card stays pending
// and its buttons re-enable, because the server still holds the approval open:
// marking the card "failed" locally (which this used to do) removed the only
// way to answer it while the agent kept waiting, and the countdown eventually
// auto-denied an action the user had already tried to approve. Only a
// server-reported terminal status moves the card out of pending.
// describeSubmitFailure turns a non-2xx approval POST into the one line the
// card shows. The status is always there; the body is included when it is a
// short plain sentence (the API's error strings), skipped when it is empty or
// a page of HTML from a proxy.
export async function describeSubmitFailure(response: Response): Promise<string> {
  let body = "";
  try {
    body = (await response.text()).trim();
  } catch {
    // Body unreadable — the status alone still says what happened.
  }
  const short = body.length > 0 && body.length <= 200 && !body.startsWith("<");
  return short
    ? `Couldn't submit your decision (HTTP ${response.status}): ${body}`
    : `Couldn't submit your decision (HTTP ${response.status}).`;
}

export function ApprovalSubmitError({
  message,
  running,
}: {
  message: string | null;
  running?: string | null;
}) {
  return (
    <>
      {running ? (
        <p
          role="status"
          data-testid="approval-still-running"
          className="text-[0.72rem] leading-[1.45]"
          style={{ color: "var(--color-text-secondary)" }}
        >
          {running}
        </p>
      ) : null}
      {message ? (
        <p
          role="alert"
          data-testid="approval-submit-error"
          className="text-[0.72rem] leading-[1.45]"
          style={{ color: "var(--color-danger-text)" }}
        >
          {message} Try again.
        </p>
      ) : null}
    </>
  );
}

// A claimed approval that is still running is not a new consent: no deny,
// edit, apply-all, or expiry countdown. "Check result" is the TUI's
// `/approve <id>` — an idempotent fetch of the outcome.
function ApprovalExecutingControls({
  progress,
  submitting,
  submitError,
  onCheck,
  onStop,
  stopping = false,
}: {
  progress?: ApprovalProgress;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  onCheck: () => void;
  // A tool in critical_tool_stop (docs/APPROVAL-STOP.md): Stop asks the
  // server to stop the call after its current item, unless the call has
  // already finished (the status says so, never that a stop was delivered).
  // Absent = no Stop button.
  onStop?: () => void;
  stopping?: boolean;
}) {
  const stopHintId = useId();
  return (
    <div className="mt-3 flex flex-col gap-2" data-testid="approval-executing">
      {progress ? <ApprovalProgressBar progress={progress} /> : null}
      {stopping ? (
        <p data-testid="approval-stopping" role="status" className="text-[0.75rem] text-[var(--color-text-secondary)]">
          Stop requested: unless the call had already finished, its server is asked to stop after its current item. The result says what ran.
        </p>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          data-testid="approval-check-result"
          className="w-fit rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
          disabled={submitting !== null}
          onClick={onCheck}
        >
          {submitting === "send" ? "Checking…" : "Check result"}
        </button>
        {onStop && !stopping ? (
          <>
            <button
              type="button"
              data-testid="approval-stop"
              aria-describedby={stopHintId}
              className="w-fit rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={onStop}
            >
              {submitting === "cancel" ? "Stopping…" : "Stop"}
            </button>
            <span id={stopHintId} className="text-[0.72rem] text-[var(--color-card-text-muted)]">
              Asks the server to stop after its current item; the result says what ran.
            </span>
          </>
        ) : null}
      </div>
      <ApprovalSubmitError
        message={submitError}
        running="This action is still running. Check result to see the outcome without running it again."
      />
    </div>
  );
}

// ApprovalProgressBar shows how far a running approved call has got, from the
// MCP progress its server reported (docs/APPROVAL-PROGRESS.md): a bar when the
// server knows the total, and "12 of 24 · message". Display only.
export function ApprovalProgressBar({ progress }: { progress: ApprovalProgress }) {
  const pct = progress.total ? Math.min(100, Math.round((progress.progress / progress.total) * 100)) : null;
  return (
    <div data-testid="approval-progress" className="flex min-w-0 flex-col gap-1">
      {pct !== null ? (
        <div
          role="progressbar"
          aria-label="Progress"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={pct}
          className="h-1.5 w-full overflow-hidden rounded-full bg-[var(--color-overlay-soft)]"
        >
          <div className="h-full rounded-full bg-[var(--color-progress)] transition-[width]" style={{ width: `${pct}%` }} />
        </div>
      ) : null}
      <span data-testid="approval-progress-text" className="min-w-0 break-words text-[0.72rem] text-[var(--color-text-secondary)]">
        {approvalProgressText(progress)}
      </span>
    </div>
  );
}

// ApprovalResult renders the text a resolved approval came back with.
//
// This existed four times in this file with three different treatments. Only the
// bash card capped and wrapped it; the email and advanced-model cards used a bare
// <p> with no wrapping at all, and the schedule card wrapped without a cap. Nobody
// expected an *email* result to be long — until one carried a 4 KB Pages deploy
// payload, and the card rendered it unwrapped and unbounded: JSON clipped mid-token
// at the card edge, no scrollbar, filling the viewport.
//
// A tool result is arbitrary text of arbitrary length, so there is one treatment:
// monospace, capped height with its own scroll, and break-all — because minified
// JSON offers no break opportunities and break-words alone will not wrap it.
function ApprovalResult({ text }: { text: string }) {
  if (!text) return null;
  return (
    <pre
      data-testid="approval-result"
      className="mt-2 max-h-60 min-w-0 max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-surface-inset)] p-2 text-[0.72rem] leading-[1.45] text-[var(--color-card-text-muted)]"
      style={{ fontFamily: "var(--font-code)" }}
    >
      {text}
    </pre>
  );
}

// EmailApprovalOutcome renders a resolved email approval's result. The send
// tool answers with a provider JSON payload, and this card used to dump it raw
// under "Email sent ✓" — which read as an error to the non-technical users the
// approval flow exists for. When the result parses as that payload, reuse the
// transcript chip's humanized treatment (status badge, warnings count, raw
// payload behind a "Delivery details" disclosure); anything else — network
// error strings, a cancelled send's note — keeps the raw view, which is the
// honest form for text we can't summarize.
function EmailApprovalOutcome({ text, failed }: { text: string; failed: boolean }) {
  const payload = parseEmailSendPayload(text);
  if (!payload) return <ApprovalResult text={text} />;
  return (
    <div className="mt-2 min-w-0 max-w-full">
      <EmailSendResult result={payload} isErr={failed} raw={text} />
    </div>
  );
}

export function ApprovalCard({
  approval,
  conversationId,
  onResolved,
  onModelSwitched,
  onSwitchAndRetry,
  onAskAgain,
  onResumeExpected,
  onRetryFailed,
  onMcpServersUpdated,
}: {
  approval: Approval;
  conversationId: string;
  onResolved: (next: Approval) => void;
  // A result card's "Retry failed (N)" (docs/APPROVAL-RESULT-CARDS.md). The
  // caller submits retryFailedPrompt(approval, keys) as a user turn, so the
  // agent stages the retry for a new approval; nothing runs from the click.
  onRetryFailed?: (approval: Approval, keys: string[]) => void | Promise<void | boolean>;
  // The server answered resume: this card's tool resumes after approval
  // (docs/RESUME-AFTER-APPROVAL.md), so fleet will start a turn on its own —
  // now (the card settled) or once the still-executing call lands.
  onResumeExpected?: (executing: boolean) => void;
  // suggest_advanced_model only: callback fired after the server confirms
  // the conversation has been pinned to a new model. Lets the caller sync
  // its local selectedModel state without a refetch.
  onModelSwitched?: (model: string) => void;
  // suggest_advanced_model only: fired AFTER onModelSwitched when the user
  // picked "Switch & retry". The caller is expected to re-submit the
  // prior user turn under the newly-pinned model.
  // Receives the model the conversation was just pinned to: the retry must
  // send it explicitly, because the caller's selected-model state has not
  // re-rendered yet and would resend (and re-pin) the old model.
  onSwitchAndRetry?: (model: string) => void | Promise<void>;
  // Fired when the user clicks Ask again on a timed-out card. The caller
  // submits askAgainPrompt(approval) as a user turn so the agent re-stages.
  onAskAgain?: (approval: Approval) => void;
  // enable_mcp_connectors only: sync the conversation's enabled optional MCP
  // connectors into the Tools picker when the user approves the card.
  onMcpServersUpdated?: (enabledOptional: string[]) => void;
}) {
  const [submitting, setSubmitting] = useState<"send" | "cancel" | null>(null);
  // Why the last resolve attempt did not reach the server, shown inline under
  // the buttons; cleared on the next attempt. See ApprovalSubmitError.
  // A grouped decision that could not decide this card hands its reason over
  // (ApprovalGroupCard), so the card says why it is still pending.
  const [submitError, setSubmitError] = useState<string | null>(approval.decisionError ?? null);
  // Held locally so a test (or a parent that has not yet patched) still
  // shows the executing chrome after onResolved; the transcript persists
  // it on Approval.executing.
  const [heldExecuting, setHeldExecuting] = useState(false);
  const executing = approval.status === "pending" && (approvalIsExecuting(approval) || heldExecuting);
  const card: Approval = executing ? { ...approval, executing: true } : approval;
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
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);
  // A running call of a tool in critical_tool_progress: poll the card for
  // its progress and outcome (docs/APPROVAL-PROGRESS.md). Called before the
  // per-kind early returns to satisfy the rules of hooks.
  // A stoppable call polls too, so the card settles on its own after a Stop
  // (docs/APPROVAL-STOP.md).
  useApprovalProgressPoll(card, conversationId, executing && (approval.progressUpdates === true || approval.stoppable === true), (next) => {
    if (next.status !== "pending") setHeldExecuting(false);
    onResolved(next);
  });

  const resolve = async (
    approved: boolean,
    edits?: { name?: string; prompt?: string; cron?: string },
    // A narrowed approval (docs/APPROVAL-NARROWING.md): the ticked values of
    // the tool's narrowable list argument. Absent approves the call whole.
    approvedValues?: string[],
  ) => {
    if (submitting || approval.status !== "pending" || !conversationId) return;
    if (executing && !approved) return;
    setSubmitting(approved ? "send" : "cancel");
    setSubmitError(null);
    // The argument the card showed goes with the values: the server refuses a
    // narrowing of any other argument (a policy that changed after a restart).
    const narrowedArg = approval.narrowable?.arg;
    const narrowing =
      approved && !executing && approvedValues !== undefined && approvedValues.length > 0 && narrowedArg !== undefined;
    try {
      const approvalUrl = conversationApprovalApiUrl(conversationId, approval.id);
      if (!approvalUrl) {
        // Both ids are server-minted UUIDs; anything else is not sent, so
        // no id can steer this POST to another path or origin.
        setSubmitError("This approval has an invalid id and can't be sent.");
        return;
      }
      const response = await fetch(approvalUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          approved,
          // A no-session tool never sends a wider scope, whatever state the
          // (hidden) checkbox is in: the server would refuse it with a 400.
          // A narrowed approval covers this call only (the server refuses a
          // wider scope with approved_values).
          scope: executing || approval.noSessionApproval || narrowing ? "once" : applyAll ? "session" : "once",
          ...(!executing && edits && Object.keys(edits).length > 0 ? { edits } : {}),
          ...(narrowing ? { approved_values: approvedValues, narrowed_arg: narrowedArg } : {}),
        }),
      });
      if (!response.ok) {
        // The server did not record a decision, so the approval is still
        // pending there — keep the card pending here too and let the user
        // retry, rather than stamping a terminal "failed" the server never
        // reported.
        setSubmitError(await describeSubmitFailure(response));
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        result_text?: string;
        is_err?: boolean;
        executing?: boolean;
        execution_unknown?: boolean;
        resume?: boolean;
        result_card?: unknown;
        narrowed?: unknown;
        stoppable?: boolean;
        stopped?: boolean;
        enabled_optional?: string[];
      };
      const nextStatus = approvalStatusFromOutcome(data);
      const narrowed = parseApprovalNarrowed(data.narrowed) ?? approval.narrowed;
      if (Array.isArray(data.enabled_optional)) {
        onMcpServersUpdated?.(data.enabled_optional);
      }
      if (data.resume === true) onResumeExpected?.(nextStatus === null);
      if (nextStatus === null) {
        setHeldExecuting(true);
        onResolved({
          ...approval,
          status: "pending",
          executing: true,
          resultText: data.result_text,
          decisionError: undefined,
          narrowed,
          stoppable: data.stoppable === true || approval.stoppable,
        });
        return;
      }
      setHeldExecuting(false);
      onResolved({
        ...approval,
        status: nextStatus,
        resultText: data.result_text,
        resultCard: resultCardFromOutcome(data),
        executing: false,
        decisionError: undefined,
        narrowed,
        stopped: data.stopped === true || undefined,
      });
    } catch (err) {
      // Network failure: nothing reached the server, same posture as a non-2xx.
      setSubmitError(
        `Couldn't reach the server (${err instanceof Error ? err.message : "request failed"}).`,
      );
    } finally {
      setSubmitting(null);
    }
  };

  // Stop a running call (docs/APPROVAL-STOP.md): the server asks the tool to
  // stop after its current item; the card shows "Stop requested" until it settles.
  const stop = async () => {
    if (submitting || !executing || !conversationId) return;
    setSubmitting("cancel");
    setSubmitError(null);
    try {
      const url = conversationApiPath(conversationId, "approvals", approval.id, "stop");
      if (!url) {
        setSubmitError("This approval has an invalid id and can't be sent.");
        return;
      }
      const response = await fetch(url, { method: "POST" });
      if (!response.ok) {
        setSubmitError(await describeSubmitFailure(response));
        return;
      }
      const data = (await response.json()) as ApprovalOutcomePayload;
      const nextStatus = approvalStatusFromOutcome(data);
      if (nextStatus === null) {
        if (data.stopping === true) {
          onResolved({ ...approval, status: "pending", executing: true, stopping: true });
        } else {
          // The server refused the stop: the call finished first and its
          // outcome is being recorded without one. The card keeps running
          // until the poll (or Check result) shows that outcome.
          setSubmitError("The action finished before it could be stopped; its outcome is being recorded.");
        }
        return;
      }
      setHeldExecuting(false);
      onResolved({
        ...approval,
        status: nextStatus,
        resultText: data.result_text,
        resultCard: resultCardFromOutcome(data),
        executing: false,
        stopping: undefined,
        stopped: data.stopped === true || undefined,
      });
    } catch (err) {
      setSubmitError(`Couldn't reach the server (${err instanceof Error ? err.message : "request failed"}).`);
    } finally {
      setSubmitting(null);
    }
  };

  if (approval.tool === "bash") {
    return (
      <BashApprovalCard
        approval={card}
        submitting={submitting}
        submitError={submitError}
        onResolve={resolve}
        onAskAgain={onAskAgain}
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
        approval={card}
        submitting={submitting}
        submitError={submitError}
        onResolve={resolve}
        onAskAgain={onAskAgain}
      />
    );
  }

  if (approval.tool === "manage_tasks") {
    return (
      <ManageTasksCard
        approval={card}
        submitting={submitting}
        submitError={submitError}
        onResolve={resolve}
        onAskAgain={onAskAgain}
      />
    );
  }

  if (approval.tool === "enable_mcp_connectors") {
    return (
      <EnableMCPConnectorsCard
        approval={card}
        submitting={submitting}
        submitError={submitError}
        onResolve={resolve}
        onAskAgain={onAskAgain}
      />
    );
  }

  // Anything that is not an email tool gets the generic critical-action card:
  // honest verbs, the tool's own arguments, and — for a notify-mode record —
  // "ran without asking" instead of a fabricated approval story. Before this,
  // a pages deploy fell through to the email chrome below and resolved as
  // "Email sent ✓".
  if (!isEmailApprovalTool(approval.tool)) {
    return (
      <GenericActionCard
        approval={card}
        submitting={submitting}
        submitError={submitError}
        onResolve={(approved, approvedValues) => void resolve(approved, undefined, approvedValues)}
        onStop={() => void stop()}
        applyAll={applyAll}
        onApplyAllChange={setApplyAll}
        onAskAgain={onAskAgain}
        onRetryFailed={onRetryFailed}
      />
    );
  }

  // To, Cc and Bcc each get their own line: folding them into one "To:" list
  // hid who was only copied (or blind-copied) — exactly what a reviewer checks
  // before letting a client-facing email go.
  const toLine = toRecipientList(approval.summary.to);
  const ccLine = toRecipientList(approval.summary.cc);
  const bccLine = toRecipientList(approval.summary.bcc);
  // Both arrays go out with the email (execution replays the frozen args), so
  // both are listed: an inline file whose cid the body never references is
  // still sent, and would otherwise appear nowhere on the card.
  const attachments = [
    ...emailAttachmentNames(approval.summary.attachments),
    ...emailAttachmentNames(approval.summary.inline_attachments).map((a) => ({ ...a, inline: true })),
  ];
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
    executing
      ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
      : approval.status === "approved"
      ? isPreviewOnly
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : { borderColor: "var(--color-success-border)", color: "var(--color-success-text)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger-text)" }
          : approval.status === "execution_unknown"
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
          : isPreviewOnly
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
            : {
                borderColor: "var(--color-accent)",
                color: "var(--color-text-primary)",
                background:
                  "var(--color-card-pending-bg)",
              };

  // Lead the title with an uppercase intent tag so a skimmer's eye
  // distinguishes draft cards from action cards before reading the rest.
  // The pending verb-phrase is what every existing e2e test asserts on
  // (e.g. /Send this email\?/i) — substring matches still pass.
  const title = isPreviewOnly
    ? executing
      ? "Dismissing…"
      : approval.status === "pending"
      ? "DRAFT · Email preview (not sent)"
      : approval.status === "approved" || approval.status === "rejected"
        ? "Draft dismissed"
        : approval.status === "execution_unknown"
          ? "Preview outcome not recorded"
          : "Preview failed"
    : executing
      ? "Sending…"
      : approval.status === "pending"
      ? "ACTION REQUIRED · Send this email?"
      : approval.status === "approved"
        ? "Email sent ✓"
        : approval.status === "rejected"
          ? "Send cancelled"
          : approval.status === "execution_unknown"
            ? "Approved — outcome not recorded"
            : "Send failed";

  // Pending preview gets a dashed border to signal "draft / not real /
  // sketch." Pending send keeps the solid border (the default `border`
  // class) so the two cards are distinguishable at a glance even before
  // a user reads the title. Resolved states fall back to solid for both
  // — once a card is finalized the draft/action distinction no longer
  // matters and the success/danger color tells the story.
  const isPendingPreview = isPreviewOnly && approval.status === "pending" && !executing;
  const borderStyleClass = isPendingPreview ? " border-dashed" : "";
  // No tint override needed for pending send — the inline `background`
  // in statusStyle replaces the default. For all other states keep the
  // existing soft overlay.
  const bgClass = approval.status === "pending" && !isPreviewOnly && !executing
    ? ""
    : " bg-[var(--color-card-bg)]";

  return (
    <div
      data-approval-id={approval.id}
      className={`rounded-[var(--radius-lg)] border px-3 py-2.5 text-[0.8125rem] leading-[1.5]${borderStyleClass}${bgClass}`}
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
            className="text-[0.72rem] underline text-[var(--color-card-text-muted)] hover:text-[var(--color-text-primary)]"
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
        {toLine ? <div data-testid="email-to"><span className="text-[var(--color-card-text-muted)]">To: </span>{toLine}</div> : null}
        {ccLine ? <div data-testid="email-cc"><span className="text-[var(--color-card-text-muted)]">Cc: </span>{ccLine}</div> : null}
        {bccLine ? <div data-testid="email-bcc"><span className="text-[var(--color-card-text-muted)]">Bcc: </span>{bccLine}</div> : null}
        <div><span className="text-[var(--color-card-text-muted)]">Subject: </span>{subject}</div>
        {from ? <div><span className="text-[var(--color-card-text-muted)]">From: </span>{from}</div> : null}
      </div>

      {/* Attachments sit OUTSIDE the collapsible body so a collapsed send card
          still says which files go out with it — a report sent without its
          CSV (or with the wrong one) is the mistake this card exists to catch.
          Inline (cid:) files are listed too, tagged "inline". */}
      {attachments.length > 0 ? (
        <div
          data-testid="email-attachments"
          className="mt-2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-2.5 py-1.5 text-[0.78rem]"
        >
          <div className="mb-1 text-[var(--color-card-text-muted)]">
            📎 {attachments.length} attachment{attachments.length === 1 ? "" : "s"}
          </div>
          <ul className="grid gap-0.5">
            {attachments.map((a, i) => (
              <li key={`${a.path}-${i}`} className="break-all text-[var(--color-text-secondary)]" title={a.path}>
                <span className="font-medium text-[var(--color-text-primary)]">{a.name}</span>
                {"inline" in a ? (
                  <StatusChip tone="neutral" className="ml-1.5">
                    inline
                  </StatusChip>
                ) : null}
                {a.path !== a.name ? (
                  <span className="ml-1.5 text-[0.72rem] text-[var(--color-card-text-muted)]">{a.path}</span>
                ) : null}
              </li>
            ))}
          </ul>
        </div>
      ) : null}

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
        executing ? (
          <ApprovalExecutingControls
            progress={approval.progress}
            submitting={submitting}
            submitError={submitError}
            onCheck={() => void resolve(true)}
          />
        ) : isPreviewOnly ? (
          <div className="mt-3 flex items-center gap-2">
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null}
              onClick={() => void resolve(true)}
            >
              {submitting === "send" ? "Dismissing…" : "Dismiss"}
            </button>
            <ApprovalSubmitError message={submitError} />
          </div>
        ) : (
          <div className="mt-3 flex flex-col gap-2">
            <div className="flex items-center gap-2">
              <button
                type="button"
                className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
                disabled={submitting !== null || countdown.expired}
                onClick={() => void resolve(true)}
              >
                {submitting === "send" ? "Sending…" : applyAll ? "Send + allow all" : "Send"}
              </button>
              <button
                type="button"
                className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
                disabled={submitting !== null || countdown.expired}
                onClick={() => void resolve(false)}
              >
                {submitting === "cancel" ? "Cancelling…" : applyAll ? "Deny + block all" : "Cancel"}
              </button>
            </div>
            <ApprovalSeatBadge server={approval.mcpServer} account={approval.mcpAccount} />
            <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
            <ApprovalSubmitError message={submitError} />
            {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
            {/* Batch approval (#300): pre-approve/deny the rest of this tool's
                calls for the conversation so the agent isn't gated per call.
                Withheld when the bundle requires a decision per call. */}
            {approval.noSessionApproval ? null : (
              <label className="flex items-center gap-1.5 text-[0.72rem] text-[var(--color-card-text-muted)]">
                <input
                  type="checkbox"
                  data-testid="approval-apply-all"
                  checked={applyAll}
                  disabled={submitting !== null}
                  onChange={(e) => setApplyAll(e.target.checked)}
                  className="size-3.5 accent-[var(--color-primary)]"
                />
                <ApplyAllText approval={approval} />
              </label>
            )}
          </div>
        )
      ) : (
        <>
          {approval.resultText ? (
            isPreviewOnly || approval.status === "execution_unknown" ? (
              <ApprovalResult text={approval.resultText} />
            ) : (
              <EmailApprovalOutcome
                text={approval.resultText}
                failed={approval.status === "failed"}
              />
            )
          ) : null}
          {isTimedOutApproval(approval) ? (
            <AskAgainButton approval={approval} onAskAgain={onAskAgain} />
          ) : null}
        </>
      )}
    </div>
  );
}

// readableTitle is a readable card's header line: the describer's plain-words
// title while the card waits, and a one-line outcome once it has resolved —
// "Applied", "Stop requested", "Not applied", "Declined", "Timed out — not
// applied", "Outcome not recorded". The tool's own result still renders below,
// as on every card.
function readableTitle(
  approval: Approval,
  cardTitle: string,
  executing: boolean,
  recorded: boolean,
  timedOut: boolean,
  stopped: boolean,
): string {
  if (executing) return `Running · ${cardTitle}`;
  switch (approval.status) {
    case "pending":
      return cardTitle;
    case "approved":
      if (stopped) return `Stop requested · ${cardTitle}`;
      return recorded ? `${cardTitle} · ran without asking` : `Applied · ${cardTitle}`;
    case "rejected":
      return timedOut ? `Timed out — not applied · ${cardTitle}` : `Declined · ${cardTitle}`;
    case "execution_unknown":
      return `Outcome not recorded · ${cardTitle}`;
    default:
      return `Not applied · ${cardTitle}`;
  }
}

// GenericActionCard renders any critical tool fleet has no tailored card for —
// bundle-declared suffixes like a pages deploy or a deal write. Two jobs:
// say honestly what is about to happen (the humanized action, its server, its
// arguments verbatim), and resolve honestly (Approve & run / Cancel — never
// "Send", never "Email sent ✓"). A notify-mode record (#1153) renders on the
// same chrome in its informational form: the tool already ran, the title says
// so, and the result carries the bundle-authored undo hint.
function GenericActionCard({
  approval,
  submitting,
  submitError,
  onResolve,
  onStop,
  applyAll,
  onApplyAllChange,
  onAskAgain,
  onRetryFailed,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  // approvedValues narrows the approval (docs/APPROVAL-NARROWING.md).
  onResolve: (approved: boolean, approvedValues?: string[]) => void;
  // Stop a running call of a tool in critical_tool_stop (docs/APPROVAL-STOP.md).
  onStop?: () => void;
  applyAll: boolean;
  onApplyAllChange: (v: boolean) => void;
  onAskAgain?: (approval: Approval) => void;
  onRetryFailed?: (approval: Approval, keys: string[]) => void | Promise<void | boolean>;
}) {
  const [detailsOpen, setDetailsOpen] = useState(false);
  const detailsId = useId();
  const executing = approvalIsExecuting(approval);
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);
  // A narrowable card (critical_tool_narrowable_args): rows the person may
  // untick before approving. Inactive for every other card.
  const narrowing = useNarrowing(approval, submitting !== null || countdown.expired);
  const leftOut = leftOutOf(approval.narrowed);
  const narrowedLine = approval.narrowed ? (
    <p data-testid="approval-narrowed-summary" className="mt-0.5 min-w-0 text-[0.75rem] text-[var(--color-text-secondary)]">
      {narrowedSummary(approval.narrowed)}
    </p>
  ) : null;
  const names = useContext(ConnectorNamesContext);
  const { action, server } = actionLabel(approval.tool, approval.mcpServer, approval.mcpAccount, names);
  const args = Array.isArray(approval.summary.args) ? approval.summary.args : [];
  const rawArgs = approval.summary.raw ?? "";
  const recorded = approval.recorded === true;
  const timedOut = isTimedOutApproval(approval);
  // A call the person stopped while it ran (critical_tool_stop,
  // docs/APPROVAL-STOP.md) that settled without an error: only part of it may
  // have run, so it never reads as applied or completed. Whether the server
  // honoured the stop is the result's to say, so the card says a stop was
  // requested, not that the call stopped.
  const stopped = approval.stopped === true && approval.status === "approved" && !executing;
  // A bundle-declared describer's readable card (docs/APPROVAL-CARD-DESCRIBERS.md):
  // plain-words title, the records, before → after, flags. Absent = the
  // generic layout below, unchanged.
  const readable = approval.card;

  const title = readable
    ? readableTitle(approval, readable.title, executing, recorded, timedOut, stopped)
    : executing
      ? `${action} · running`
      : approval.status === "pending"
      ? `ACTION REQUIRED · Run "${action}"?`
      : approval.status === "approved"
        ? stopped
          ? `${action} · stop requested`
          : recorded
          ? `${action} · ran without asking`
          : `${action} · completed ✓`
        : approval.status === "rejected"
          ? timedOut
            ? `${action} · timed out`
            : `${action} · cancelled`
          : approval.status === "execution_unknown"
            ? `${action} · outcome not recorded`
            : `${action} · failed`;

  // A record is information, not a past decision — muted chrome, like the
  // dismissed-preview state, so it never masquerades as an approval.
  const statusStyle: React.CSSProperties =
    executing
      ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
      : stopped
      ? { borderColor: "var(--color-warning-border)", color: "var(--color-text-primary)" }
      : approval.status === "approved"
      ? recorded
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
        : { borderColor: "var(--color-success-border)", color: "var(--color-success-text)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger-text)" }
          : approval.status === "execution_unknown"
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  // The raw identity and arguments: the whole body of the generic card, and
  // the collapsed "Details" of a readable one.
  const toolHeader = server ? (
    <div className="mb-1 text-[0.72rem] text-[var(--color-card-text-muted)]">
      via <span className="font-mono">{server}</span>
      <span className="mx-1">·</span>
      <span className="font-mono">{approval.tool}</span>
    </div>
  ) : null;
  const argsBlock =
    args.length > 0 ? (
      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {args.map((row) => (
          <div key={row.key} className="min-w-0">
            <span className="text-[var(--color-card-text-muted)]">{row.key}: </span>
            <span className="break-all">{row.value}</span>
          </div>
        ))}
      </div>
    ) : rawArgs ? (
      <pre
        className="max-h-40 min-w-0 max-w-full overflow-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-surface-inset)] p-2 text-[0.75rem] text-[var(--color-text-primary)]"
        style={{ fontFamily: "var(--font-code)" }}
      >
        {rawArgs}
      </pre>
    ) : null;

  // A settled success whose result carried an outcome card (a tool in
  // critical_tool_result_cards, docs/APPROVAL-RESULT-CARDS.md): the completed
  // card reads it. It keeps the approved card's title in view and its
  // records, the raw call and the raw result under Details. Any other
  // settled card renders below, as before.
  if (approval.resultCard && approval.status === "approved" && !executing) {
    return (
      <ApprovalResultCardView
        approval={approval}
        result={approval.resultCard}
        recorded={recorded}
        approvedTitle={readable?.title}
        approvedNote={approval.narrowed ? narrowedSummary(approval.narrowed) : undefined}
        approvedRecords={readable && readable.items.length > 0 ? <ApprovalCardItems items={readable.items} leftOut={leftOut} /> : null}
        toolDetails={
          <div className="mt-1 min-w-0">
            {toolHeader}
            {argsBlock}
          </div>
        }
        onRetryFailed={onRetryFailed}
      />
    );
  }

  // A pending card with a describer's readable card, waiting for a decision:
  // the designer's layout (APPROVAL NEEDED eyebrow and countdown, the record
  // in its inner box, changes right-aligned, Approve / Cancel with Details on
  // one row). The controls and their rules are the generic card's.
  if (readable && approval.status === "pending" && !executing) {
    return (
      <div
        data-approval-id={approval.id}
        data-tool={approval.tool}
        data-testid="generic-action-card"
        data-layout="readable"
        className="rounded-[var(--radius-lg)] border px-4 py-3 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)]"
        style={{
          borderColor: "color-mix(in srgb, var(--color-accent) 55%, transparent)",
          background: "var(--color-card-pending-bg)",
        }}
      >
        <div className="flex min-w-0 flex-wrap items-center justify-between gap-x-3 gap-y-1">
          <span
            data-testid="approval-eyebrow"
            className="text-[0.68rem] font-medium uppercase tracking-[0.18em] text-[var(--color-card-link)]"
            style={{ fontFamily: "var(--font-mono)" }}
          >
            Approval needed
          </span>
          <EyebrowCountdown remaining={countdown.remaining} expired={countdown.expired} />
        </div>
        <div data-testid="approval-card-readable" className="min-w-0">
          <div data-testid="generic-action-title" className="mt-1.5 min-w-0 break-words [overflow-wrap:anywhere] text-[0.95rem] font-semibold">
            {readable.title}
          </div>
          {readable.subtitle ? (
            <p className="mt-0.5 min-w-0 break-words text-[0.8rem] text-[var(--color-text-secondary)]">{readable.subtitle}</p>
          ) : null}
          {narrowing.active && narrowing.approved ? (
            <p
              data-testid="approval-narrow-count"
              aria-live="polite"
              className="mt-2 text-[0.72rem] font-medium text-[var(--color-text-secondary)]"
            >
              {selectedCountText(narrowing.approved.length, narrowing.total)}
            </p>
          ) : null}
          <ReadablePendingRecords items={readable.items} selection={narrowing.selection} />
          {readable.footer ? (
            <p className="mt-2 min-w-0 break-words text-[0.72rem] text-[var(--color-card-text-muted)]">{readable.footer}</p>
          ) : null}
        </div>
        <div className="mt-3 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-2 border-t border-[var(--color-border-subtle)] pt-3">
          <button
            type="button"
            data-testid="approval-approve"
            className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-4 py-1.5 text-[0.8rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
            disabled={submitting !== null || countdown.expired}
            onClick={() => {
              // Narrowed: name the ticked values. Nothing ticked is a decline.
              if (narrowing.approved && narrowing.approved.length === 0) onResolve(false);
              else if (narrowing.approved && narrowing.approved.length < narrowing.total) onResolve(true, narrowing.approved);
              else onResolve(true);
            }}
          >
            {submitting === "send"
              ? "Running…"
              : narrowing.approved && narrowing.approved.length === 0
                ? "Decline"
                : narrowing.approved
                  ? narrowedApproveLabel(narrowing.approved.length, narrowing.total)
                  : applyAll
                    ? "Approve + allow all"
                    : "Approve"}
          </button>
          <button
            type="button"
            className="rounded-[var(--radius-md)] px-3 py-1.5 text-[0.8rem] font-medium text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={submitting !== null || countdown.expired}
            onClick={() => onResolve(false)}
          >
            {submitting === "cancel" ? "Cancelling…" : applyAll ? "Deny + block all" : "Cancel"}
          </button>
          <DetailsToggle open={detailsOpen} onToggle={() => setDetailsOpen((v) => !v)} controls={detailsId} />
        </div>
        <DetailsPanel id={detailsId} open={detailsOpen}>
          {toolHeader}
          {argsBlock}
        </DetailsPanel>
        <div className="mt-2 flex flex-col gap-2 empty:hidden">
          {narrowing.approved && narrowing.approved.length < narrowing.total ? (
            <p data-testid="approval-narrow-scope" className="text-[0.72rem] text-[var(--color-card-text-muted)]">
              {narrowing.approved.length === 0
                ? "Nothing is selected, so this declines the action."
                : `Covers only the ${narrowing.approved.length} selected; ${narrowing.total - narrowing.approved.length} left out of the call.`}
            </p>
          ) : null}
          <ApprovalSeatBadge server={approval.mcpServer} account={approval.mcpAccount} verb="Runs as" />
          <ApprovalSubmitError message={submitError} />
          {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
          {approval.noSessionApproval || narrowing.active ? null : (
            <label className="flex items-center gap-1.5 text-[0.72rem] text-[var(--color-card-text-muted)]">
              <input
                type="checkbox"
                data-testid="approval-apply-all"
                checked={applyAll}
                disabled={submitting !== null}
                onChange={(e) => onApplyAllChange(e.target.checked)}
                className="size-3.5 accent-[var(--color-primary)]"
              />
              <ApplyAllText approval={approval} />
            </label>
          )}
        </div>
      </div>
    );
  }

  return (
    <div
      data-approval-id={approval.id}
      data-tool={approval.tool}
      data-testid="generic-action-card"
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex min-w-0 items-center gap-2">
        <span aria-hidden>{recorded ? "📓" : "🛠️"}</span>
        <span data-testid="generic-action-title" className="min-w-0 break-words font-medium">{title}</span>
      </div>
      {/* A narrowed approval says so on every settled or running card, with
          or without a readable card (docs/APPROVAL-NARROWING.md). */}
      {narrowedLine}
      {stopped ? (
        <p data-testid="approval-stopped" className="mt-0.5 min-w-0 text-[0.75rem] text-[var(--color-warning-text)]">
          You pressed Stop while this ran, so it may have run only in part. The result says what ran.
        </p>
      ) : null}

      {readable ? (
        <>
          {/* Pending: the records in plain words. Resolved: the one-line
              outcome above is the card; the records move into Details. */}
          {approval.status === "pending" ? <ApprovalReadableBody card={readable} leftOut={leftOut} /> : null}
          <ApprovalCardDetails>
            {approval.status !== "pending" ? <ApprovalCardItems items={readable.items} leftOut={leftOut} /> : null}
            {toolHeader}
            {argsBlock}
          </ApprovalCardDetails>
        </>
      ) : (
        <>
          {toolHeader}
          {argsBlock}
        </>
      )}

      {approval.status === "pending" ? (
        executing ? (
          <ApprovalExecutingControls
            progress={approval.progress}
            submitting={submitting}
            submitError={submitError}
            onCheck={() => onResolve(true)}
            onStop={approval.stoppable ? onStop : undefined}
            stopping={approval.stopping === true}
          />
        ) : (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true)}
            >
              {submitting === "send" ? "Running…" : applyAll ? "Approve + allow all" : "Approve & run"}
            </button>
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : applyAll ? "Deny + block all" : "Cancel"}
            </button>
          </div>
          <ApprovalSeatBadge server={approval.mcpServer} account={approval.mcpAccount} verb="Runs as" />
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
          <ApprovalSubmitError message={submitError} />
          {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
          {/* Batch approval (#300), same contract as the email card —
              withheld when the bundle requires a decision per call. */}
          {approval.noSessionApproval ? null : (
            <label className="flex items-center gap-1.5 text-[0.72rem] text-[var(--color-card-text-muted)]">
              <input
                type="checkbox"
                data-testid="approval-apply-all"
                checked={applyAll}
                disabled={submitting !== null}
                onChange={(e) => onApplyAllChange(e.target.checked)}
                className="size-3.5 accent-[var(--color-primary)]"
              />
              <ApplyAllText approval={approval} />
            </label>
          )}
        </div>
        )
      ) : (
        <>
          {approval.resultText ? <ApprovalResult text={approval.resultText} /> : null}
          {timedOut ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
        </>
      )}
    </div>
  );
}

function BashApprovalCard({
  approval,
  submitting,
  submitError,
  onResolve,
  onAskAgain,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  onResolve: (approved: boolean) => void;
  onAskAgain?: (approval: Approval) => void;
}) {
  const command = approval.summary.command ?? approval.summary.preview ?? "";
  const workingDir = approval.summary.working_dir ?? "";
  const timeoutSec = approval.summary.timeout_seconds ?? 0;
  const executing = approvalIsExecuting(approval);
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);

  const statusStyle: React.CSSProperties =
    executing
      ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
      : approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success-text)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger-text)" }
          : approval.status === "execution_unknown"
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  return (
    <div
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>⚡</span>
        <span className="font-medium">
          {executing
            ? "Command running"
            : approval.status === "pending"
            ? "Run this shell command?"
            : approval.status === "approved"
              ? "Command executed"
              : approval.status === "rejected"
                ? "Command declined"
                : approval.status === "execution_unknown"
                  ? "Command outcome not recorded"
                  : "Command failed"}
        </span>
      </div>

      <pre className="overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-[var(--color-surface-inset)] p-2 font-mono text-[0.78rem] text-[var(--color-text-primary)]">
        {command}
      </pre>

      {(workingDir || timeoutSec) ? (
        <div className="mt-1 grid gap-0.5 text-[0.72rem] text-[var(--color-card-text-muted)]">
          {workingDir ? <div>cwd: {workingDir}</div> : null}
          {timeoutSec ? <div>timeout: {timeoutSec}s</div> : null}
        </div>
      ) : null}

      {approval.status === "pending" ? (
        executing ? (
          <ApprovalExecutingControls
            progress={approval.progress}
            submitting={submitting}
            submitError={submitError}
            onCheck={() => onResolve(true)}
          />
        ) : (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex items-center gap-2">
            <button
              type="button"
              className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true)}
            >
              {submitting === "send" ? "Running…" : "Approve & run"}
            </button>
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : "Cancel"}
            </button>
          </div>
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
          <ApprovalSubmitError message={submitError} />
          {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
        </div>
        )
      ) : (
        <>
          {approval.resultText ? <ApprovalResult text={approval.resultText} /> : null}
          {isTimedOutApproval(approval) ? (
            <AskAgainButton approval={approval} onAskAgain={onAskAgain} />
          ) : null}
        </>
      )}
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
  submitError,
  onResolve,
  onAskAgain,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  onResolve: (approved: boolean, edits?: { name?: string; prompt?: string; cron?: string }) => void;
  onAskAgain?: (approval: Approval) => void;
}) {
  const executing = approvalIsExecuting(approval);
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);
  const s = approval.summary;
  const name = (s.name ?? "").trim();
  const promptPreview = s.prompt_preview ?? "";
  // Inline edit-before-approve: the proposal is a starting point, not a
  // take-it-or-leave-it. Fields start from the STAGED values (summary.prompt is
  // the full prompt — the preview is truncated); on approve, only fields that
  // actually differ are sent as edits, and the server re-validates them with
  // the same checks the staged args passed.
  const [editing, setEditing] = useState(false);
  const [editName, setEditName] = useState(name);
  const [editCron, setEditCron] = useState((s.cron ?? "").trim());
  const [editPrompt, setEditPrompt] = useState((s.prompt ?? promptPreview).trim());

  const collectEdits = (): { name?: string; prompt?: string; cron?: string } | undefined => {
    if (!editing) return undefined;
    const edits: { name?: string; prompt?: string; cron?: string } = {};
    if (editName.trim() !== name) edits.name = editName.trim();
    const stagedPrompt = (s.prompt ?? promptPreview).trim();
    if (editPrompt.trim() !== stagedPrompt) edits.prompt = editPrompt.trim();
    if (s.recurring && editCron.trim() !== (s.cron ?? "").trim()) edits.cron = editCron.trim();
    return Object.keys(edits).length > 0 ? edits : undefined;
  };

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
  // Connectors the task inherits from this chat (ADR-0068). A scheduled run
  // binds only its saved connectors plus the bundle's always-on ones, so a
  // task with neither runs without email, mailbox or data feeds — say so
  // before Approve, not in a failed run's log.
  const connectors = Array.isArray(s.connectors) ? s.connectors : [];
  const alwaysOn = Array.isArray(s.always_on_connectors) ? s.always_on_connectors : [];
  const connectorsLine =
    connectors.length > 0
      ? connectors.join(", ")
      : alwaysOn.length > 0
        ? `always-on only (${alwaysOn.join(", ")})`
        : null;
  const noConnectors = s.no_connectors === true || (connectors.length === 0 && alwaysOn.length === 0);

  const statusStyle: React.CSSProperties =
    executing
      ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
      : approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success-text)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger-text)" }
          : approval.status === "execution_unknown"
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const title =
    executing
      ? "Scheduling…"
      : approval.status === "pending"
      ? "ACTION REQUIRED · Schedule this task?"
      : approval.status === "approved"
        ? "Task scheduled ✓"
        : approval.status === "rejected"
          ? "Scheduling cancelled"
          : approval.status === "execution_unknown"
            ? "Approved — outcome not recorded"
            : "Scheduling failed";

  return (
    <div
      data-approval-id={approval.id}
      data-tool="schedule_task"
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>🗓️</span>
        <span className="font-medium">{title}</span>
      </div>

      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {name ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Name: </span>
            {name}
          </div>
        ) : null}
        <div>
          <span className="text-[var(--color-card-text-muted)]">Schedule: </span>
          {scheduleLine}
        </div>
        {frequencyLine ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Frequency: </span>
            {frequencyLine}
          </div>
        ) : null}
        {s.model ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Model: </span>
            {s.model}
          </div>
        ) : null}
        {s.allow_network ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Network: </span>
            egress allowed
          </div>
        ) : null}
        {connectorsLine ? (
          <div data-testid="schedule-connectors">
            <span className="text-[var(--color-card-text-muted)]">Connectors: </span>
            {connectorsLine}
          </div>
        ) : null}
      </div>

      {noConnectors ? (
        <div
          data-testid="schedule-no-connectors"
          role="note"
          className="mt-2 rounded-md border border-[var(--color-warning-border)] px-2 py-1.5 text-[0.75rem] text-[var(--color-warning-text)]"
        >
          <span aria-hidden>⚠️ </span>
          <span className="font-medium">No connectors.</span> This task will run without any email,
          mailbox or data connectors, so it cannot send or fetch anything. Turn the connectors it
          needs on in this chat&apos;s Tools menu and schedule again, or add them to the task in the
          Operations Center after approving.
        </div>
      ) : null}

      {editing && approval.status === "pending" && !executing ? (
        <div className="mt-2 grid gap-2">
          <label className="grid gap-1 text-[0.72rem] text-[var(--color-card-text-muted)]">
            Name
            <input
              value={editName}
              onChange={(e) => setEditName(e.target.value)}
              className="rounded-md border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>
          {s.recurring ? (
            <label className="grid gap-1 text-[0.72rem] text-[var(--color-card-text-muted)]">
              Cron schedule
              <input
                value={editCron}
                onChange={(e) => setEditCron(e.target.value)}
                className="rounded-md border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
              />
            </label>
          ) : null}
          <label className="grid gap-1 text-[0.72rem] text-[var(--color-card-text-muted)]">
            Prompt
            <textarea
              value={editPrompt}
              onChange={(e) => setEditPrompt(e.target.value)}
              rows={6}
              className="rounded-md border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>
        </div>
      ) : promptPreview ? (
        <pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-[var(--color-surface-inset)] p-2 text-[0.75rem] text-[var(--color-text-primary)]">
          {promptPreview}
        </pre>
      ) : null}

      {approval.status === "pending" ? (
        executing ? (
          <ApprovalExecutingControls
            progress={approval.progress}
            submitting={submitting}
            submitError={submitError}
            onCheck={() => onResolve(true)}
          />
        ) : (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true, collectEdits())}
            >
              {submitting === "send"
                ? "Scheduling…"
                : editing && collectEdits()
                  ? "Approve with changes"
                  : "Approve & schedule"}
            </button>
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => {
                setEditing((was) => {
                  if (was) {
                    // Discard: reset fields to the staged values.
                    setEditName(name);
                    setEditCron((s.cron ?? "").trim());
                    setEditPrompt((s.prompt ?? promptPreview).trim());
                  }
                  return !was;
                });
              }}
            >
              {editing ? "Discard edits" : "Edit…"}
            </button>
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : "Cancel"}
            </button>
          </div>
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
          <ApprovalSubmitError message={submitError} />
          {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
        </div>
        )
      ) : (
        <>
          {approval.resultText ? <ApprovalResult text={approval.resultText} /> : null}
          {isTimedOutApproval(approval) ? (
            <AskAgainButton approval={approval} onAskAgain={onAskAgain} />
          ) : null}
        </>
      )}
    </div>
  );
}

// ManageTasksCard renders the approval card for a manage_tasks call (#1152):
// changing or stopping tasks that already exist. Two things make this card
// different from the schedule_task one, and both come from the same place —
// this call acts on work the user already has running.
//
// It renders the SELECTOR rather than a resolved list. Resolving at staging time
// would produce a snapshot that could disagree with the executor's own
// resolution by the time the human clicks; the boundaries live where they are
// true instead (the tool refuses a filtered stop outright, the server caps an
// update's blast radius and refuses past it, and the post-approval result names
// every task it touched).
//
// And "stop" reads as destructive, because it is: a recurring job stops
// recurring, and there is no undo from chat.
function ManageTasksCard({
  approval,
  submitting,
  submitError,
  onResolve,
  onAskAgain,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  onResolve: (approved: boolean) => void;
  onAskAgain?: (approval: Approval) => void;
}) {
  const executing = approvalIsExecuting(approval);
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);
  const s = approval.summary as {
    action?: string;
    task_ids?: string[];
    match?: string[];
    changes?: string[];
    destructive?: boolean;
    consequence?: string;
    max_tasks?: number;
  };
  const stopping = s.action === "stop";
  const ids = Array.isArray(s.task_ids) ? s.task_ids : [];
  const match = Array.isArray(s.match) ? s.match : [];
  const changes = Array.isArray(s.changes) ? s.changes : [];

  const title =
    executing
      ? stopping
        ? "Stopping tasks…"
        : "Updating tasks…"
      : approval.status === "pending"
      ? stopping
        ? "Stop scheduled tasks?"
        : "Change scheduled tasks?"
      : approval.status === "approved"
        ? stopping
          ? "Tasks stopped"
          : "Tasks updated"
        : approval.status === "rejected"
          ? "Change cancelled"
          : approval.status === "execution_unknown"
            ? "Approved — outcome not recorded"
            : "Change failed";

  const statusStyle =
    executing
      ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
      : approval.status === "pending"
      ? {
          borderColor: stopping ? "var(--color-danger, #b3261e)" : "var(--color-accent)",
        }
      : undefined;

  return (
    <div
      data-approval-id={approval.id}
      data-tool="manage_tasks"
      data-destructive={stopping ? "true" : undefined}
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <span aria-hidden>{stopping ? "🛑" : "✏️"}</span>
        <span className="font-medium">{title}</span>
      </div>

      <div className="grid gap-0.5 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {ids.length > 0 ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">
              {ids.length === 1 ? "Task: " : `Tasks (${ids.length}): `}
            </span>
            <code className="text-[0.72rem]">{ids.join(", ")}</code>
          </div>
        ) : null}
        {match.length > 0 ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Applies to every task where: </span>
            {match.join("; ")}
            {s.max_tasks ? (
              <span className="text-[var(--color-card-text-muted)]">
                {" "}
                (refused above {s.max_tasks} matches)
              </span>
            ) : null}
          </div>
        ) : null}
      </div>

      {changes.length > 0 ? (
        <ul className="mt-2 grid gap-0.5 rounded-md bg-[var(--color-surface-inset)] p-2 text-[0.75rem] text-[var(--color-text-primary)]">
          {changes.map((line) => (
            <li key={line}>{line}</li>
          ))}
        </ul>
      ) : null}

      {s.consequence ? (
        <p className="mt-2 text-[0.75rem] text-[var(--color-text-secondary)]">{s.consequence}</p>
      ) : null}

      {approval.status === "pending" ? (
        executing ? (
          <ApprovalExecutingControls
            progress={approval.progress}
            submitting={submitting}
            submitError={submitError}
            onCheck={() => onResolve(true)}
          />
        ) : (
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(true)}
            >
              {submitting === "send"
                ? stopping
                  ? "Stopping…"
                  : "Updating…"
                : stopping
                  ? "Approve & stop"
                  : "Approve & update"}
            </button>
            <button
              type="button"
              className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
              disabled={submitting !== null || countdown.expired}
              onClick={() => onResolve(false)}
            >
              {submitting === "cancel" ? "Cancelling…" : "Cancel"}
            </button>
          </div>
          <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
          <ApprovalSubmitError message={submitError} />
          {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
        </div>
        )
      ) : (
        <>
          {approval.resultText ? <ApprovalResult text={approval.resultText} /> : null}
          {isTimedOutApproval(approval) ? (
            <AskAgainButton approval={approval} onAskAgain={onAskAgain} />
          ) : null}
        </>
      )}
    </div>
  );
}

function EnableMCPConnectorsCard({
  approval,
  submitting,
  submitError,
  onResolve,
  onAskAgain,
}: {
  approval: Approval;
  submitting: "send" | "cancel" | null;
  submitError: string | null;
  onResolve: (approved: boolean) => void;
  onAskAgain?: (approval: Approval) => void;
}) {
  const executing = approval.status === "pending" && approvalIsExecuting(approval);
  const countdown = useApprovalCountdown(approval.expiresAt, approval.status, executing);
  const s = approval.summary as Record<string, unknown>;
  const labels = Array.isArray(s.labels)
    ? (s.labels as unknown[]).filter((x): x is string => typeof x === "string")
    : Array.isArray(s.connectors)
      ? (s.connectors as unknown[]).filter((x): x is string => typeof x === "string")
      : [];
  const reason = typeof s.reason === "string" ? s.reason.trim() : "";

  const title = executing
    ? "Enabling connector(s)…"
    : approval.status === "pending"
      ? `ACTION REQUIRED · Enable ${labels.length === 1 ? "MCP connector" : "MCP connectors"}?`
      : approval.status === "approved"
        ? approval.recorded
          ? "Auto-enabled MCP connector(s) ✓"
          : "MCP connector(s) enabled ✓"
        : approval.status === "rejected"
          ? "Connector enablement declined"
          : "Connector enablement failed";

  const statusStyle = executing
    ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
    : approval.status === "pending"
      ? { borderColor: "var(--color-accent)" }
      : undefined;

  return (
    <div
      data-approval-id={approval.id}
      data-tool="enable_mcp_connectors"
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
      style={statusStyle}
    >
      <div className="mb-2 flex items-center gap-2">
        <Icon name="tools" className="h-3.5 w-3.5 shrink-0 text-[var(--color-text-secondary)]" />
        <span className="font-medium">{title}</span>
      </div>

      <div className="grid gap-1 break-words text-[0.78rem] text-[var(--color-text-secondary)]">
        {labels.length > 0 ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">
              {labels.length === 1 ? "Connector: " : `Connectors (${labels.length}): `}
            </span>
            <span className="font-medium text-[var(--color-text-primary)]">
              {labels.join(", ")}
            </span>
          </div>
        ) : null}
        {reason ? (
          <div>
            <span className="text-[var(--color-card-text-muted)]">Why: </span>
            {reason}
          </div>
        ) : null}
      </div>

      {approval.status === "pending" ? (
        executing ? (
          <div className="mt-2.5 flex items-center gap-2 text-[0.75rem] text-[var(--color-text-secondary)]">
            <span>Enabling connector(s) and continuing turn…</span>
          </div>
        ) : (
          <div className="mt-2.5 flex flex-col gap-2">
            <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                disabled={submitting !== null || countdown.expired}
                onClick={() => onResolve(true)}
                className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
              >
                {submitting === "send" ? "Enabling…" : "Approve & continue"}
              </button>
              <button
                type="button"
                disabled={submitting !== null || countdown.expired}
                onClick={() => onResolve(false)}
                className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-transparent px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-bg-hover)] disabled:opacity-50"
              >
                {submitting === "cancel" ? "Declining…" : "Decline"}
              </button>
            </div>
            <ApprovalCountdown remaining={countdown.remaining} expired={countdown.expired} />
            {submitError ? <ApprovalSubmitError message={submitError} /> : null}
            {countdown.expired ? <AskAgainButton approval={approval} onAskAgain={onAskAgain} /> : null}
          </div>
        )
      ) : (
        <>
          {approval.resultText ? <ApprovalResult text={approval.resultText} /> : null}
          {isTimedOutApproval(approval) ? (
            <AskAgainButton approval={approval} onAskAgain={onAskAgain} />
          ) : null}
        </>
      )}
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
  // Receives the model the conversation was just pinned to: the retry must
  // send it explicitly, because the caller's selected-model state has not
  // re-rendered yet and would resend (and re-pin) the old model.
  onSwitchAndRetry?: (model: string) => void | Promise<void>;
}) {
  const [pending, setPending] = useState<SuggestAction | null>(null);
  // Same contract as ApprovalCard.resolve: a failed POST leaves the nudge
  // pending (the server never recorded a choice) and says why, inline.
  const [submitError, setSubmitError] = useState<string | null>(null);

  const reason = approval.summary.reason ?? "Advanced mode would handle this better.";
  const recommendedSlug = approval.summary.recommend_model ?? currentAdvancedModel();
  const recommendedLabel = labelForModel(recommendedSlug);

  const submit = async (action: SuggestAction) => {
    if (pending || approval.status !== "pending" || !conversationId) return;
    setPending(action);
    setSubmitError(null);
    try {
      const approvalUrl = conversationApprovalApiUrl(conversationId, approval.id);
      if (!approvalUrl) {
        // Both ids are server-minted UUIDs; anything else is not sent, so
        // no id can steer this POST to another path or origin.
        setSubmitError("This approval has an invalid id and can't be sent.");
        return;
      }
      const response = await fetch(approvalUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          approved: action !== "dismiss",
          action,
        }),
      });
      if (!response.ok) {
        setSubmitError(await describeSubmitFailure(response));
        return;
      }
      const data = (await response.json()) as {
        status: ApprovalStatus;
        action?: SuggestAction;
        model?: string;
        result_text?: string;
      };
      // Do not run this through approvalStatusFromOutcome: the winning pin
      // response omits is_err (the model flip IS the outcome). Treating a
      // missing boolean as execution_unknown would hide a successful switch.
      onResolved({
        ...approval,
        status: data.status,
        resultText: data.result_text,
        executing: false,
      });
      if (data.status === "approved" && data.model) {
        onModelSwitched?.(data.model);
        if (action === "switch_and_retry") {
          await onSwitchAndRetry?.(data.model);
        }
      }
    } catch (err) {
      setSubmitError(
        `Couldn't reach the server (${err instanceof Error ? err.message : "request failed"}).`,
      );
    } finally {
      setPending(null);
    }
  };

  const statusStyle: React.CSSProperties =
    approval.status === "approved"
      ? { borderColor: "var(--color-success-border)", color: "var(--color-success-text)" }
      : approval.status === "rejected"
        ? { borderColor: "var(--color-border-strong)", color: "var(--color-card-text-muted)" }
        : approval.status === "failed"
          ? { borderColor: "var(--color-danger-border)", color: "var(--color-danger-text)" }
          : approval.status === "execution_unknown"
            ? { borderColor: "var(--color-border-strong)", color: "var(--color-text-secondary)" }
          : { borderColor: "var(--color-accent)", color: "var(--color-text-primary)" };

  const title =
    approval.status === "pending"
      ? `Try ${recommendedLabel} for the rest of this chat?`
      : approval.status === "approved"
        ? `Switched to ${recommendedLabel}`
        : approval.status === "rejected"
          ? "Suggestion dismissed"
          : approval.status === "execution_unknown"
            ? "Approved — outcome not recorded"
            : "Suggestion failed";

  return (
    <div
      data-approval-id={approval.id}
      data-tool="suggest_advanced_model"
      className="rounded-[var(--radius-lg)] border bg-[var(--color-card-bg)] px-3 py-2.5 text-[0.8125rem] leading-[1.5]"
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
            className="rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_and_retry")}
          >
            {pending === "switch_and_retry" ? "Switching…" : "Switch & retry"}
          </button>
          <button
            type="button"
            className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("switch_only")}
          >
            {pending === "switch_only" ? "Switching…" : "Just switch"}
          </button>
          <button
            type="button"
            className="rounded-[var(--radius-md)] border border-transparent px-3 py-1.5 text-[0.75rem] text-[var(--color-card-text-muted)] transition hover:text-[var(--color-text-primary)] disabled:opacity-50"
            disabled={pending !== null}
            onClick={() => void submit("dismiss")}
          >
            {pending === "dismiss" ? "Dismissing…" : "Dismiss"}
          </button>
          <ApprovalSubmitError message={submitError} />
        </div>
      ) : approval.resultText ? (
        <ApprovalResult text={approval.resultText} />
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
  // The stage is chrome AROUND the iframe, not part of the email, so it tracks
  // the deployment palette. This was the literal #f4f6fb — fleet's own light page
  // color — which read as a foreign tint inside a bundle-themed shell.
  const stageBg = inbox === "dark" ? "#121212" : "var(--color-surface-2, #f4f6fb)";
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
        // Tuned map first (fleet's own palette), algorithmic fallback second so a
        // bundle-themed email inverts sensibly instead of not at all.
        const swapBG =
          DARK_BG_MAP[normRgb(cs.backgroundColor)] ?? darkenBackground(cs.backgroundColor);
        if (swapBG) el.style.setProperty("background-color", swapBG, "important");
        const swapText = DARK_TEXT_MAP[normRgb(cs.color)] ?? lightenText(cs.color);
        if (swapText) el.style.setProperty("color", swapText, "important");
        const swapBorder =
          DARK_BORDER_MAP[normRgb(cs.borderTopColor)] ?? darkenBorder(cs.borderTopColor);
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
          <p className="mb-1 text-[0.7rem] text-[var(--color-card-text-muted)]">{overflowNotice}</p>
        ) : null}
        <pre
          className="max-h-[16rem] min-w-0 max-w-full overflow-auto rounded-[0.6rem] border border-[var(--color-border)] bg-[var(--color-surface-inset)] px-2 py-1.5 text-[0.75rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
          style={{ fontFamily: "var(--font-code)" }}
        >
          {html}
        </pre>
      </div>
    );
  }

  return (
    <div className="mt-2 min-w-0 max-w-full overflow-hidden rounded-[0.6rem] border border-[var(--color-border)]">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-border)] bg-[var(--color-surface-inset)] px-2 py-1.5 text-[0.7rem]">
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
          className="underline text-[var(--color-card-text-muted)] hover:text-[var(--color-text-primary)]"
          onClick={() => onShowRawChange(!showRaw)}
        >
          {showRaw ? "Hide source" : "Show source"}
        </button>
      </div>

      {overflowNotice ? (
        <p className="border-b border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.7rem] text-[var(--color-card-text-muted)]">
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
          className="rounded-[0.4rem] border border-black/10 bg-white transition-[width] duration-base"
          style={{ width: `${viewportPx}px`, maxWidth: "100%", height: "28rem" }}
        />
      </div>

      {showRaw ? (
        <pre
          className="max-h-[14rem] min-w-0 max-w-full overflow-auto border-t border-[var(--color-border)] bg-[var(--color-surface-inset)] px-2 py-1.5 text-[0.72rem] leading-[1.45] text-[var(--color-text-secondary)] whitespace-pre-wrap break-all"
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
//
// These are an OVERRIDE LAYER, not the whole story. Every key is a hex from
// fleet's own canonical email palette, so an email themed by a client-config
// bundle (whose theme find-replaces those hexes for its own) matched almost
// nothing and the dark preview half-inverted — a few whites swapped, everything
// else stayed light, which reads as a broken preview rather than a dark inbox.
// darkenBackground / lightenText below are the palette-agnostic fallback for any
// color the maps don't name; the maps stay so fleet's own emails keep their
// hand-tuned appearance exactly.
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

// parseRgb pulls the channels out of whatever getComputedStyle returned. Returns
// null for `transparent` / rgba(...,0) so a see-through element is left alone
// rather than being painted an opaque dark box.
function parseRgb(c: string): [number, number, number] | null {
  const m = /^rgba?\((\d+),(\d+),(\d+)(?:,([\d.]+))?\)$/.exec(normRgb(c));
  if (!m) return null;
  if (m[4] !== undefined && Number(m[4]) === 0) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

// Relative luminance, same formula as the WCAG contrast maths.
function relLum([r, g, b]: [number, number, number]): number {
  const f = (v: number) => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(b);
}

// Hue-preserving lightness remap, done in HSL so a branded accent keeps its
// identity: a yellow rule stays yellow and a blue CTA stays blue, they just move
// to a lightness that works against an inverted surface. This is the same shape
// of transform a real dark-mode-forcing client applies, and deliberately an
// approximation — the preview says "roughly how this reads in a dark inbox", not
// "pixel-exact Gmail".
function remapLightness(
  [r, g, b]: [number, number, number],
  lo: number,
  hi: number,
  desaturate: number,
): string {
  const rn = r / 255;
  const gn = g / 255;
  const bn = b / 255;
  const max = Math.max(rn, gn, bn);
  const min = Math.min(rn, gn, bn);
  const l = (max + min) / 2;
  let h = 0;
  let s = 0;
  if (max !== min) {
    const d = max - min;
    s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
    if (max === rn) h = ((gn - bn) / d + (gn < bn ? 6 : 0)) / 6;
    else if (max === gn) h = ((bn - rn) / d + 2) / 6;
    else h = ((rn - gn) / d + 4) / 6;
  }
  // Invert lightness, then compress into the target band so the result is
  // always usable regardless of how extreme the input was.
  const target = lo + (1 - l) * (hi - lo);
  return `hsl(${Math.round(h * 360)} ${Math.round(s * desaturate * 100)}% ${Math.round(target * 100)}%)`;
}

// A light surface becomes a dark one; an already-dark surface is left alone (an
// email whose header is deliberately near-black should stay near-black).
function darkenBackground(c: string): string | null {
  const rgb = parseRgb(c);
  if (!rgb) return null;
  if (relLum(rgb) < 0.22) return null;
  return remapLightness(rgb, 0.1, 0.26, 0.55);
}

// Dark body text becomes light. Text that is already light (e.g. white on a
// branded header band) is left alone so it doesn't get flipped to unreadable.
function lightenText(c: string): string | null {
  const rgb = parseRgb(c);
  if (!rgb) return null;
  if (relLum(rgb) > 0.5) return null;
  return remapLightness(rgb, 0.72, 0.95, 0.5);
}

// Borders land mid-band: visible against a dark surface without becoming a line
// of hard contrast.
function darkenBorder(c: string): string | null {
  const rgb = parseRgb(c);
  if (!rgb) return null;
  if (relLum(rgb) < 0.15) return null;
  return remapLightness(rgb, 0.24, 0.38, 0.4);
}

function SegGroup({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="inline-flex items-center gap-1.5">
      <span className="text-[0.68rem] uppercase tracking-wide text-[var(--color-card-text-muted)]">{label}</span>
      <div role="group" aria-label={label} className="seg">
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
    <button type="button" aria-pressed={active} className="seg-option" onClick={onClick}>
      {children}
    </button>
  );
}

// emailAttachmentNames normalizes a staged send_email `attachments` argument
// for display. The bundles' senders accept bare paths or objects with a "path"
// key (or its inline-shape alias "file"; optionally a display "filename"/"name"),
// and models emit a single bare
// string too — the same shapes internal/agentcore's attachmentNames accepts.
// Anything unrecognizable is skipped rather than rendered as "[object Object]".
export function emailAttachmentNames(value: unknown): Array<{ name: string; path: string }> {
  const items = Array.isArray(value) ? value : value == null ? [] : [value];
  const out: Array<{ name: string; path: string }> = [];
  for (const item of items) {
    let path = "";
    let label = "";
    if (typeof item === "string") {
      path = item.trim();
    } else if (item && typeof item === "object") {
      const rec = item as Record<string, unknown>;
      // "file" is the inline shape's alias for "path" — the server's cid
      // expansion and the send fingerprint both accept it, so the card must.
      for (const k of ["path", "file"]) {
        if (typeof rec[k] === "string" && (rec[k] as string).trim()) {
          path = (rec[k] as string).trim();
          break;
        }
      }
      for (const k of ["filename", "name"]) {
        if (typeof rec[k] === "string" && (rec[k] as string).trim()) {
          label = (rec[k] as string).trim();
          break;
        }
      }
    }
    if (!path && !label) continue;
    const base = path.split(/[\\/]/).filter(Boolean).pop() ?? path;
    out.push({ name: label || base, path: path || label });
  }
  return out;
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

// supersedeNote maps the accept endpoint's supersede outcome to card copy.
// Guard outcomes are surfaced honestly — silently keeping two contradictory
// facts is the failure mode #515 exists to avoid.
function supersedeNote(outcome?: string): string | undefined {
  switch (outcome) {
    case "retired":
      return "Replaced the older fact (kept for audit, no longer used).";
    case "target_pinned":
      return "The older fact is pinned, so it was kept — unpin it to let this replace it.";
    case "target_changed":
      return "The older fact was edited since this was proposed, so it was kept.";
    case "target_missing":
      return "The older fact had already been deleted.";
    case "target_retired":
      return "The older fact was already retired.";
    default:
      return undefined;
  }
}

// MemoryProject is the project a chat belongs to, as the destination picker
// needs it: the id to POST to, the name to show, and whether the project is
// shared with a team (which decides the PRESELECTED destination, never a
// hidden one).
export type MemoryProject = { id: string; name: string; teamShared: boolean };

export type MemoryDestination = "personal" | "project";

const MEMORY_DESTINATIONS = [
  { value: "personal", label: "My memory" },
  { value: "project", label: "Team learnings" },
] as const;

// MemoryDestinationPicker is the visible "Save to:" choice (Item D1). It is
// deliberately a segmented control rather than a default with an override
// somewhere else: a member working inside a team-shared project usually means
// the team, and the one thing worse than the wrong default is not being able
// to see which one is about to happen.
//
// Shared by both capture paths — the approval card and the "Save" action under
// a reply — so members learn one model for both.
export function MemoryDestinationPicker({
  project,
  value,
  onChange,
  disabled,
}: {
  project: MemoryProject;
  value: MemoryDestination;
  onChange: (next: MemoryDestination) => void;
  disabled?: boolean;
}) {
  // Spans, not divs: this renders both as a block inside the approval card and
  // INLINE inside the message action row, and a <div> nested in that row's
  // <span> is invalid markup React will complain about at hydration.
  return (
    <span className="mb-2 flex flex-wrap items-center gap-1.5">
      <span className="text-[0.7rem] text-[var(--color-card-text-muted)]">Save to:</span>
      <Segmented
        label="Where to save this memory"
        value={value}
        options={MEMORY_DESTINATIONS}
        onChange={onChange}
        disabled={disabled}
      />
      <span className="text-[0.68rem] text-[var(--color-card-text-muted)]">
        {value === "project"
          ? `everyone in ${project.name}`
          : "only you, in every chat"}
      </span>
    </span>
  );
}

export function MemoryProposalCard({
  proposal,
  project,
  onResolved,
}: {
  proposal: MemoryProposal;
  // The chat's project, when it is in one. null = no destination choice; the
  // card behaves exactly as it always did.
  project?: MemoryProject | null;
  onResolved: (next: MemoryProposal) => void;
}) {
  const [submitting, setSubmitting] = useState<"save" | "dismiss" | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Inside a team-shared project, TEAM LEARNINGS is preselected — the
  // destination the context and the assistant's own framing imply
  // (docs/TEAM-SHARING.md, "Capturing one"). "Remember that this project is
  // for testing purposes" is a fact about the project; a personal default
  // filed it privately on one lazy click, and disagreed with the sentence the
  // user had just read. Outside a team-shared project (a personal project, or
  // none) there is no team to learn anything, so My memory it is.
  //
  // The picker is always shown, so whichever way it lands the user can flip it
  // before saving — the promise the doc makes is "never a hidden default", not
  // a particular one.
  //
  // Synced rather than read once at mount, and guarded by `touched`, for the
  // same reason SaveToMemoryAction is: the projects list can land after the
  // first render on a boot restore, and a late-arriving project must not stomp
  // a choice the user has already made. Render-time adjustment (React's
  // "adjusting state when a prop changes"), not an effect, so the picker never
  // paints once with a default computed from a project it hadn't loaded yet.
  const teamShared = Boolean(project?.teamShared);
  const [destination, setDestination] = useState<MemoryDestination>(
    teamShared ? "project" : "personal",
  );
  const [touched, setTouched] = useState(false);
  const [seenTeamShared, setSeenTeamShared] = useState(teamShared);
  if (teamShared !== seenTeamShared) {
    setSeenTeamShared(teamShared);
    if (!touched) setDestination(teamShared ? "project" : "personal");
  }

  const resolve = async (save: boolean) => {
    if (submitting || proposal.status !== "pending") return;
    setSubmitting(save ? "save" : "dismiss");
    setError(null);
    try {
      if (save) {
        const toProject = Boolean(project) && destination === "project";
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}/accept`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(toProject ? { project_id: project?.id } : {}),
        });
        if (!response.ok) {
          // A failed save is NOT a dismissal. Reporting "Dismissed." told the
          // user they had thrown the memory away when in fact nothing was
          // written and the proposal is still there to retry — and the team
          // destination has its own failure (a membership re-check server
          // side) that this card is the only place to see.
          setError(
            response.status === 404 && toProject
              ? "Couldn't save to team learnings — you may no longer be a member of this project."
              : `Couldn't save this memory (HTTP ${response.status}).`,
          );
          return;
        }
        // #515 stage 2: the accept response reports what happened to the
        // older fact this proposal claimed to replace.
        let note: string | undefined;
        try {
          const body = (await response.json()) as { supersede?: string };
          note = supersedeNote(body.supersede);
        } catch {
          // body is display-only; a parse failure never affects the save
        }
        onResolved({
          ...proposal,
          status: "saved",
          savedTo:
            project && destination === "project" ? project.name : undefined,
          resolutionNote: note,
        });
      } else {
        const response = await fetch(`/api/memories/${encodeURIComponent(proposal.id)}`, {
          method: "DELETE",
        });
        if (!response.ok) {
          setError(`Couldn't dismiss this memory (HTTP ${response.status}).`);
          return;
        }
        onResolved({ ...proposal, status: "dismissed" });
      }
    } catch {
      setError("Couldn't reach the server — nothing was saved or dismissed.");
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
          <StatusChip tone="neutral" className="font-normal">
            {proposal.kind}
          </StatusChip>
        ) : null}
      </div>
      <p className="mb-3 whitespace-pre-wrap text-[0.8125rem] leading-[1.5] text-[var(--color-text-secondary)]">
        {proposal.content}
      </p>
      {proposal.supersedesContent ? (
        <p className="mb-3 text-[0.75rem] leading-[1.5] text-[var(--color-card-text-muted)]">
          Replaces: <span className="line-through">{proposal.supersedesContent}</span>
          <span className="ml-1">(the older fact is retired only if you save)</span>
        </p>
      ) : null}
      {proposal.status === "pending" && project ? (
        <MemoryDestinationPicker
          project={project}
          value={destination}
          onChange={(next) => {
            setTouched(true);
            setDestination(next);
          }}
          disabled={submitting !== null}
        />
      ) : null}
      {error ? (
        <p
          role="alert"
          className="mb-2 text-[0.75rem] leading-[1.5] text-[var(--color-danger-text)]"
        >
          {error}
        </p>
      ) : null}
      {proposal.status === "pending" ? (
        <div className="flex items-center gap-2">
          <button
            type="button"
            className="rounded-[var(--radius-md)] bg-[var(--color-text-primary)] px-3 py-1.5 text-[0.75rem] font-medium text-[var(--color-surface-1)] transition hover:opacity-80 disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(true)}
          >
            {submitting === "save" ? "Saving..." : "Save"}
          </button>
          <button
            type="button"
            className="rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
            disabled={submitting !== null}
            onClick={() => void resolve(false)}
          >
            {submitting === "dismiss" ? "Dismissing..." : "Don't save"}
          </button>
        </div>
      ) : (
        <div className="text-[0.75rem] text-[var(--color-card-text-muted)]">
          {proposal.status === "saved"
            ? `${proposal.savedTo ? `Saved to ${proposal.savedTo}’s team learnings.` : "Saved to memories."}${proposal.resolutionNote ? " " + proposal.resolutionNote : ""}`
            : "Dismissed."}
        </div>
      )}
    </div>
  );
}
