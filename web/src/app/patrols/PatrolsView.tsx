import { t as tr } from "@/app/lib/i18n";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ChatSchedule, ConversationApprovalMode } from "@/app/chat/ui/history";
import { AutonomousRiskModal } from "@/app/chat/ui/AutonomousRiskModal";
import {
  APPROVAL_MODE_OPTIONS,
  approvalModeLabel,
  formatCountdown,
} from "@/app/chat/ui/PatrolBanner";
import { ApprovalSegmented, AutonomousConfirm, PatrolForm, editBody, type PatrolFormValues } from "@/app/chat/ui/PatrolsDialog";
import { matchesSearch, searchTokens } from "@/app/chat/ui/conversationOrganization";
import { conversationApiUrl } from "@/app/lib/conversationApiUrl";
import { currentDefaultModel } from "@/app/lib/modelAliases";
import { Link } from "@/app/lib/Link";
import { responseErrorText } from "@/app/lib/responseError";
import { cadenceWithZone } from "@/app/launchpad/cadence";
import { openPatrolWizard, usePatrolWizardOpen } from "@/app/launchpad/patrolWizard";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import {
  PatrolApiError,
  createSchedule,
  deleteSchedule,
  runScheduleNow,
  setScheduleAction,
  updateSchedule,
} from "@/app/shared/lib/patrolApi";
import { OwnerAvatar, compactUntil } from "@/app/shared/sidebar/RowStatus";
import { teamChatHref } from "@/app/shared/sidebar/chatHandoff";
import { fetchPatrolList, isOwnPatrol, refreshPatrols, usePatrols, type PatrolBrief } from "@/app/shared/sidebar/targets";
import { Icon } from "@/app/shared/ui/Icon";
import { Segmented } from "@/app/shared/ui/Segmented";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";

// PatrolsView — the Patrols page (docs/SIDEBAR.md): every patrol the person
// set up, across their chats, and the patrols teammates run in chats shared
// with them. A patrol lives in its chat; this page is where they are seen
// together, steered (run now, pause, approval mode, schedule and task) and
// started. Rows are the ones a chat's Patrols dialog shows, plus the chat
// each belongs to.
//
// A teammate's patrol is listed to read: when it runs, how it went, and a way
// into its chat. Only the person who set a patrol up can run, pause, edit or
// remove it (the server's rule), so those controls are theirs alone.

const LABEL_CLASS =
  "font-[family-name:var(--font-code)] text-[0.6875rem] font-medium uppercase tracking-[0.1em] text-[var(--color-text-subtle)]";

type PatrolRun = {
  at: number;
  due_at?: number;
  trigger: "schedule" | "manual" | string;
  outcome: "queued" | "busy" | "gone" | string;
  reason?: string;
  run_number?: number;
};

type Detail = { patrol: PatrolBrief; runs: PatrolRun[] };

async function fetchPatrolDetail(id: string): Promise<Detail | null> {
  try {
    const res = await fetch(`/api/patrols/${encodeURIComponent(id)}`, { cache: "no-store" });
    if (!res.ok) return null;
    const data = (await res.json()) as Partial<Detail>;
    if (!data.patrol) return null;
    return { patrol: data.patrol, runs: Array.isArray(data.runs) ? data.runs : [] };
  } catch {
    return null;
  }
}

/** What a run-history entry says happened. */
function runText(run: PatrolRun): string {
  if (run.outcome === "queued") {
    const what = run.trigger === "manual" ? tr("Ran when asked (Run now)") : tr("Ran on schedule");
    return run.run_number ? `${what} · ${tr("Run #{0}", [run.run_number])}` : what;
  }
  if (run.outcome === "busy") {
    if (run.reason === "approval_pending") return tr("Skipped: an approval was waiting");
    if (run.reason === "patrol_in_flight") return tr("Skipped: the previous run was still going");
    if (run.reason === "queue_full") return tr("Skipped: the chat's queue was full");
    return tr("Skipped: the chat was busy");
  }
  return tr("Stopped: the chat was no longer available");
}

/** "Today 08:00", "Wed 08:00", "Mar 3 08:00": when a run fired, in the reader's own time. */
function runWhen(at: number, now: Date): string {
  const d = new Date(at * 1000);
  const time = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false });
  const days = Math.round(
    (new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime() -
      new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()) /
      86_400_000,
  );
  if (days === 0) return `${tr("Today")} ${time}`;
  if (days > 0 && days < 7) return `${d.toLocaleDateString(undefined, { weekday: "short" })} ${time}`;
  return `${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${time}`;
}

/** One line under a patrol's name: its rhythm, how often it has run, how much it may do alone. */
function metaLine(p: PatrolBrief): string {
  return [
    cadenceWithZone(p.cron ?? "", p.timezone ?? ""),
    (p.run_count ?? 0) > 0 ? tr("Run #{0}", [p.run_count ?? 0]) : "",
    p.approval_mode ? approvalModeLabel(p.approval_mode) : "",
  ]
    .filter(Boolean)
    .join(" · ");
}

