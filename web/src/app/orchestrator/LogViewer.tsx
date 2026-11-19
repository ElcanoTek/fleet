import { formatCredits } from "@/app/shared/lib/credits";
import { formatFixed } from "@/app/lib/i18n";
import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type {
  LogSession,
  Task,
  TaskStreamFrame,
} from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi, OrchestratorError } from "@/app/shared/lib/orchestratorApi";
import { formatTimeFirst } from "@/app/shared/lib/format";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { runStateLabel, RunStatusChip } from "@/app/shared/ui/StatusChip";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { useToast } from "@/app/shared/ui/Toast";
import {
  blockedReason,
  createdByLabel,
  scheduleLabel,
  scheduleStoppedReason,
  scheduleTitle,
  taskRunLabel,
  TaskSlaBadge,
  TaskStatusBadge,
  isLive,
  isRunnable,
  isTerminal,
} from "./taskDisplay";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import {
  Checklist,
  parseTaskTrackerOutput,
  type ChecklistState,
} from "./Checklist";
import { ToolStepCard, TranscriptTimeline } from "./LogTimeline";
import { formatDuration, unwrapDeferredCall } from "./logTimeline";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { saveBlob } from "@/app/shared/lib/downloadFile";
import { copyText } from "@/app/shared/lib/clipboard";

// The transcript itself — tool steps, events, the prompt, filters — renders in
// LogTimeline.tsx; this file owns the modal around it. The ReactMarkdown
// pipeline (micromark + remark-gfm, ~43 KiB transfer) and the workspace img/a
// rewrites live in LogMarkdown, which LogTimeline lazy-loads so the initial
// /orchestrator bundle doesn't pay for them (same split as chat's
// AssistantContent, #757).

// LogViewer — the task log modal. React port of moc modals.js openLogModal().
// moc rendered logs with marked + DOMPurify + highlight.js; per the migration
// plan those are DROPPED in favor of react-markdown (the chat toolchain
// standard), which is safe-by-default (no raw HTML) so DOMPurify is unneeded.
//
// Inline images (#271): a scheduled task's agent can produce an image with the
// generate_image tool and reference it in its reply as `![alt](weekly.png)`,
// exactly as it does in interactive chat. Without rewriting, ReactMarkdown
// would emit `<img src="weekly.png">` and the browser would 404 on a sibling of
// the orchestrator page. The img/a overrides below rewrite a RELATIVE workspace
// path to the task's workspace file proxy (resolveTaskWorkspaceHref →
// /api/orchestrator/tasks/<id>/workspace/<path>), reusing chat's existing
// workspace-href safety policy:
//   - Only relative paths the agent wrote into its own workspace are rewritten.
//     Absolute http(s)/data/mailto/protocol-relative/site-root hrefs pass
//     through untouched, so a poisoned log can't make the browser fetch an
//     arbitrary remote URL (no SSRF / tracking-pixel vector).
//   - The bytes are streamed through the authenticated, task-creator-scoped
//     workspace proxy (#287's GET /tasks/{id}/workspace/*), which path-guards
//     every access with SafeWorkspaceJoin.
//   - A workspace image that fails to load (file GC'd, still running, wrong
//     type) DEGRADES to a plain download link rather than a broken image.

export type LogViewerProps = {
  task: Task | null;
  onClose: () => void;
  // canStop shows the Stop button on a live run. The server enforces the real
  // permission (admin or the task's creator); this only gates the affordance.
  canStop?: boolean;
  // readOnly hides every control that writes: Resubmit / Run now, the wake
  // event, and run feedback. A readonly Operations Center user is refused all
  // of them with 403, so offering them only manufactures failures. Edit,
  // Delete and Stop are gated by their own props (omit onEdit / onDelete,
  // pass canStop={false}).
  readOnly?: boolean;
  // Called after a successful Resubmit so the dashboard can refetch and show
  // the new run immediately.
  onResubmitted?: () => void;
  // Opens the edit form for this task (parent closes the log modal first).
  onEdit?: (task: Task) => void;
  // Swaps the modal to another task (the History panel's row click).
  onSelectTask?: (task: Task) => void;
  // Permanently delete this task. The parent owns the confirm + API call and
  // closes the modal, so this component stays presentational (same contract as
  // onEdit). Omitted = no affordance.
  onDelete?: (task: Task) => void;
  // Called once when a live run's stream reports that the run ended. Which
  // view renders follows task.status, so the finished record replaces the
  // live view only once the parent hands in a copy of the task that is no
  // longer running; this is the parent's cue to fetch one.
  onRunEnded?: (taskId: string) => void;
};

export function LogViewer({ task, onClose, canStop, readOnly, onResubmitted, onEdit, onSelectTask, onDelete, onRunEnded }: LogViewerProps) {
  if (!task) return null;
  // Key the inner body on the task id so switching tasks remounts the fetch
  // hook — that reproduces the old "reset session to null then refetch on task
  // change" behavior cleanly, without a manual reset effect.
  const live = isLive(task.status);
  if (live) {
    return (
      <LiveTaskView
        key={task.id}
        task={task}
        onClose={onClose}
        canStop={!!canStop}
        onRunEnded={onRunEnded}
      />
    );
  }
  return (
    <LogViewerBody
      key={task.id}
      task={task}
      onClose={onClose}
      readOnly={!!readOnly}
      onResubmitted={onResubmitted}
      onEdit={onEdit}
      onSelectTask={onSelectTask}
      onDelete={onDelete}
    />
  );
}

// TaskSummary mirrors the Recent Tasks row the user clicked (ID / Status /
// SLA / Schedule / Created By / Created) so the modal carries the row's
// context — which in turn lets the phone card view drop those columns.
function TaskSummary({ task }: { task: Task }) {
  const schedule = scheduleLabel(task);
  const items: Array<{ label: string; node: ReactNode }> = [
    {
      label: tr("ID"),
      node: (
        <code title={task.id}>{task.id.slice(0, 8)}…</code>
      ),
    },
    { label: tr("Status"), node: <TaskStatusBadge task={task} /> },
    { label: tr("SLA"), node: <TaskSlaBadge task={task} /> },
    {
      label: tr("Schedule"),
      node: (
        <span title={scheduleTitle(task)}>{schedule}</span>
      ),
    },
    { label: tr("Created by"), node: <span>{createdByLabel(task)}</span> },
    { label: tr("Created"), node: <span>{formatTimeFirst(task.created_at)}</span> },
  ];
  // A stopped schedule says so, and why, in the summary itself — a tooltip
  // alone is invisible on a phone (ADR-0073).
  const stopped = scheduleStoppedReason(task);
  if (stopped) {
    items.push({
      label: tr("Schedule stopped"),
      node: <span data-testid="schedule-stopped">{stopped}</span>,
    });
  }
  // A Blocked run says why in the summary too, for the same reason.
  const blocked = blockedReason(task);
  if (blocked) {
    items.push({
      label: tr("Blocked"),
      node: <span data-testid="blocked-reason">{blocked}</span>,
    });
  }
  if (task.status === "paused_awaiting_wake") {
    items.push({
      label: tr("Wake"),
      node: (
        <span data-testid="wake-summary">
          {task.wake_event_key ? tr("event “{0}”", [task.wake_event_key]) : "timer"}
          {task.wake_at ? tr(" · by {0}", [formatTimeFirst(task.wake_at)]) : ""}
        </span>
      ),
    });
  }
  return (
    <div className="task-summary" data-testid="task-summary">
      {items.map((it) => (
        <div key={it.label} className="task-summary-item">
          <span className="task-summary-label">{it.label}</span>
          <span className="task-summary-value">{it.node}</span>
        </div>
      ))}
    </div>
  );
}

// SessionMetrics — the moc-style strip over the transcript: cumulative token
// and cost figures recorded on the captain's-log session.
function SessionMetrics({ session }: { session: LogSession }) {
  const num = (n: number | undefined) =>
    typeof n === "number" ? n.toLocaleString(getLanguage()) : undefined;
  const toolCalls = (session.messages ?? []).reduce(
    (n, m) => n + (m.tool_calls?.length ?? 0),
    0,
  );
  const span =
    session.created_at && session.updated_at && session.updated_at > session.created_at
      ? formatDuration(session.updated_at - session.created_at)
      : undefined;
  const tiles: Array<{ label: string; value: string | undefined }> = [
    { label: tr("Session"), value: session.title || undefined },
    { label: tr("Duration"), value: span },
    { label: tr("Messages"), value: num(session.messages?.length) },
    { label: tr("Tool calls"), value: toolCalls > 0 ? num(toolCalls) : undefined },
    { label: tr("Prompt tokens"), value: num(session.prompt_tokens) },
    { label: tr("Completion tokens"), value: num(session.completion_tokens) },
    { label: tr("Cached tokens"), value: num(session.cached_tokens) },
    {
      label: tr("Cost"),
      value:
        typeof session.cost === "number" && session.cost > 0
          ? formatCredits(session.cost, 4)
          : undefined,
    },
  ];
  const shown = tiles.filter((t) => t.value !== undefined);
  if (shown.length === 0) return null;
  return (
    <div className="log-metrics" data-testid="log-metrics">
      {shown.map((t) => (
        <div key={t.label} className="log-metric">
          <span className="log-metric-label">{t.label}</span>
          <span className="log-metric-value">{t.value}</span>
        </div>
      ))}
    </div>
  );
}

// RunCorrelation — the ids that join this run to the process log and to its
// trace (#2052): every log line of the run carries run_id=…, and every span of
// it (model calls, tools, sandbox, broker, DB) is in the one trace. Shown only
// when the run recorded them (runs before #2052 did not; trace_id only when
// tracing is on), each with a copy action.
function RunCorrelation({ session }: { session: LogSession }) {
  const ids: Array<{ label: string; value: string }> = [];
  if (session.run_id) ids.push({ label: tr("Run ID"), value: session.run_id });
  if (session.trace_id) ids.push({ label: tr("Trace ID"), value: session.trace_id });
  if (ids.length === 0) return null;
  return (
    <div className="log-correlation" data-testid="log-correlation">
      {ids.map((id) => (
        <CorrelationID key={id.label} label={id.label} value={id.value} />
      ))}
    </div>
  );
}