const openChatHref = (p: PatrolBrief) =>
  isOwnPatrol(p)
    ? `/chat?c=${encodeURIComponent(p.conversation_id)}`
    : teamChatHref({ id: p.conversation_id, multiplayer: p.multiplayer, team_visible: p.team_visible });

// The schedule a form edits, from what the page holds of the patrol.
function asSchedule(p: PatrolBrief): ChatSchedule {
  return {
    id: p.id,
    conversation_id: p.conversation_id,
    title: p.title,
    prompt: p.prompt ?? "",
    kind: "patrol",
    cron: p.cron,
    timezone: p.timezone,
    status: p.status === "paused" ? "paused" : "active",
    next_run_at: p.next_run_at ?? 0,
    run_count: p.run_count ?? 0,
  };
}

async function setApprovalMode(conversationId: string, mode: ConversationApprovalMode): Promise<"ok" | "accept" | string> {
  const url = conversationApiUrl(conversationId, "/auto-approve");
  if (!url) return tr("Invalid conversation or schedule id.");
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ approval_mode: mode, auto_approve: mode !== "manual" }),
    });
    // 409: Autonomous needs the person's acceptance of what it does first.
    if (res.status === 409) return "accept";
    if (!res.ok) return await responseErrorText(res, tr("Couldn't change the approval mode"));
    return "ok";
  } catch {
    return tr("Couldn't change the approval mode");
  }
}

const PILL_BUTTON =
  "pill-btn inline-flex h-9 shrink-0 items-center gap-1.5 px-3.5 font-medium disabled:opacity-40";

// ── One patrol ───────────────────────────────────────────────────────────────
function PatrolCard({
  patrol,
  now,
  expanded,
  justAdded,
  onToggle,
  onChanged,
  onError,
  requireAutonomousAccept,
  canWrite,
  chatArmed,
}: {
  // False for a read-only Viewer: the server refuses every change they
  // could make, so the row is for reading.
  canWrite: boolean;
  // A patrol in this patrol's chat is running: switching the chat to
  // Autonomous asks once more first, as the chat's own Patrols dialog does.
  chatArmed: boolean;
  patrol: PatrolBrief;
  now: number;
  expanded: boolean;
  justAdded: boolean;
  onToggle: () => void;
  // The patrol changed on the server: read the list again.
  onChanged: () => Promise<unknown>;
  onError: (message: string) => void;
  requireAutonomousAccept: (then: () => void) => void;
}) {
  const own = isOwnPatrol(patrol);
  // `mine` gates the controls: the person's own patrol, on an account that
  // may change things.
  const mine = own && canWrite;
  // The approval mode is the chat's, and only the chat's owner may change it:
  // a patrol set up in a teammate's Multiplayer chat is theirs to run and
  // edit, but its mode is read here.
  const canSetMode = mine && patrol.owns_chat !== false;
  const paused = patrol.status === "paused";
  const [confirmingAutonomous, setConfirmingAutonomous] = useState(false);
  const skipped = own && !paused && (patrol.skipped?.count ?? 0) > 0;
  const [detail, setDetail] = useState<Detail | null>(null);
  const [detailState, setDetailState] = useState<"idle" | "loading" | "failed">("idle");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(false);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [showAll, setShowAll] = useState(false);
  const busyRef = useRef(false);

  const loadDetail = useCallback(async () => {
    setDetailState("loading");
    const d = await fetchPatrolDetail(patrol.id);
    setDetail(d);
    setDetailState(d ? "idle" : "failed");
  }, [patrol.id]);
  // Read when the row opens; `expanded` is the trigger.
  useEffect(() => {
    if (!expanded) return;
    let live = true;
    void (async () => {
      const d = await fetchPatrolDetail(patrol.id);
      if (!live) return;
      setDetail(d);
      setDetailState(d ? "idle" : "failed");
    })();
    return () => {
      live = false;
    };
  }, [expanded, patrol.id]);

  // One action at a time per patrol: a Run now during a save would race it.
  const act = async (fn: () => Promise<unknown>, failed: string) => {
    if (busyRef.current) return false;
    busyRef.current = true;
    setBusy(true);
    try {
      await fn();
      await onChanged();
      if (expanded) await loadDetail();
      return true;
    } catch (e) {
      onError(e instanceof PatrolApiError ? e.message : errorMessage(e, failed));
      return false;
    } finally {
      busyRef.current = false;
      setBusy(false);
    }
  };

  const applyMode = (mode: ConversationApprovalMode) => {
    void act(async () => {
      const outcome = await setApprovalMode(patrol.conversation_id, mode);
      if (outcome === "accept") {
        requireAutonomousAccept(() => applyMode(mode));
        return;
      }
      if (outcome !== "ok") throw new Error(outcome);
    }, tr("Couldn't change the approval mode"));
  };
  const changeMode = (mode: ConversationApprovalMode) => {
    setConfirmingAutonomous(false);
    if (mode === (patrol.approval_mode ?? "manual")) return;
    // A running patrol's next run would act without cards: ask once more.
    if (mode === "autonomous" && chatArmed) {
      setConfirmingAutonomous(true);
      return;
    }
    applyMode(mode);
  };

  const prompt = detail?.patrol.prompt ?? "";
  const longPrompt = prompt.length > 420 || prompt.split("\n").length > 6;
  const nowDate = new Date(now * 1000);
  // "in 19h": to one unit, as the sidebar's row says it. The exact time is
  // the tooltip's.
  const nextAt = patrol.next_run_at ?? 0;
  // A patrol that skipped runs says so once, in the chip beside this; here it
  // still says when it checks next.
  const next = paused
    ? tr("paused")
    : nextAt > now
      ? tr("in {0}", [compactUntil(nextAt, now)])
      : nextAt > 0
        ? tr("due now")
        : "";

  return (
    <li
      data-testid="patrol-card"
      data-patrol-id={patrol.id}
      className={[
        "overflow-hidden rounded-[var(--radius-lg)] border bg-[var(--color-surface-2)]",
        justAdded ? "border-[var(--color-accent)]" : expanded ? "border-[var(--color-selected-border)]" : "border-[var(--color-border)]",
      ].join(" ")}
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 py-1.5 pl-1 pr-2.5">
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() => {
            setEditing(false);
            setConfirmRemove(false);
            onToggle();
          }}
          className="flex min-h-[4rem] min-w-0 flex-[1_1_18rem] items-center gap-2.5 rounded-[var(--radius-md)] px-2 py-1.5 text-left transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          <Icon
            name="chevron-down"
            className={`size-4 shrink-0 text-[var(--color-text-subtle)] transition-transform ${expanded ? "" : "-rotate-90"}`}
          />
          <span
            aria-hidden="true"
            className={[
              "inline-block size-2 shrink-0 rounded-full",
              paused
                ? "border-2 border-[var(--color-text-muted)]"
                : skipped
                  ? "bg-[var(--color-warning)] shadow-[0_0_0_3px_var(--color-warning-tint)]"
                  : "bg-[var(--color-state-ok)]",
            ].join(" ")}
          />
          <span className="grid min-w-0 flex-1 gap-[3px]">
            <span className="flex min-w-0 items-center gap-2">
              <span className="min-w-0 text-[0.9rem] font-semibold text-[var(--color-text-primary)] sm:truncate">
                {patrol.title || patrol.conversation_title || tr("Patrol")}
              </span>
              {!own && patrol.owner_email ? (
                <>
                  <OwnerAvatar email={patrol.owner_email} />
                  <span className="sr-only">{tr("Shared by {0}", [patrol.owner_email])}</span>
                </>
              ) : null}
            </span>
            <span className="min-w-0 text-[0.8rem] text-[var(--color-text-muted)] sm:truncate">{metaLine(patrol)}</span>
            <span className="flex min-w-0 items-center gap-1.5 text-[0.78rem] text-[var(--color-text-subtle)]">
              <Icon name="message" className="size-3 shrink-0" />
              <span className="min-w-0 truncate">{patrol.conversation_title || tr("Untitled")}</span>
            </span>
          </span>
        </button>
        {/* The row's state, then its controls. Beside the name where there
            is room; on a phone, two lines of their own under it (state, then
            controls at the card's right edge), the same on every card, so
            nothing wraps differently from one patrol to the next. */}
        <div className="ml-auto flex min-w-0 max-w-full flex-wrap items-center justify-end gap-2 pb-1 pl-[3.5rem] max-sm:w-full sm:pb-0 sm:pl-0">
          {skipped || next ? (
          <div className="flex min-w-0 flex-wrap items-center gap-2 max-sm:basis-full">
          {skipped ? (
            <span
              data-testid="patrol-skipped"
              className="inline-flex h-7 items-center gap-1.5 rounded-full border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] px-2.5 text-[0.75rem] text-[var(--color-warning-text)]"
            >
              <Icon name="alert-triangle" className="size-3 shrink-0 text-[var(--color-warning-icon)]" />
              {patrol.skipped?.count === 1
                ? tr("1 run skipped · approval waiting")
                : tr("{0} runs skipped · approval waiting", [patrol.skipped?.count ?? 0])}
            </span>
          ) : null}
          <span
            data-testid="patrol-next"
            title={!paused && nextAt > 0 ? `${formatCountdown(nextAt, now)} · ${new Date(nextAt * 1000).toLocaleString()}` : undefined}
            className="font-[family-name:var(--font-code)] text-[0.75rem] text-[var(--color-text-secondary)] sm:min-w-[3.5rem] sm:text-right"
          >
            {next}
          </span>
          </div>
          ) : null}
          <div className="flex shrink-0 items-center gap-2">
          {mine ? (
            <>
              <button
                type="button"
                disabled={busy}
                data-testid="patrol-run-now"
                onClick={() =>
                  void act(() => runScheduleNow(patrol.conversation_id, patrol.id), tr("Couldn't trigger patrol"))
                }
                className={PILL_BUTTON}
              >
                <Icon name="play" className="size-3" />
                {tr("Run now")}
              </button>
              <button
                type="button"
                disabled={busy}
                data-testid="patrol-pause"
                aria-label={paused ? tr("Resume {0}", [patrol.title]) : tr("Pause {0}", [patrol.title])}
                title={paused ? tr("Resume {0}", [patrol.title]) : tr("Pause {0}", [patrol.title])}
                onClick={() =>
                  void act(
                    () => setScheduleAction(patrol.conversation_id, patrol.id, paused ? "resume" : "pause"),
                    tr("Couldn't save patrol"),
                  )
                }
                className="hit-area inline-flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-40"
              >
                <Icon name={paused ? "play" : "pause"} className="size-3.5" />
              </button>
            </>
          ) : null}
          {/* The way into the patrol's chat: the last control on every row,
              the person's own and a teammate's alike, so it sits in one
              column down the page. A link, so it also opens in a new tab. */}
          <Link
            href={openChatHref(patrol)}
            data-testid="patrol-open-chat"
            aria-label={tr("Open chat: {0}", [patrol.conversation_title || patrol.title || tr("Untitled")])}
            className={`${PILL_BUTTON} no-underline focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]`}
          >
            <Icon name="message" className="size-3.5" />
            {tr("Open chat")}
          </Link>
          </div>
        </div>
      </div>

      {expanded && !editing ? (
        <div className="grid gap-4 border-t border-[var(--color-divider)] px-4 pb-4 pt-3.5 sm:pl-[3.4rem]">
          <div className="grid gap-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
            {canSetMode ? (
              <div className="grid content-start gap-2">
                <span className={LABEL_CLASS}>{tr("Approval mode")}</span>
                <ApprovalSegmented
                  label={tr("Approval mode")}
                  testIdPrefix="patrol-mode"
                  value={patrol.approval_mode ?? "manual"}
                  onChange={changeMode}
                />
                {confirmingAutonomous ? (
                  <AutonomousConfirm
                    onKeep={() => setConfirmingAutonomous(false)}
                    onConfirm={() => {
                      setConfirmingAutonomous(false);
                      applyMode("autonomous");
                    }}
                  />
                ) : null}
                <p className="m-0 text-[0.78rem] leading-[1.45] text-[var(--color-text-muted)]">
                  {APPROVAL_MODE_OPTIONS.find((o) => o.id === (patrol.approval_mode ?? "manual"))?.desc}{" "}
                  {tr("It is the chat's setting, so it applies to every patrol in that chat.")}
                </p>
              </div>
            ) : own ? (
              <div className="grid content-start gap-1.5" data-testid="patrol-mode-readonly">
                <span className={LABEL_CLASS}>{tr("Approval mode")}</span>
                <p className="m-0 text-[0.84rem] leading-[1.5] text-[var(--color-text-secondary)]">
                  {approvalModeLabel(patrol.approval_mode ?? "manual")}
                </p>
                <p className="m-0 text-[0.78rem] leading-[1.45] text-[var(--color-text-muted)]">
                  {patrol.owns_chat === false
                    ? tr("It is the chat's setting. Only the chat's owner can change it.")
                    : tr("It is the chat's setting, so it applies to every patrol in that chat.")}
                </p>
              </div>
            ) : (
              <div className="grid content-start gap-1.5">
                <span className={LABEL_CLASS}>{tr("Whose it is")}</span>
                <p className="m-0 text-[0.84rem] leading-[1.5] text-[var(--color-text-secondary)]">
                  {tr("{0} set this patrol up. You can read its chat; only they can run, pause or change it.", [patrol.owner_email ?? tr("A teammate")])}
                </p>
              </div>
            )}
            <div className="grid content-start gap-2">
              <span className={LABEL_CLASS}>{tr("Recent runs")}</span>
              {/* Until the first read answers there is no history to show, so
                  it is loading (never "No runs yet"), the first read included. */}
              {!detail && detailState !== "failed" ? (
                <p className="m-0 text-[0.8rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
              ) : detailState === "failed" ? (
                <p className="m-0 text-[0.8rem] text-[var(--color-text-muted)]">
                  {tr("Couldn't load this patrol's history.")}{" "}
                  <button type="button" className="text-[var(--color-link)] underline underline-offset-2" onClick={() => void loadDetail()}>
                    {tr("Try again")}
                  </button>
                </p>
              ) : (detail?.runs.length ?? 0) === 0 ? (
                <p className="m-0 text-[0.8rem] text-[var(--color-text-muted)]">{tr("No runs yet.")}</p>
              ) : (
                <ul className="m-0 grid list-none gap-1.5 p-0" data-testid="patrol-runs">
                  {detail?.runs.slice(0, 5).map((run, i) => (
                    <li key={`${run.at}-${i}`} className="flex min-w-0 items-baseline gap-2.5 text-[0.8rem]">
                      <span
                        aria-hidden="true"
                        className={[
                          "inline-block size-1.5 shrink-0 translate-y-[-1px] rounded-full",
                          run.outcome === "queued" ? "bg-[var(--color-state-ok)]" : "bg-[var(--color-warning)]",
                        ].join(" ")}
                      />
                      <span className="w-[6.5rem] shrink-0 font-[family-name:var(--font-code)] text-[0.72rem] text-[var(--color-text-muted)]">
                        {runWhen(run.at, nowDate)}
                      </span>
                      <span className="min-w-0 text-[var(--color-text-secondary)]">{runText(run)}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          </div>

          {own && prompt ? (
            <div className="grid gap-1.5">
              <span className={LABEL_CLASS}>{tr("Each run")}</span>
              <p
                data-testid="patrol-prompt"
                className={[
                  "m-0 whitespace-pre-wrap text-[0.84rem] leading-[1.5] text-[var(--color-text-secondary)]",
                  // Four lines' height, cut clean: a line clamp draws its
                  // ellipsis on a blank line between paragraphs.
                  longPrompt && !showAll ? "max-h-[6em] overflow-hidden" : "",
                ].join(" ")}
              >
                {prompt}
              </p>
              {longPrompt ? (
                <button
                  type="button"
                  aria-expanded={showAll}
                  className="justify-self-start rounded text-[0.8rem] text-[var(--color-link)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
                  onClick={() => setShowAll((v) => !v)}
                >
                  {showAll ? tr("Show less") : tr("Show full task")}
                </button>
              ) : null}
            </div>
          ) : null}

          {/* Editing and removing are the creator's; the way into the chat is
              on the row itself, for everyone. */}
          {mine ? (
          <div className="flex flex-wrap items-center gap-2">
              <button
                type="button"
                disabled={busy || !detail}
                data-testid="patrol-edit"
                onClick={() => {
                  setEditing(true);
                  setConfirmRemove(false);
                }}
                className={PILL_BUTTON}
              >
                <Icon name="edit" className="size-3.5" />
                {tr("Edit schedule & task")}
              </button>
            <span className="flex-1" />
                <button
                  type="button"
                  data-testid="patrol-remove"
                  disabled={busy && confirmRemove}
                  onClick={async () => {
                    if (!confirmRemove) {
                      setConfirmRemove(true);
                      return;
                    }
                    // A refused or unreachable DELETE leaves the confirm
                    // armed so one click retries it.
                    const ok = await act(() => deleteSchedule(patrol.conversation_id, patrol.id), tr("Couldn't cancel patrol"));
                    if (ok) setConfirmRemove(false);
                  }}
                  className={[
                    "h-9 rounded-[var(--radius-md)] border px-3 text-[0.8125rem] font-medium transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
                    confirmRemove
                      ? "border-[var(--color-status-error-border)] bg-[var(--color-status-error-bg)] text-[var(--color-status-error-fg)]"
                      : "border-transparent text-[var(--color-danger-text)] hover:bg-[var(--color-status-error-bg)]",
                  ].join(" ")}
                >
                  {confirmRemove ? tr("Confirm remove") : tr("Remove patrol")}
                </button>
                {confirmRemove ? (
                  <span role="status" className="w-full text-right text-[0.75rem] text-[var(--color-text-muted)]">
                    {tr("The patrol stops for good. Click again to confirm.")}
                  </span>
                ) : null}
          </div>
          ) : null}
        </div>
      ) : null}

      {expanded && editing && detail ? (
        <div className="border-t border-[var(--color-divider)] p-3">
          <PatrolForm
            mode="edit"
            patrolOnly
            schedule={asSchedule({ ...patrol, prompt: detail.patrol.prompt })}
            approvalMode={patrol.approval_mode ?? "manual"}
            showApprovalMode={canSetMode}
            onRequireAutonomousAccept={requireAutonomousAccept}
            confirmAutonomous={chatArmed}
            canCancel
            onCancel={() => setEditing(false)}
            onSubmit={async (v) => {
              const sc = asSchedule({ ...patrol, prompt: detail.patrol.prompt });
              let acceptMode: ConversationApprovalMode | null = null;
              const ok = await act(async () => {
                const body = editBody(sc, v);
                if (Object.keys(body).length > 0) await updateSchedule(patrol.conversation_id, patrol.id, body);
                if (canSetMode && v.approvalMode !== (patrol.approval_mode ?? "manual")) {
                  const outcome = await setApprovalMode(patrol.conversation_id, v.approvalMode);
                  // The schedule is saved; the mode waits for the person's
                  // acceptance of what Autonomous does, then is applied.
                  if (outcome === "accept") acceptMode = v.approvalMode;
                  else if (outcome !== "ok") throw new Error(outcome);
                }
              }, tr("Couldn't save patrol"));
              if (ok && acceptMode) {
                const mode = acceptMode;
                requireAutonomousAccept(() => applyMode(mode));
              }
              if (ok) setEditing(false);
              return ok;
            }}
          />
        </div>
      ) : null}
    </li>
  );
}

// ── The new-patrol form ──────────────────────────────────────────────────────
type OwnChat = { id: string; title: string; lockdown?: boolean };

function NewPatrolForm({
  canCancel,
  onCancel,
  onStarted,
  onError,
  requireAutonomousAccept,
}: {
  canCancel: boolean;
  onCancel: () => void;
  onStarted: (scheduleId: string) => Promise<unknown>;
  onError: (message: string) => void;
  requireAutonomousAccept: (then: () => void) => void;
}) {
  const [where, setWhere] = useState<"new" | "existing">("new");
  const [chats, setChats] = useState<OwnChat[] | null>(null);
  const [chatsFailed, setChatsFailed] = useState(false);
  const [chatId, setChatId] = useState("");

  // The person's own chats, read when they ask to pick one. A sealed chat
  // cannot take a patrol.
  useEffect(() => {
    if (where !== "existing" || chats) return;
    let live = true;
    void (async () => {
      try {
        const res = await fetch("/api/conversations", { cache: "no-store" });
        if (!res.ok) throw new Error(String(res.status));
        const data = (await res.json()) as { conversations?: OwnChat[] };
        if (live) {
          setChats((data.conversations ?? []).filter((c) => !c.lockdown));
          setChatsFailed(false);
        }
      } catch {
        if (live) setChatsFailed(true);
      }
    })();
    return () => {
      live = false;
    };
  }, [where, chats]);

  const start = async (v: PatrolFormValues): Promise<boolean> => {
    const body = {
      title: v.title,
      prompt: v.prompt,
      cron: v.cron,
      timezone: v.timezone,
      auto_approve: v.approvalMode !== "manual",
      approval_mode: v.approvalMode,
    };
    let created = "";
    try {
      let conversationId = chatId;
      if (where === "new") {
        const res = await fetch("/api/conversations", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ title: v.title, model: currentDefaultModel() }),
        });
        if (!res.ok) throw new Error(await responseErrorText(res, tr("Could not create the patrol chat")));
        conversationId = ((await res.json()) as { id?: string }).id ?? "";
        if (!conversationId) throw new Error(tr("Could not create the patrol chat"));
        created = conversationId;
      }
      const sched = await createSchedule(conversationId, {
        ...body,
        // Next to a chat's existing patrols this adds one; it never
        // overwrites the patrol already running there.
        allow_multiple: where === "existing" || undefined,
      });
      await onStarted(sched.schedule?.id ?? "");
      return true;
    } catch (e) {
      // A chat made for a patrol that then could not be armed is taken back
      // out, so a retry does not leave an empty chat behind.
      const url = created ? conversationApiUrl(created) : null;
      if (url) await fetch(url, { method: "DELETE" }).catch(() => null);
      if (e instanceof PatrolApiError && e.status === 409 && v.approvalMode === "autonomous") {
        requireAutonomousAccept(() => void start(v));
        return false;
      }
      onError(e instanceof PatrolApiError ? e.message : errorMessage(e, tr("Couldn't schedule patrol")));
      return false;
    }
  };

  return (
    <PatrolForm
      mode="new"
      patrolOnly
      approvalMode="auto"
      onRequireAutonomousAccept={requireAutonomousAccept}
      canCancel={canCancel}
      onCancel={onCancel}
      ready={where === "new" || Boolean(chatId)}
      onSubmit={start}
      headingAction={
        <button
          type="button"
          data-testid="patrols-guided"
          className="inline-flex shrink-0 items-center gap-1.5 rounded-md px-1.5 py-1 text-[0.8125rem] text-[var(--color-link)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
          onClick={(e) => openPatrolWizard(e.currentTarget)}
        >
          <Icon name="sparkles" className="size-3.5" />
          {tr("Use guided setup")}
        </button>
      }
      intro={
        <div className="grid gap-2">
          <span className={LABEL_CLASS}>{tr("Runs in")}</span>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <Segmented<"new" | "existing">
              label={tr("Runs in")}
              value={where}
              onChange={setWhere}
              options={[
                { value: "new", label: tr("A new chat") },
                { value: "existing", label: tr("An existing chat") },
              ]}
            />
            {where === "new" ? (
              <span className="text-[0.8rem] text-[var(--color-text-muted)]">
                {tr("Named after the label, with the Patrol bar on top.")}
              </span>
            ) : chatsFailed ? (
              <span role="alert" className="text-[0.8rem] text-[var(--color-danger-text)]">
                {tr("Couldn't load your chats.")}
              </span>
            ) : (
              <NativeSelect
                aria-label={tr("Chat to patrol")}
                data-testid="patrols-new-chat"
                value={chatId}
                onChange={(e) => setChatId(e.target.value)}
                className="h-9 min-w-0 max-w-[22rem] flex-1 bg-[var(--color-surface-1)] pl-2.5 pr-8 text-[0.8125rem]"
              >
                <NativeSelectOption value="">{chats ? tr("Choose a chat…") : tr("Loading…")}</NativeSelectOption>
                {(chats ?? []).map((c) => (
                  <NativeSelectOption key={c.id} value={c.id}>
                    {c.title || tr("Untitled")}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </div>
        </div>
      }
    />
  );
}

// ── The page's body ──────────────────────────────────────────────────────────
export function PatrolsView({
  adding,
  setAdding,
  canWrite = true,
}: {
  // False for a read-only Viewer: nothing that changes a patrol is offered.
  canWrite?: boolean;
  // The new-patrol form is open (the header's New patrol button owns it).
  adding: boolean;
  setAdding: (open: boolean) => void;
}) {
  const patrols = usePatrols(true);
  const [state, setState] = useState<"loading" | "ready" | "failed">("loading");
  const [query, setQuery] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [justAddedId, setJustAddedId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [pendingRisk, setPendingRisk] = useState<(() => void) | null>(null);
  const [now, setNow] = useState(() => Math.floor(Date.now() / 1000));

  const reload = useCallback(async () => {
    const list = await refreshPatrols();
    setState(list ? "ready" : "failed");
    return list;
  }, []);
  // The first read tells "none yet" from "could not load"; the shared store
  // (usePatrols) then keeps this page and the sidebar's marks in step.
  useEffect(() => {
    let live = true;
    void fetchPatrolList().then((list) => {
      if (!live) return;
      setState(list ? "ready" : "failed");
      void refreshPatrols();
    });
    return () => {
      live = false;
    };
  }, []);
  // The countdowns tick, and a patrol changed elsewhere (its chat, another
  // tab) shows when the page is looked at again.
  useEffect(() => {
    const id = window.setInterval(() => setNow(Math.floor(Date.now() / 1000)), 30_000);
    const onFocus = () => void refreshPatrols();
    window.addEventListener("focus", onFocus);
    return () => {
      window.clearInterval(id);
      window.removeEventListener("focus", onFocus);
    };
  }, []);
  // The wizard may have launched one: read again when it closes.
  const wizardOpen = usePatrolWizardOpen();
  const wasWizardOpen = useRef(wizardOpen);
  useEffect(() => {
    if (wasWizardOpen.current && !wizardOpen) void reload();
    wasWizardOpen.current = wizardOpen;
  }, [wizardOpen, reload]);

  const tokens = useMemo(() => searchTokens(query), [query]);
  const shown = useMemo(
    () => patrols.filter((p) => matchesSearch(tokens, p.title, p.conversation_title, p.owner_email)),
    [patrols, tokens],
  );
  const mine = shown.filter(isOwnPatrol);
  const shared = shown.filter((p) => !isOwnPatrol(p));
  const requireAutonomousAccept = useCallback((then: () => void) => setPendingRisk(() => then), []);
  // The chats with a running patrol, the caller's or a teammate's.
  const armedChats = useMemo(
    () => new Set(patrols.filter((p) => p.status === "active").map((p) => p.conversation_id)),
    [patrols],
  );

  const card = (p: PatrolBrief) => (
    <PatrolCard
      key={p.id}
      patrol={p}
      now={now}
      expanded={expandedId === p.id}
      justAdded={justAddedId === p.id}
      onToggle={() => {
        setExpandedId((cur) => (cur === p.id ? null : p.id));
        setJustAddedId(null);
      }}
      onChanged={reload}
      onError={setError}
      requireAutonomousAccept={requireAutonomousAccept}
      canWrite={canWrite}
      chatArmed={armedChats.has(p.conversation_id)}
    />
  );

  const form = (
    <NewPatrolForm
      canCancel
      onCancel={() => setAdding(false)}
      onError={setError}
      requireAutonomousAccept={requireAutonomousAccept}
      onStarted={async (id) => {
        setAdding(false);
        setQuery("");
        setJustAddedId(id || null);
        await reload();
      }}
    />
  );

  return (
    <div className="mx-auto grid w-full max-w-[72rem] gap-4 px-4 pb-14 pt-5 sm:px-6" data-testid="patrols-page">
      {error ? (
        <div
          role="alert"
          data-testid="patrols-page-error"
          className="flex items-start gap-2 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] px-3 py-2 text-[0.8125rem] leading-[1.5] text-[var(--color-danger-text)]"
        >
          <p className="m-0 min-w-0 flex-1">{error}</p>
          <button type="button" className="shrink-0 underline underline-offset-2" onClick={() => setError(null)}>
            {tr("Dismiss")}
          </button>
        </div>
      ) : null}

      {state === "loading" && patrols.length === 0 ? (
        <p className="m-0 py-6 text-[0.875rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
      ) : state === "failed" && patrols.length === 0 ? (
        <div role="alert" data-testid="patrols-load-failed" className="grid justify-items-start gap-2 py-6">
          <p className="m-0 text-[0.875rem] text-[var(--color-text-secondary)]">
            {tr("Couldn’t load your patrols. They are safe in Chat.")}
          </p>
          <button type="button" className={PILL_BUTTON} onClick={() => void reload()}>
            <Icon name="refresh" className="size-3.5" />
            {tr("Try again")}
          </button>
        </div>
      ) : patrols.length === 0 && !canWrite ? (
        <p className="m-0 py-6 text-[0.875rem] text-[var(--color-text-muted)]" data-testid="patrols-empty-readonly">
          {tr("No patrols are shared with you yet.")}
        </p>
      ) : patrols.length === 0 ? (
        adding ? (
          form
        ) : (
          // Nothing yet: the guided setup is the way in, with the plain form
          // a click away for someone who knows what they want.
          <section
            data-testid="patrols-empty"
            className="grid justify-items-center gap-4 rounded-[var(--radius-xl)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-6 py-14 text-center"
          >
            <span aria-hidden="true" className="picker-icon-tile size-14">
              <Icon name="sparkles" className="size-6" />
            </span>
            <h2 className="m-0 font-heading text-[1.4rem] font-bold text-[var(--color-text-primary)]">
              {tr("Set up your first patrol")}
            </h2>
            <p className="m-0 max-w-[34rem] text-[0.9375rem] leading-[1.6] text-[var(--color-text-secondary)]">
              {tr("A patrol is a chat that checks your tools on a schedule and brings you findings, not spreadsheets. The guided setup takes about two minutes.")}
            </p>
            <button
              type="button"
              data-testid="patrols-first"
              className="inline-flex h-11 items-center gap-2 rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-5 text-[0.9375rem] font-semibold text-[var(--color-on-primary-fill)] transition hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={(e) => openPatrolWizard(e.currentTarget)}
            >
              <Icon name="sparkles" className="size-4" />
              {tr("Set up your first patrol")}
            </button>
            <button
              type="button"
              data-testid="patrols-write"
              className="rounded text-[0.875rem] text-[var(--color-link)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
              onClick={() => setAdding(true)}
            >
              {tr("Or write one yourself")}
            </button>
          </section>
        )
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-3">
            <div className="relative flex min-w-0 max-w-[30rem] flex-1 items-center">
              <Icon
                name="search"
                className="pointer-events-none absolute left-3 size-[0.95rem] text-[var(--color-text-muted)]"
              />
              <Input
                type="search"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                placeholder={tr("Search patrols")}
                aria-label={tr("Search patrols")}
                data-testid="patrols-search"
                className="search-input-no-native-clear h-10 pl-9 text-[0.875rem]"
              />
            </div>
            <span className="text-[0.8125rem] text-[var(--color-text-muted)]" data-testid="patrols-count" role="status">
              {tokens.length > 0
                ? tr("{0} of {1}", [shown.length, patrols.length])
                : patrols.length === 1
                  ? tr("1 patrol")
                  : tr("{0} patrols", [patrols.length])}
            </span>
          </div>

          {adding && canWrite ? form : null}

          {shown.length === 0 ? (
            <p className="m-0 py-4 text-[0.875rem] text-[var(--color-text-muted)]">
              {tr("Nothing matches “{0}”.", [query.trim()])}
            </p>
          ) : (
            <>
              {mine.length > 0 ? (
                <ul className="m-0 grid list-none gap-2.5 p-0" aria-label={tr("Your patrols")}>
                  {mine.map(card)}
                </ul>
              ) : null}
              {shared.length > 0 ? (
                <section className="grid gap-2.5" aria-labelledby="patrols-shared">
                  <h2 id="patrols-shared" className={`${LABEL_CLASS} m-0 mt-2`}>
                    {tr("Shared with you")}
                  </h2>
                  <ul className="m-0 grid list-none gap-2.5 p-0">{shared.map(card)}</ul>
                </section>
              ) : null}
            </>
          )}
        </>
      )}

      <AutonomousRiskModal
        open={pendingRisk !== null}
        onAccepted={() => {
          const next = pendingRisk;
          setPendingRisk(null);
          next?.();
        }}
        onClose={() => setPendingRisk(null)}
      />
    </div>
  );
}