function CorrelationID({ label, value }: { label: string; value: string }) {
  const [state, setState] = useState<"idle" | "ok" | "err">("idle");
  const resetRef = useRef<number | null>(null);
  useEffect(
    () => () => {
      if (resetRef.current) window.clearTimeout(resetRef.current);
    },
    [],
  );
  const onCopy = async () => {
    setState((await copyText(value)) ? "ok" : "err");
    if (resetRef.current) window.clearTimeout(resetRef.current);
    resetRef.current = window.setTimeout(() => setState("idle"), 1500);
  };
  return (
    <div className="log-correlation-item">
      <span className="log-metric-label">{label}</span>
      <code className="log-correlation-value">{value}</code>
      <button
        type="button"
        className="btn btn-small"
        aria-label={tr("Copy {0}", [label])}
        onClick={() => void onCopy()}
      >
        {state === "idle" ? tr("Copy") : state === "ok" ? tr("Copied") : tr("Copy failed")}
      </button>
    </div>
  );
}

// ── #508 live activity view ──────────────────────────────────────────────────

type ActivityEntry = {
  key: string;
  kind: "message" | "tool";
  callId?: string;
  name?: string;
  text: string;
  result?: string;
  isError?: boolean;
  pending?: boolean;
  repeats?: number;
};

function sameFailedCall(a: ActivityEntry, b: ActivityEntry) {
  return a.kind === "tool" && b.kind === "tool" && a.isError && b.isError &&
    a.name === b.name && a.text === b.text && a.result === b.result;
}

// Keep enough tool output for real debugging without letting a pathological
// result pin an unbounded amount of browser memory. Long entries render behind
// a disclosure below; the old 600-character clamp hid the useful error tail.
const clampText = (s: string, max = 20_000) =>
  s.length > max
    ? s.slice(0, max) + tr("\n… output capped at 20,000 characters")
    : s;

// streamGoneMessage maps a stream failure that will not heal on reconnect to
// the final message shown in place of "reconnecting…"; null means transient.
// A live task can 404 for a moment during the leased → running handoff,
// before the run registers its in-memory stream (and before any log exists),
// so a 404 ends the watch only after this many in a row; 401/403 end it at
// once.
const STREAM_NOT_FOUND_GRACE = 5;

function streamGoneMessage(err: unknown): string | null {
  if (!(err instanceof OrchestratorError)) return null;
  if (err.status === 401) return tr("Sign in again to keep watching this run.");
  if (err.status === 403 || err.status === 404) return tr("This run is no longer available.");
  return null;
}

// LiveTaskView attaches to GET /tasks/{id}/stream and renders the run's
// tool-by-tool activity as it happens (#508): each tool call, its result, and
// the assistant's text, chronologically, with an optional Stop control that
// interrupts the governed run at its next checkpoint (with who-stopped-it
// attribution recorded server-side).
function LiveTaskView({
  task,
  onClose,
  canStop,
  onRunEnded,
}: {
  task: Task;
  onClose: () => void;
  canStop: boolean;
  onRunEnded?: (taskId: string) => void;
}) {
  const [entries, setEntries] = useState<ActivityEntry[]>([]);
  // Which live step cards are open, and each tool entry's 1-based step number.
  const [openEntries, setOpenEntries] = useState<Set<string>>(new Set());
  const toggleEntry = useCallback(
    (key: string) =>
      setOpenEntries((prev) => {
        const next = new Set(prev);
        if (next.has(key)) next.delete(key);
        else next.add(key);
        return next;
      }),
    [],
  );
  const toolSteps = useMemo(() => {
    const steps = new Map<string, number>();
    for (const e of entries) if (e.kind === "tool") steps.set(e.key, steps.size + 1);
    return steps;
  }, [entries]);
  // checklist holds the LATEST task_tracker plan (#518) for the live progress
  // panel; it updates each time the agent revises its plan mid-run.
  const [checklist, setChecklist] = useState<ChecklistState | null>(null);
  const [checklistOpen, setChecklistOpen] = useState(false);
  const [runStatus, setRunStatus] = useState<string>("running");
  const [stoppedBy, setStoppedBy] = useState<string | null>(null);
  const [stopping, setStopping] = useState(false);
  const [streamError, setStreamError] = useState<string | null>(null);
  const seq = useRef(0);
  const terminalRef = useRef(false);
  // Escape closes, Tab is trapped, focus returns to the row that opened it —
  // the overlay was aria-modal in name only.
  const overlayRef = useRef<HTMLDivElement | null>(null);
  useDialogA11y(true, overlayRef, onClose);
  const lastEventID = useRef<string | undefined>(undefined);
  const bottomRef = useRef<HTMLDivElement | null>(null);
  // Through a ref so a parent re-render does not reattach the stream.
  const onRunEndedRef = useRef(onRunEnded);
  useEffect(() => {
    onRunEndedRef.current = onRunEnded;
  });

  useEffect(() => {
    const ac = new AbortController();
    const onFrame = (frame: TaskStreamFrame) => {
      setStreamError(null);
      if (frame._event_id) lastEventID.current = frame._event_id;
      if (frame.type === "status") {
        if (frame.status && frame.status !== "running") {
          const first = !terminalRef.current;
          terminalRef.current = true;
          setRunStatus(frame.status);
          if (frame.stopped_by) setStoppedBy(frame.stopped_by);
          if (first) onRunEndedRef.current?.(task.id);
        }
        return;
      }
      if (frame.type === "agent_message" && (frame.content || frame.replace)) {
        const content = frame.content ?? "";
        // Coalesce adjacent text deltas into one readable assistant entry.
        // replace=true is the run's authoritative final text: drop the
        // concatenated pre-audit draft instead of appending.
        setEntries((prev) => {
          if (frame.replace) {
            const kept = prev.filter((e) => e.kind !== "message");
            const entry: ActivityEntry = {
              key: `e${seq.current++}`,
              kind: "message",
              text: clampText(content),
            };
            return [...kept, entry].slice(-1000);
          }
          const last = prev[prev.length - 1];
          if (last?.kind === "message") {
            return [...prev.slice(0, -1), { ...last, text: clampText(last.text + content) }];
          }
          const entry: ActivityEntry = { key: `e${seq.current++}`, kind: "message", text: content };
          return [...prev, entry].slice(-1000);
        });
        return;
      } else if (frame.type === "tool_call") {
        // task_tracker is represented once by the dedicated progress panel.
        if (frame.name === "task_tracker") return;
        const display = unwrapDeferredCall(frame.name, frame.input);
        const entry: ActivityEntry = {
          key: `e${seq.current++}`,
          kind: "tool",
          callId: frame.call_id,
          name: display.name,
          text: clampText(display.input),
          pending: true,
          repeats: 1,
        };
        setEntries((prev) => [...prev, entry].slice(-1000));
        return;
      } else if (frame.type === "tool_result") {
        const parsedChecklist =
          frame.name === "task_tracker"
            ? parseTaskTrackerOutput(frame.output ?? "")
            : null;
        if (parsedChecklist) {
          setChecklist(parsedChecklist);
          return;
        }
        setEntries((prev) => {
          let idx = -1;
          if (frame.call_id) {
            for (let i = prev.length - 1; i >= 0; i -= 1) {
              if (prev[i].kind === "tool" && prev[i].callId === frame.call_id) {
                idx = i;
                break;
              }
            }
          }
          const result = clampText(frame.output ?? "");
          if (idx < 0) {
            const entry: ActivityEntry = {
              key: `e${seq.current++}`,
              kind: "tool",
              callId: frame.call_id,
              name: frame.name || "tool",
              text: "",
              result,
              isError: !!frame.error,
              pending: false,
              repeats: 1,
            };
            return [...prev, entry].slice(-1000);
          }
          const updated = { ...prev[idx], result, isError: !!frame.error, pending: false };
          const next = [...prev.slice(0, idx), updated, ...prev.slice(idx + 1)];
          const before = next[idx - 1];
          if (idx === next.length - 1 && before && sameFailedCall(before, updated)) {
            return [...next.slice(0, idx - 1), {
              ...before,
              repeats: (before.repeats ?? 1) + 1,
            }].slice(-1000);
          }
          return next.slice(-1000);
        });
      }
    };
    // fetch streams do not reconnect like EventSource. Keep reattaching with
    // Last-Event-ID until a terminal status arrives, so a proxy/mobile network
    // blip does not silently freeze the live Operations Center view.
    const pump = async () => {
      let failures = 0;
      let notFound = 0;
      while (!ac.signal.aborted && !terminalRef.current) {
        try {
          await orchestratorApi.streamTaskActivity(
            task.id,
            onFrame,
            ac.signal,
            lastEventID.current,
          );
          failures = 0;
          notFound = 0;
          if (!terminalRef.current)
            await new Promise((resolve) => window.setTimeout(resolve, 500));
        } catch (err) {
          if (ac.signal.aborted) return;
          // A refusal that retrying cannot change ends the watch with a final
          // message instead of reconnecting forever: the run is gone or not
          // ours (404/403), or the session lapsed (401). Network errors, 5xx,
          // 408 and 429 stay transient and keep the backoff loop, and so does
          // a 404 until STREAM_NOT_FOUND_GRACE in a row (the stream may not be
          // registered yet).
          const is404 = err instanceof OrchestratorError && err.status === 404;
          notFound = is404 ? notFound + 1 : 0;
          const gone = streamGoneMessage(err);
          if (gone && (!is404 || notFound >= STREAM_NOT_FOUND_GRACE)) {
            setStreamError(gone);
            return;
          }
          failures += 1;
          setStreamError(
            tr("{0}; reconnecting…", [errorMessage(err, tr("stream failed"))]),
          );
          await new Promise((resolve) =>
            window.setTimeout(
              resolve,
              Math.min(5000, 500 * 2 ** Math.min(failures, 4)),
            ),
          );
        }
      }
      if (terminalRef.current) setStreamError(null);
    };
    void pump();
    return () => ac.abort();
  }, [task.id]);

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: "end" });
  }, [entries.length, runStatus]);

  const stop = async () => {
    if (stopping) return;
    setStopping(true);
    try {
      await orchestratorApi.cancelTask(task.id);
      // The terminal "stopped" frame arrives on the stream; nothing else to do.
    } catch (err) {
      setStreamError(errorMessage(err, tr("stop failed")));
      setStopping(false);
    }
  };

  const terminal = runStatus !== "running";
  return (
    <div
      ref={overlayRef}
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label={tr("Live task activity")}
    >
      <div className="modal modal-log live-activity-modal">
        <div className="modal-header">
          <h3 className="modal-log-title" title={task.prompt ?? ""}>
            {tr("Live activity")}
            <RunStatusChip
              state={terminal ? (runStatus === "succeeded" ? "success" : runStatus === "stopped" ? "cancelled" : "error") : "running"}
              className="ml-2"
              testId="live-run-status"
            >
              {runStateLabel(runStatus)}
              {stoppedBy ? tr(" by {0}", [stoppedBy]) : ""}
            </RunStatusChip>
          </h3>
          <div style={{ display: "flex", gap: "0.5rem", alignItems: "center" }}>
            {canStop && !terminal ? (
              <button
                type="button"
                className="btn btn-danger"
                data-testid="stop-task-button"
                disabled={stopping}
                onClick={() => void stop()}
              >
                {stopping ? tr("Stopping…") : tr("Stop run")}
              </button>
            ) : null}
            <CloseButton label={tr("Close modal")} onClick={onClose} />
          </div>
        </div>
        <TaskSummary task={task} />
        {checklist ? (
          <div className="live-progress-panel" data-testid="live-progress-panel">
            <button
              type="button"
              className="live-progress-toggle"
              aria-expanded={checklistOpen}
              onClick={() => setChecklistOpen((open) => !open)}
            >
              <span className="live-progress-copy">
                <span className="live-progress-meta">
        {rich("Progress · {0}/{1} done", checklist.summary.done, checklist.summary.total)}
      </span>
                <span className="live-progress-active">
                  {checklist.activeTask || tr("Plan complete")}
                </span>
              </span>
              <span className="live-progress-chevron" aria-hidden="true">
                {checklistOpen ? "▴" : "▾"}
              </span>
            </button>
            <div className="live-progress-track" aria-hidden="true">
              <span
                style={{
                  width: `${checklist.summary.total ? (checklist.summary.done / checklist.summary.total) * 100 : 0}%`,
                }}
              />
            </div>
            {checklistOpen ? (
              <div className="live-progress-details">
                <Checklist state={checklist} live showSummary={false} />
              </div>
            ) : null}
          </div>
        ) : null}
        <div className="modal-body" data-testid="live-activity-body">
          {streamError ? (
            <div className="table-error">
        {rich("Live stream error: {0}", streamError)}
      </div>
          ) : null}
          {entries.length === 0 && !streamError ? (
            <div className="loading">
              <p>{tr("Waiting for activity…")}</p>
            </div>
          ) : (
            <div className="log-session" aria-live="polite">
              {entries.map((e) => e.kind === "message" ? (
                <div key={e.key} className="live-activity-message" data-testid="live-assistant-message">
                  {e.text}
                </div>
              ) : (
                // The same step card the finished transcript uses, so a live
                // run and its stored log read alike.
                <ToolStepCard
                  key={e.key}
                  testId="live-tool-entry"
                  open={openEntries.has(e.key)}
                  onToggle={toggleEntry}
                  tool={{
                    key: e.key,
                    step: toolSteps.get(e.key) ?? 0,
                    name: e.name ?? "tool",
                    argsRaw: e.text,
                    resultRaw: e.pending ? undefined : (e.result ?? ""),
                    isError: !!e.isError,
                    pending: e.pending,
                    repeats: e.repeats,
                  }}
                />
              ))}
              <div ref={bottomRef} />
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function LogViewerBody({
  task,
  onClose,
  readOnly,
  onResubmitted,
  onEdit,
  onSelectTask,
  onDelete,
}: {
  task: Task;
  onClose: () => void;
  readOnly: boolean;
  onResubmitted?: () => void;
  onEdit?: (task: Task) => void;
  onSelectTask?: (task: Task) => void;
  onDelete?: (task: Task) => void;
}) {
  // Same keyboard contract as LiveTaskView (Escape / Tab trap / focus return).
  const overlayRef = useRef<HTMLDivElement | null>(null);
  useDialogA11y(true, overlayRef, onClose);
  // The shared hook owns the cancelled-ref guard and the lone setState after
  // the await, so this component no longer needs its own one-shot load-flag
  // setState-in-effect disable.
  // Per-attempt history: attemptId null = the latest transcript (logs row);
  // a number = one superseded transcript (run_logs row). Switching refetches
  // through the same cancellable hook, so loading/error handling is shared.
  const [attemptId, setAttemptId] = useState<number | null>(null);
  // The transcript endpoints answer 404 for TWO different conditions with one
  // status and one message, deliberately: the task has never run, or the
  // caller may not read its transcript (log_authz.go hides the denied case
  // behind the same 404 so it cannot leak whether the task exists). The board
  // gates nothing per row, so a user without per-task read rights CAN open
  // this modal — which means the UI must not present either condition as a
  // failure ("Failed to load logs"), and must not try to tell them apart
  // client-side (that would reveal the denied case the server hides). A 404
  // therefore resolves to null — the empty state, worded to be true in both
  // cases — and every other status still throws and renders the real error.
  const absent404 = useCallback(
    <T,>(p: Promise<T>): Promise<T | null> =>
      p.catch((e: unknown) => {
        if (e instanceof OrchestratorError && e.status === 404) return null;
        throw e;
      }),
    [],
  );
  const {
    data: session,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(
      (signal: AbortSignal) =>
        absent404(
          attemptId === null
            ? orchestratorApi.taskLogs(task.id, signal)
            : orchestratorApi.taskLogHistoryEntry(task.id, attemptId, signal),
        ),
      [absent404, task.id, attemptId],
    ),
    [task.id, attemptId],
  );
  // The superseded-attempts list is metadata-only and cheap; most tasks have
  // none, in which case the picker never renders. Same 404 contract as the
  // transcript itself: never-run and not-readable are one response.
  const { data: attemptHistory } = useCancellableFetch(
    useCallback(
      (signal: AbortSignal) => absent404(orchestratorApi.taskLogHistory(task.id, signal)),
      [absent404, task.id],
    ),
    [task.id],
  );
  const attempts = attemptHistory?.entries ?? [];
  const { showToast } = useToast();
  const [resubmitting, setResubmitting] = useState(false);
  const messages = useMemo(() => session?.messages ?? [], [session]);

  // hasRun distinguishes the two shapes of the same action: a finished task is
  // RESUBMITTED, a not-yet-run one is kicked off NOW. Both post the same
  // one-off copy and leave the source (and its schedule) untouched; only the
  // wording differs, because "Resubmit" on a job that has never run reads as
  // though something went wrong.
  const hasRun = isTerminal(task.status);
  const actionLabel = hasRun ? tr("Resubmit") : tr("Run now");

  const resubmit = async () => {
    if (resubmitting) return;
    setResubmitting(true);
    try {
      const created = await orchestratorApi.rerunTask(task.id);
      showToast(
        hasRun
          ? tr("Resubmitted as task {0}… — running now", [created.id.slice(0, 8)])
          : tr("Started run {0}… — the task keeps its schedule", [created.id.slice(0, 8)]),
        "success",
      );
      onResubmitted?.();
    } catch (err) {
      showToast(
        tr("{0} failed: {1}", [actionLabel, errorMessage(err, tr("unknown error"))]),
        "error",
      );
    } finally {
      setResubmitting(false);
    }
  };

  // Download the stored session (plus the task row for context) as JSON — the
  // full fidelity record, not the filtered view.
  const download = () => {
    const payload = { task, session };
    const blob = new Blob([JSON.stringify(payload, null, 2)], {
      type: "application/json",
    });
    saveBlob(blob, `fleet-task-${task.id.slice(0, 8)}-logs.json`);
  };

  const canResubmit = !readOnly && isRunnable(task.status);
  const [historyOpen, setHistoryOpen] = useState(false);
  const [wakeNote, setWakeNote] = useState("");
  const [waking, setWaking] = useState(false);
  const canFireWake =
    !readOnly &&
    task.status === "paused_awaiting_wake" &&
    Boolean(task.wake_event_key);

  const fireWake = async () => {
    if (waking || !task.wake_event_key) return;
    setWaking(true);
    try {
      await orchestratorApi.wakeTask(task.id, task.wake_event_key, wakeNote.trim());
      showToast(tr("Fired “{0}” — task is pending again", [task.wake_event_key]), "success");
      onResubmitted?.();
    } catch (err) {
      showToast(
        tr("Fire event failed: {0}", [errorMessage(err, tr("unknown error"))]),
        "error",
      );
    } finally {
      setWaking(false);
    }
  };

  // "Discuss this run" (docs/DISCUSS-RUN.md): the BFF fetches this run's
  // transcript, creates a chat conversation seeded with a digest, and we
  // navigate there. Full page navigation (not router.push) because /chat is
  // a separate app shell.
  const [discussing, setDiscussing] = useState(false);
  const discuss = async () => {
    if (discussing) return;
    setDiscussing(true);
    try {
      const res = await fetch(
        `/api/orchestrator/tasks/${encodeURIComponent(task.id)}/discuss`,
        { method: "POST" },
      );
      if (!res.ok) {
        const body = (await res.json().catch(() => null)) as { error?: string } | null;
        throw new Error(body?.error ?? tr("HTTP {0}", [res.status]));
      }
      const { conversation_id } = (await res.json()) as { conversation_id: string | null };
      if (!conversation_id) throw new Error(tr("no conversation id returned"));
      window.location.href = `/chat?c=${encodeURIComponent(conversation_id)}`;
    } catch (err) {
      showToast(
        tr("Discuss failed: {0}", [errorMessage(err, tr("unknown error"))]),
        "error",
      );
      setDiscussing(false);
    }
  };

  return (
    <div
      ref={overlayRef}
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label={tr("Task Logs")}
    >
      <div className="modal modal-log">
        <div className="modal-header">
          {/* Lead with the operator's own title when the job has one; the
              full prompt stays in the tooltip either way. */}
          <h3 className="modal-log-title" title={task.prompt ?? ""}>
        {rich("Task: {0}", taskRunLabel(task, 90))}
      </h3>
          <CloseButton label={tr("Close modal")} onClick={onClose} />
        </div>
        <TaskSummary task={task} />
        {task.expected_duration_minutes ? <SLADetail task={task} /> : null}
        <div className="modal-body" data-testid="log-modal-body">
          <div className="task-detail-bar" data-testid="task-detail-bar">
            <span className="task-detail-status">
              {tr("Status:")}{" "}<TaskStatusBadge task={task} />
            </span>
            <span className="task-detail-actions">
              {attempts.length > 0 ? (
                <select
                  className="btn btn-secondary"
                  data-testid="attempt-picker"
                  aria-label={tr("Transcript attempt")}
                  value={attemptId ?? "latest"}
                  onChange={(e) =>
                    setAttemptId(
                      e.target.value === "latest" ? null : Number(e.target.value),
                    )
                  }
                >
                  <option value="latest">{tr("Latest transcript")}</option>
                  {attempts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {tr("Superseded")}{" "}{formatTimeFirst(a.superseded_at)}
                    </option>
                  ))}
                </select>
              ) : null}
              {onEdit && isRunnable(task.status) ? (
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="edit-task-button"
                  onClick={() => onEdit(task)}
                >
                  {tr("Edit")}
                </button>
              ) : null}
              {canResubmit ? (
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="resubmit-task-button"
                  disabled={resubmitting}
                  onClick={() => void resubmit()}
                >
                  {resubmitting ? (hasRun ? tr("Resubmitting…") : tr("Starting…")) : actionLabel}
                </button>
              ) : null}
              {/* Deleting is the only way to free a task's NAME: cancelling
                  keeps the row, and the name carries a unique index, so a
                  broken job blocked its own replacement. This modal is where
                  someone lands when a job has gone wrong, so it is where the
                  way out belongs. */}
              {onDelete ? (
                <button
                  type="button"
                  className="btn btn-danger"
                  data-testid="delete-task-button"
                  title={tr("Permanently delete this task and its run history")}
                  onClick={() => onDelete(task)}
                >
                  {tr("Delete")}
                </button>
              ) : null}
              {canFireWake ? (
                <span className="wake-fire" data-testid="wake-fire">
                  <input
                    type="text"
                    aria-label={tr("Wake note")}
                    placeholder={tr("optional note")}
                    value={wakeNote}
                    onChange={(e) => setWakeNote(e.target.value)}
                  />
                  <button
                    type="button"
                    className="btn btn-primary"
                    data-testid="fire-event-button"
                    disabled={waking}
                    onClick={() => void fireWake()}
                  >
                    {waking ? tr("Firing…") : tr("Fire “{0}”", [task.wake_event_key])}
                  </button>
                </span>
              ) : null}
              <button
                type="button"
                className="btn btn-secondary"
                data-testid="history-button"
                aria-expanded={historyOpen}
                onClick={() => setHistoryOpen((v) => !v)}
              >
                {tr("History")}
              </button>
              {session && session.messages && session.messages.length > 0 ? (
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="discuss-run-button"
                  disabled={discussing}
                  onClick={() => void discuss()}
                >
                  {discussing ? tr("Opening chat…") : tr("Discuss in chat")}
                </button>
              ) : null}
              <button
                type="button"
                className="btn btn-secondary"
                data-testid="download-logs-button"
                disabled={!session}
                onClick={download}
              >
                {tr("Download logs")}
              </button>
            </span>
          </div>
          {historyOpen ? (
            <TaskRunHistory task={task} onSelectTask={onSelectTask} />
          ) : null}
          {session ? <SessionMetrics session={session} /> : null}
          {session ? <RunCorrelation session={session} /> : null}
          <TaskRunIfBanner task={task} />
          {loading ? (
            <div className="loading">
              <p>{tr("Loading logs...")}</p>
            </div>
          ) : error ? (
            <div className="table-error">
        {rich("Failed to load logs: {0}", error)}
      </div>
          ) : !session || !session.messages || session.messages.length === 0 ? (
            // Worded to be true for BOTH conditions the server's one-status
            // 404 covers (see absent404 above): a task that has never run, and
            // one whose transcript this principal may not read. Do not split
            // the cases or name a cause here — distinguishing them client-side
            // would leak the denied case the server deliberately hides.
            <div className="table-empty">{tr("No transcript available for this task.")}</div>
          ) : (
            <TranscriptTimeline
              key={attemptId ?? "latest"}
              messages={messages}
              taskId={task.id}
            />
          )}
        </div>
      </div>
    </div>
  );
}

// TaskRunHistory lists the other runs of this task. Each recurring firing is a
// fresh row with name blanked and no source_task_id (see
// storage.scheduleNextRecurrence); the server stamps previous_occurrence_id
// for context-carry, but that is a single back-pointer, not a queryable
// chain, so runs are grouped the same way the SLA report buckets them: exact
// prompt equality. The server-side q= search narrows by prompt substring; the exact
// match is enforced client-side (ILIKE wildcards in a prompt can over-match,
// never under-match).
function TaskRunHistory({
  task,
  onSelectTask,
}: {
  task: Task;
  onSelectTask?: (task: Task) => void;
}) {
  const prompt = (task.prompt ?? "").trim();
  const {
    data,
    loading,
    error,
  } = useCancellableFetch(
    useCallback((signal: AbortSignal) => {
      const p = new URLSearchParams();
      // The first 120 chars keep the query URL sane for very long prompts;
      // the exact-equality filter below does the real matching.
      p.set("q", prompt.slice(0, 120));
      p.set("limit", "50");
      // orchestratorApi.tasks prepends the "?" itself.
      return orchestratorApi.tasks(p.toString(), signal);
    }, [prompt]),
    [prompt],
  );

  const runs = (data?.data ?? [])
    .filter((t) => (t.prompt ?? "").trim() === prompt)
    .sort(
      (a, b) =>
        new Date(b.created_at ?? 0).getTime() - new Date(a.created_at ?? 0).getTime(),
    );

  return (
    <div className="task-history" data-testid="task-history">
      <div className="task-history-title">
        {tr("Run history")}{" "}<span className="task-history-hint">{tr("(same task prompt)")}</span>
      </div>
      {loading ? (
        <div className="loading">
          <p>{tr("Loading history…")}</p>
        </div>
      ) : error ? (
        <div className="table-error">
        {rich("Failed to load history: {0}", error)}
      </div>
      ) : runs.length <= 1 ? (
        <div className="task-history-empty">{tr("No other runs of this task yet.")}</div>
      ) : (
        <ul className="task-history-list">
          {runs.map((run) => {
            const current = run.id === task.id;
            return (
              <li key={run.id}>
                <button
                  type="button"
                  className={`task-history-row${current ? " task-history-row--current" : ""}`}
                  data-testid="task-history-row"
                  disabled={current || !onSelectTask}
                  onClick={() => onSelectTask?.(run)}
                >
                  <span className="task-history-time">
                    {formatTimeFirst(run.created_at)}
                  </span>
                  <TaskStatusBadge task={run} />
                  <TaskSlaBadge task={run} />
                  <code className="task-history-id">{run.id.slice(0, 8)}</code>
                  {current ? (
                    <span className="task-history-current">{tr("viewing")}</span>
                  ) : (
                    <span className="task-history-open">{tr("view →")}</span>
                  )}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

// SLADetail renders the task's actual vs. expected duration as a labeled
// progress bar (#274). Shown in the log modal header only when the task has an
// expected_duration_minutes. A breach (actual > expected * fail multiplier, or
// the latched sla_breached flag) turns the bar red; a warn crossing (actual >
// expected * warn multiplier) turns it amber; otherwise it stays green. The bar
// caps at 200% so a 10x overrun doesn't overflow the modal.
function SLADetail({ task }: { task: Task }) {
  const expected = task.expected_duration_minutes ?? 0;
  const actualSecs = task.actual_duration_seconds ?? 0;
  const actualMin = actualSecs / 60;
  const warnMul = task.sla_warn_multiplier || 1.5;
  const failMul = task.sla_fail_multiplier || 2.0;
  const warnMin = expected * warnMul;
  const failMin = expected * failMul;

  let tone = "ok";
  if (task.sla_breached || (expected > 0 && actualMin >= failMin))
    tone = "fail";
  else if (expected > 0 && actualMin >= warnMin) tone = "warn";

  // Bar width: 100% == actual === expected. Cap at 200% so a runaway task
  // doesn't blow out the modal layout; the numeric label still shows the truth.
  const pct = expected > 0 ? Math.min((actualMin / expected) * 100, 200) : 0;
  const label =
    actualSecs > 0
      ? tr("{0}m / {1}m expected", [formatFixed(actualMin, 1), expected])
      : tr("{0}m expected (not yet complete)", [expected]);

  return (
    <div className="sla-detail" data-testid="sla-detail" data-sla-tone={tone}>
      <div className="sla-detail-label">
        <span>{tr("SLA")}</span>
        <span>{label}</span>
      </div>
      <div
        className="sla-progress"
        role="progressbar"
        aria-valuenow={Math.round(pct)}
        aria-valuemin={0}
        aria-valuemax={200}
        aria-label={tr("Actual vs expected duration")}
      >
        <div
          className={`sla-progress-bar sla-progress-${tone}`}
          style={{ width: `${pct}%` }}
        />
        <div
          className="sla-progress-mark sla-progress-mark-warn"
          style={{ left: `${Math.min(warnMul * 100, 200)}%` }}
          title={tr("warn @ {0}×", [warnMul])}
        />
        <div
          className="sla-progress-mark sla-progress-mark-fail"
          style={{ left: `${Math.min(failMul * 100, 200)}%` }}
          title={tr("fail @ {0}×", [failMul])}
        />
      </div>
    </div>
  );
}

// TaskRunIfBanner renders the optional pre-run shell gate (#269) as a read-only
// code block + a collapsible skip badge. Shown at the top of the log modal so
// an operator sees the gate that gates this task and its recent skip history at
// a glance. Renders nothing when the task has no run_if.
function TaskRunIfBanner({ task }: { task: Task }) {
  const [expanded, setExpanded] = useState(false);
  if (!task.run_if) return null;
  const exitCode = task.run_if.exit_code_is ?? 0;
  const timeout = task.run_if.timeout_seconds ?? 30;
  const onError = task.run_if.on_error ?? "run";
  const skipped = (task.skip_count ?? 0) > 0;
  return (
    <div className="task-run-if-banner" data-testid="task-run-if-banner">
      <div className="task-run-if-banner__header">
        <span className="task-run-if-banner__title">{tr("Pre-run gate")}</span>
        <code className="task-run-if-banner__command">
          {task.run_if.command}
        </code>
        <span className="task-run-if-banner__meta">
          {tr("Exit code: {0} · Timeout: {1}s · On error: {2}", [exitCode, timeout, onError === "skip" ? tr("Skip") : tr("Run")])}
        </span>
        {skipped ? (
          <button
            type="button"
            className="task-run-if-banner__skip-toggle"
            aria-expanded={expanded}
            aria-controls="task-run-if-skip-detail"
            onClick={() => setExpanded((v) => !v)}
          >
        {rich("Skipped {0}×{1}", task.skip_count, expanded ? " ▾" : " ▸")}
      </button>
        ) : null}
      </div>
      {skipped && expanded ? (
        <div
          id="task-run-if-skip-detail"
          className="task-run-if-banner__skip-detail"
        >
          <div>
        {rich("Last skip: {0}", task.last_skip_at
              ? new Date(task.last_skip_at).toLocaleString(getLanguage())
              : "—")}
      </div>
          <div>
        {rich("Reason: {0}", task.last_skip_reason ?? "—")}
      </div>
        </div>
      ) : null}
    </div>
  );
}

export default LogViewer;
