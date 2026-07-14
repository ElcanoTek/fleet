"use client";

import {
  lazy,
  Suspense,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import type {
  LogMessage,
  LogSession,
  Task,
  TaskStreamFrame,
  TaskLearnedInstruction,
} from "@/app/shared/lib/orchestratorApi";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { formatTimeFirst, stripAnsiCodes } from "@/app/shared/lib/format";
import { useToast } from "@/app/shared/ui/Toast";
import { createdByLabel, scheduleLabel, TaskSlaBadge } from "./taskDisplay";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import {
  Checklist,
  parseTaskTrackerOutput,
  type ChecklistState,
} from "./Checklist";

// The ReactMarkdown pipeline (micromark + remark-gfm, ~43 KiB transfer) and
// the workspace img/a rewrites live in LogMarkdown, lazy-loaded so the
// initial /orchestrator bundle doesn't pay for them — the modal is the only
// consumer and it opens on user action. Until the chunk arrives the log shows
// raw text with preserved whitespace, then upgrades in place. Same split as
// chat's AssistantContent (#757).
const LogMarkdown = lazy(() => import("./LogMarkdown"));

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
  // Called after a successful Resubmit so the dashboard can refetch and show
  // the new run immediately.
  onResubmitted?: () => void;
  // Opens the edit form for this task (parent closes the log modal first).
  onEdit?: (task: Task) => void;
  // Swaps the modal to another task (the History panel's row click).
  onSelectTask?: (task: Task) => void;
};

export function LogViewer({ task, onClose, canStop, onResubmitted, onEdit, onSelectTask }: LogViewerProps) {
  if (!task) return null;
  // Key the inner body on the task id so switching tasks remounts the fetch
  // hook — that reproduces the old "reset session to null then refetch on task
  // change" behavior cleanly, without a manual reset effect.
  const live = task.status === "running" || task.status === "assigned";
  if (live) {
    return (
      <LiveTaskView
        key={task.id}
        task={task}
        onClose={onClose}
        canStop={!!canStop}
      />
    );
  }
  return (
    <LogViewerBody
      key={task.id}
      task={task}
      onClose={onClose}
      canStop={!!canStop}
      onResubmitted={onResubmitted}
      onEdit={onEdit}
      onSelectTask={onSelectTask}
    />
  );
}

// Terminal statuses that make sense to resubmit as a fresh one-off run. A
// pending/scheduled task will run on its own; a running one already is.
const RESUBMITTABLE = new Set(["success", "error", "cancelled", "dead_lettered"]);

// TaskSummary mirrors the Recent Tasks row the user clicked (ID / Status /
// SLA / Schedule / Created By / Created) so the modal carries the row's
// context — which in turn lets the phone card view drop those columns.
function TaskSummary({ task }: { task: Task }) {
  const schedule = scheduleLabel(task);
  const items: Array<{ label: string; node: ReactNode }> = [
    {
      label: "ID",
      node: (
        <code title={task.id}>{task.id.slice(0, 8)}…</code>
      ),
    },
    {
      label: "Status",
      node: (
        <span className={`status-badge status-${task.status ?? "unknown"}`}>
          {task.status ?? "-"}
        </span>
      ),
    },
    { label: "SLA", node: <TaskSlaBadge task={task} /> },
    {
      label: "Schedule",
      node: (
        <span title={task.recurrence || undefined}>{schedule}</span>
      ),
    },
    { label: "Created by", node: <span>{createdByLabel(task)}</span> },
    { label: "Created", node: <span>{formatTimeFirst(task.created_at)}</span> },
  ];
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
    typeof n === "number" ? n.toLocaleString() : undefined;
  const tiles: Array<{ label: string; value: string | undefined }> = [
    { label: "Session", value: session.title || undefined },
    { label: "Messages", value: num(session.messages?.length) },
    { label: "Prompt tokens", value: num(session.prompt_tokens) },
    { label: "Completion tokens", value: num(session.completion_tokens) },
    { label: "Cached tokens", value: num(session.cached_tokens) },
    {
      label: "Cost",
      value:
        typeof session.cost === "number" && session.cost > 0
          ? `$${session.cost.toFixed(4)}`
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

// ── interaction filters ──────────────────────────────────────────────────────
// Chip taxonomy computed once per session: highlight kinds, tool names,
// models, providers. Selecting chips narrows the transcript to messages
// matching ANY selected chip (union); no selection shows everything.

type FilterChip = { key: string; label: string; count: number };
type FilterGroups = Array<{ title: string; chips: FilterChip[] }>;

function buildFilterIndex(messages: LogMessage[]): {
  tags: Array<Set<string>>;
  groups: FilterGroups;
  toolNames: Map<number, string>;
} {
  // Resolve a tool-result message's tool name through the assistant tool_call
  // it answers (LogMessage carries only tool_call_id).
  const callName = new Map<string, string>();
  for (const m of messages) {
    for (const tc of m.tool_calls ?? []) {
      if (tc.id && tc.name) callName.set(tc.id, tc.name);
    }
  }
  const toolNames = new Map<number, string>();
  const tags = messages.map((m, i) => {
    const t = new Set<string>();
    const role = m.role ?? "";
    if ((m.reasoning ?? "").trim()) t.add("hl:reasoning");
    if (role === "assistant" && (m.content ?? "").trim()) t.add("hl:responses");
    if ((m.tool_calls?.length ?? 0) > 0) t.add("hl:tool-calls");
    if (role === "tool") t.add("hl:tool-results");
    const names = new Set<string>();
    for (const tc of m.tool_calls ?? []) if (tc.name) names.add(tc.name);
    if (role === "tool" && m.tool_call_id) {
      const n = callName.get(m.tool_call_id);
      if (n) {
        names.add(n);
        toolNames.set(i, n);
      }
    }
    for (const n of names) {
      t.add(`tool:${n}`);
      if (n === "task_tracker") t.add("hl:task-tracker");
    }
    if (m.model) t.add(`model:${m.model}`);
    if (m.provider) t.add(`provider:${m.provider}`);
    return t;
  });

  const count = (key: string) => tags.filter((t) => t.has(key)).length;
  const collect = (prefix: string) => {
    const keys = new Set<string>();
    for (const t of tags)
      for (const k of t) if (k.startsWith(prefix)) keys.add(k);
    return [...keys]
      .sort()
      .map((k) => ({ key: k, label: k.slice(prefix.length), count: count(k) }));
  };
  const highlights: FilterChip[] = [
    { key: "hl:reasoning", label: "Reasoning", count: count("hl:reasoning") },
    { key: "hl:responses", label: "Responses", count: count("hl:responses") },
    { key: "hl:tool-calls", label: "Tool calls", count: count("hl:tool-calls") },
    { key: "hl:tool-results", label: "Tool results", count: count("hl:tool-results") },
    { key: "hl:task-tracker", label: "Task tracker", count: count("hl:task-tracker") },
  ].filter((c) => c.count > 0);
  const groups: FilterGroups = [
    { title: "Highlights", chips: highlights },
    { title: "Tool types", chips: collect("tool:") },
    { title: "Models", chips: collect("model:") },
    { title: "Providers", chips: collect("provider:") },
  ].filter((g) => g.chips.length > 0);
  return { tags, groups, toolNames };
}

function LogFilters({
  groups,
  selected,
  onToggle,
  onClear,
  shown,
  total,
}: {
  groups: FilterGroups;
  selected: Set<string>;
  onToggle: (key: string) => void;
  onClear: () => void;
  shown: number;
  total: number;
}) {
  if (groups.length === 0) return null;
  return (
    <div className="log-filters" data-testid="log-filters">
      <div className="log-filters-head">
        <span className="log-filters-title">Interaction filters</span>
        {selected.size > 0 ? (
          <button type="button" className="btn btn-small" onClick={onClear}>
            Clear all
          </button>
        ) : null}
      </div>
      {groups.map((g) => (
        <div key={g.title} className="log-filter-group">
          <span className="log-filter-group-title">{g.title}</span>
          <div className="log-filter-chips">
            {g.chips.map((c) => (
              <button
                key={c.key}
                type="button"
                className={`log-chip${selected.has(c.key) ? " log-chip--active" : ""}`}
                aria-pressed={selected.has(c.key)}
                onClick={() => onToggle(c.key)}
              >
                {c.label} <span className="log-chip-count">{c.count}</span>
              </button>
            ))}
          </div>
        </div>
      ))}
      <div className="log-filters-shown">
        {selected.size === 0
          ? `Showing all ${total} messages`
          : `Showing ${shown} of ${total} messages`}
      </div>
    </div>
  );
}

// LogMessageCard — one transcript entry, moc-style: a role-tinted header bar
// (avatar initial, role, model, timestamp) over the rendered content.
// Reasoning and tool arguments sit behind disclosures; tool OUTPUT renders as
// preformatted text (it is machine output, not markdown).
function LogMessageCard({
  msg,
  taskId,
  toolName,
}: {
  msg: LogMessage;
  taskId: string;
  toolName?: string;
}) {
  const role = msg.role ?? "unknown";
  const ts = msg.created_at
    ? new Date(msg.created_at * 1000).toLocaleTimeString()
    : null;
  const initial =
    role === "user" ? "U" : role === "assistant" ? "A" : role === "tool" ? "T" : "?";
  const roleLabel =
    role === "tool" && toolName ? `tool · ${toolName}` : role;
  const content = stripAnsiCodes(msg.content ?? "");
  return (
    <div className={`log-message log-message--${role}`}>
      <div className="log-message-head">
        <span className={`log-avatar log-avatar--${role}`} aria-hidden="true">
          {initial}
        </span>
        <span className="log-message-role">{roleLabel}</span>
        {msg.model ? <span className="log-message-model">{msg.model}</span> : null}
        {ts ? <span className="log-message-time">{ts}</span> : null}
      </div>
      <div className="log-message-content">
        {(msg.reasoning ?? "").trim() ? (
          <details className="log-reasoning">
            <summary>Reasoning</summary>
            <pre className="log-pre">{stripAnsiCodes(msg.reasoning ?? "")}</pre>
          </details>
        ) : null}
        {(msg.tool_calls?.length ?? 0) > 0 ? (
          <div className="log-tool-calls">
            {msg.tool_calls!.map((tc, i) => (
              <details key={tc.id ?? i} className="log-tool-call">
                <summary>{tc.name ?? "tool"}</summary>
                <pre className="log-pre">{tc.arguments ?? ""}</pre>
              </details>
            ))}
          </div>
        ) : null}
        {content ? (
          role === "tool" ? (
            content.length > 1200 ? (
              <details className="log-tool-output">
                <summary>
                  Tool output ({content.length.toLocaleString()} characters)
                </summary>
                <pre className="log-pre">{content}</pre>
              </details>
            ) : (
              <pre className="log-pre">{content}</pre>
            )
          ) : (
            <Suspense
              fallback={<div className="whitespace-pre-wrap">{content}</div>}
            >
              <LogMarkdown content={content} taskId={taskId} />
            </Suspense>
          )
        ) : null}
      </div>
    </div>
  );
}

// ── #508 live activity view ──────────────────────────────────────────────────

type ActivityEntry = {
  key: string;
  kind: "message" | "tool_call" | "tool_result";
  name?: string;
  text: string;
  isError?: boolean;
  // checklist is set for a task_tracker tool_result whose JSON parsed into a
  // plan (#518); the entry then renders as a checklist instead of raw text.
  checklist?: ChecklistState;
};

// Keep enough tool output for real debugging without letting a pathological
// result pin an unbounded amount of browser memory. Long entries render behind
// a disclosure below; the old 600-character clamp hid the useful error tail.
const clampText = (s: string, max = 20_000) =>
  s.length > max
    ? s.slice(0, max) + "\n… output capped at 20,000 characters"
    : s;

// LiveTaskView attaches to GET /tasks/{id}/stream and renders the run's
// tool-by-tool activity as it happens (#508): each tool call, its result, and
// the assistant's text, chronologically, with an optional Stop control that
// interrupts the governed run at its next checkpoint (with who-stopped-it
// attribution recorded server-side).
function LiveTaskView({
  task,
  onClose,
  canStop,
}: {
  task: Task;
  onClose: () => void;
  canStop: boolean;
}) {
  const [entries, setEntries] = useState<ActivityEntry[]>([]);
  // checklist holds the LATEST task_tracker plan (#518) for the live progress
  // panel; it updates each time the agent revises its plan mid-run.
  const [checklist, setChecklist] = useState<ChecklistState | null>(null);
  const [runStatus, setRunStatus] = useState<string>("running");
  const [stoppedBy, setStoppedBy] = useState<string | null>(null);
  const [stopping, setStopping] = useState(false);
  const [streamError, setStreamError] = useState<string | null>(null);
  const seq = useRef(0);
  const terminalRef = useRef(false);
  const lastEventID = useRef<string | undefined>(undefined);
  const bottomRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const ac = new AbortController();
    const onFrame = (frame: TaskStreamFrame) => {
      setStreamError(null);
      if (frame._event_id) lastEventID.current = frame._event_id;
      if (frame.type === "status") {
        if (frame.status && frame.status !== "running") {
          terminalRef.current = true;
          setRunStatus(frame.status);
          if (frame.stopped_by) setStoppedBy(frame.stopped_by);
        }
        return;
      }
      const key = `e${seq.current++}`;
      let entry: ActivityEntry | null = null;
      if (frame.type === "agent_message" && frame.content) {
        entry = { key, kind: "message", text: frame.content };
      } else if (frame.type === "tool_call") {
        entry = {
          key,
          kind: "tool_call",
          name: frame.name,
          text: clampText(frame.input ?? ""),
        };
      } else if (frame.type === "tool_result") {
        // A task_tracker result carries the structured plan as JSON; parse it
        // (raw, before clamping) into a checklist for the live progress panel and
        // a readable history entry, falling back to raw text if it doesn't parse.
        const parsedChecklist =
          frame.name === "task_tracker"
            ? parseTaskTrackerOutput(frame.output ?? "")
            : null;
        if (parsedChecklist) {
          setChecklist(parsedChecklist);
          entry = {
            key,
            kind: "tool_result",
            name: frame.name,
            text: "",
            checklist: parsedChecklist,
          };
        } else {
          entry = {
            key,
            kind: "tool_result",
            name: frame.name,
            text: clampText(frame.output ?? ""),
            isError: !!frame.error,
          };
        }
      }
      if (entry) {
        // Bound the DOM/history for extremely chatty runs while retaining the
        // most recent activity where failures and final output appear.
        setEntries((prev) => [...prev, entry!].slice(-1000));
      }
    };
    // fetch streams do not reconnect like EventSource. Keep reattaching with
    // Last-Event-ID until a terminal status arrives, so a proxy/mobile network
    // blip does not silently freeze the live Operations Center view.
    const pump = async () => {
      let failures = 0;
      while (!ac.signal.aborted && !terminalRef.current) {
        try {
          await orchestratorApi.streamTaskActivity(
            task.id,
            onFrame,
            ac.signal,
            lastEventID.current,
          );
          failures = 0;
          if (!terminalRef.current)
            await new Promise((resolve) => window.setTimeout(resolve, 500));
        } catch (err) {
          if (ac.signal.aborted) return;
          failures += 1;
          setStreamError(
            `${err instanceof Error ? err.message : "stream failed"}; reconnecting…`,
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
      setStreamError(err instanceof Error ? err.message : "stop failed");
      setStopping(false);
    }
  };

  const terminal = runStatus !== "running";
  return (
    <div
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label="Live task activity"
    >
      <div className="modal modal-log">
        <div className="modal-header">
          <h3 className="modal-log-title" title={task.prompt ?? ""}>
            Live activity
            <span
              className={`status-badge status-${terminal ? (runStatus === "succeeded" ? "success" : runStatus === "stopped" ? "cancelled" : "error") : "running"}`}
              style={{ marginLeft: 8 }}
              data-testid="live-run-status"
            >
              {runStatus}
              {stoppedBy ? ` by ${stoppedBy}` : ""}
            </span>
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
                {stopping ? "Stopping…" : "Stop run"}
              </button>
            ) : null}
            <button
              type="button"
              className="icon-action modal-close"
              aria-label="Close modal"
              onClick={onClose}
            >
              ×
            </button>
          </div>
        </div>
        <TaskSummary task={task} />
        <div className="modal-body" data-testid="live-activity-body">
          {streamError ? (
            <div className="table-error">Live stream error: {streamError}</div>
          ) : null}
          {checklist ? (
            <div className="live-checklist-panel">
              <Checklist state={checklist} live />
            </div>
          ) : null}
          {entries.length === 0 && !streamError ? (
            <div className="loading">
              <p>Waiting for activity…</p>
            </div>
          ) : (
            <div className="log-session" aria-live="polite">
              {entries.map((e) => (
                <div
                  key={e.key}
                  className={`log-message log-message--${e.kind === "message" ? "assistant" : "tool"}`}
                >
                  <div className="log-message-head">
                    <span className="log-message-role">
                      {e.kind === "tool_call"
                        ? `▶ ${e.name ?? "tool"}`
                        : e.kind === "tool_result"
                          ? `${e.isError ? "✗" : "✓"} ${e.name ?? "result"}`
                          : "assistant"}
                    </span>
                  </div>
                  <div className="log-message-content">
                    {e.checklist ? (
                      <Checklist state={e.checklist} />
                    ) : e.kind !== "message" && e.text.length > 1200 ? (
                      <details>
                        <summary style={{ cursor: "pointer" }}>
                          Show tool details ({e.text.length.toLocaleString()}{" "}
                          characters)
                        </summary>
                        <pre
                          style={{
                            whiteSpace: "pre-wrap",
                            margin: "0.5rem 0 0",
                          }}
                        >
                          {e.text}
                        </pre>
                      </details>
                    ) : (
                      <pre
                        style={{
                          whiteSpace: "pre-wrap",
                          margin: 0,
                          fontFamily:
                            e.kind === "message" ? "inherit" : undefined,
                        }}
                      >
                        {e.text}
                      </pre>
                    )}
                  </div>
                </div>
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
  canStop,
  onResubmitted,
  onEdit,
  onSelectTask,
}: {
  task: Task;
  onClose: () => void;
  canStop: boolean;
  onResubmitted?: () => void;
  onEdit?: (task: Task) => void;
  onSelectTask?: (task: Task) => void;
}) {
  // The shared hook owns the cancelled-ref guard and the lone setState after
  // the await, so this component no longer needs its own one-shot load-flag
  // setState-in-effect disable.
  const {
    data: session,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(() => orchestratorApi.taskLogs(task.id), [task.id]),
    [task.id],
  );
  const { showToast } = useToast();
  const [resubmitting, setResubmitting] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const messages = useMemo(() => session?.messages ?? [], [session]);
  const { tags, groups, toolNames } = useMemo(
    () => buildFilterIndex(messages),
    [messages],
  );
  const visibleIdx = useMemo(
    () =>
      messages
        .map((_, i) => i)
        .filter(
          (i) =>
            selected.size === 0 || [...selected].some((k) => tags[i].has(k)),
        ),
    [messages, tags, selected],
  );

  const toggleChip = (key: string) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  const resubmit = async () => {
    if (resubmitting) return;
    setResubmitting(true);
    try {
      const created = await orchestratorApi.rerunTask(task.id);
      showToast(
        `Resubmitted as task ${created.id.slice(0, 8)}… — running now`,
        "success",
      );
      onResubmitted?.();
    } catch (err) {
      showToast(
        `Resubmit failed: ${err instanceof Error ? err.message : "unknown error"}`,
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
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `fleet-task-${task.id.slice(0, 8)}-logs.json`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  const canResubmit = RESUBMITTABLE.has(task.status ?? "");
  const [historyOpen, setHistoryOpen] = useState(false);

  return (
    <div
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label="Task Logs"
    >
      <div className="modal modal-log">
        <div className="modal-header">
          <h3 className="modal-log-title" title={task.prompt ?? ""}>
            Task: {(task.prompt ?? "").trim().slice(0, 90) || task.id.slice(0, 8)}
            {(task.prompt ?? "").trim().length > 90 ? "…" : ""}
          </h3>
          <button
            type="button"
            className="icon-action modal-close"
            aria-label="Close modal"
            onClick={onClose}
          >
            ×
          </button>
        </div>
        <TaskSummary task={task} />
        {task.expected_duration_minutes ? <SLADetail task={task} /> : null}
        <div className="modal-body" data-testid="log-modal-body">
          <div className="task-detail-bar" data-testid="task-detail-bar">
            <span className="task-detail-status">
              Status:{" "}
              <span className={`status-badge status-${task.status ?? "unknown"}`}>
                {task.status ?? "-"}
              </span>
            </span>
            <span className="task-detail-actions">
              {onEdit && ["pending", "scheduled", ...RESUBMITTABLE].includes(task.status ?? "") ? (
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="edit-task-button"
                  onClick={() => onEdit(task)}
                >
                  Edit
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
                  {resubmitting ? "Resubmitting…" : "Resubmit"}
                </button>
              ) : null}
              <button
                type="button"
                className="btn btn-secondary"
                data-testid="history-button"
                aria-expanded={historyOpen}
                onClick={() => setHistoryOpen((v) => !v)}
              >
                History
              </button>
              <button
                type="button"
                className="btn btn-secondary"
                data-testid="download-logs-button"
                disabled={!session}
                onClick={download}
              >
                Download logs
              </button>
            </span>
          </div>
          {historyOpen ? (
            <TaskRunHistory task={task} onSelectTask={onSelectTask} />
          ) : null}
          {session ? <SessionMetrics session={session} /> : null}
          <SelfImprovePanel task={task} canManage={canStop} />
          <TaskRunIfBanner task={task} />
          {loading ? (
            <div className="loading">
              <p>Loading logs...</p>
            </div>
          ) : error ? (
            <div className="table-error">Failed to load logs: {error}</div>
          ) : !session || !session.messages || session.messages.length === 0 ? (
            <div className="table-empty">No logs for this task.</div>
          ) : (
            <>
              <LogFilters
                groups={groups}
                selected={selected}
                onToggle={toggleChip}
                onClear={() => setSelected(new Set())}
                shown={visibleIdx.length}
                total={messages.length}
              />
              <div className="log-session">
                {visibleIdx.map((i) => (
                  <LogMessageCard
                    key={messages[i].id ?? i}
                    msg={messages[i]}
                    taskId={task.id}
                    toolName={toolNames.get(i)}
                  />
                ))}
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// TaskRunHistory lists the other runs of this task. Fleet has no first-class
// lineage for recurring occurrences (each firing is a fresh row with name
// blanked and no source_task_id — see storage.scheduleNextRecurrence), so
// runs are grouped the same way the SLA report buckets them: exact prompt
// equality. The server-side q= search narrows by prompt substring; the exact
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
    useCallback(() => {
      const p = new URLSearchParams();
      // The first 120 chars keep the query URL sane for very long prompts;
      // the exact-equality filter below does the real matching.
      p.set("q", prompt.slice(0, 120));
      p.set("limit", "50");
      // orchestratorApi.tasks prepends the "?" itself.
      return orchestratorApi.tasks(p.toString());
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
        Run history <span className="task-history-hint">(same task prompt)</span>
      </div>
      {loading ? (
        <div className="loading">
          <p>Loading history…</p>
        </div>
      ) : error ? (
        <div className="table-error">Failed to load history: {error}</div>
      ) : runs.length <= 1 ? (
        <div className="task-history-empty">No other runs of this task yet.</div>
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
                  <span className={`status-badge status-${run.status ?? "unknown"}`}>
                    {run.status ?? "-"}
                  </span>
                  <TaskSlaBadge task={run} />
                  <code className="task-history-id">{run.id.slice(0, 8)}</code>
                  {current ? (
                    <span className="task-history-current">viewing</span>
                  ) : (
                    <span className="task-history-open">view →</span>
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
      ? `${actualMin.toFixed(1)}m / ${expected}m expected`
      : `${expected}m expected (not yet complete)`;

  return (
    <div className="sla-detail" data-testid="sla-detail" data-sla-tone={tone}>
      <div className="sla-detail-label">
        <span>SLA</span>
        <span>{label}</span>
      </div>
      <div
        className="sla-progress"
        role="progressbar"
        aria-valuenow={Math.round(pct)}
        aria-valuemin={0}
        aria-valuemax={200}
        aria-label="Actual vs expected duration"
      >
        <div
          className={`sla-progress-bar sla-progress-${tone}`}
          style={{ width: `${pct}%` }}
        />
        <div
          className="sla-progress-mark sla-progress-mark-warn"
          style={{ left: `${Math.min(warnMul * 100, 200)}%` }}
          title={`warn @ ${warnMul}×`}
        />
        <div
          className="sla-progress-mark sla-progress-mark-fail"
          style={{ left: `${Math.min(failMul * 100, 200)}%` }}
          title={`fail @ ${failMul}×`}
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
        <span className="task-run-if-banner__title">Pre-run gate</span>
        <code className="task-run-if-banner__command">
          {task.run_if.command}
        </code>
        <span className="task-run-if-banner__meta">
          exit={exitCode} · timeout={timeout}s · on_error={onError}
        </span>
        {skipped ? (
          <button
            type="button"
            className="task-run-if-banner__skip-toggle"
            aria-expanded={expanded}
            aria-controls="task-run-if-skip-detail"
            onClick={() => setExpanded((v) => !v)}
          >
            Skipped {task.skip_count}×{expanded ? " ▾" : " ▸"}
          </button>
        ) : null}
      </div>
      {skipped && expanded ? (
        <div
          id="task-run-if-skip-detail"
          className="task-run-if-banner__skip-detail"
        >
          <div>
            Last skip:{" "}
            {task.last_skip_at
              ? new Date(task.last_skip_at).toLocaleString()
              : "—"}
          </div>
          <div>Reason: {task.last_skip_reason ?? "—"}</div>
        </div>
      ) : null}
    </div>
  );
}

// SelfImprovePanel (#516): thumbs up/down + critique on a finished task's
// output, and the versioned learned instructions distilled from that feedback
// — activate a version, or revert (deactivate). Operators (canManage) see the
// activate/revert controls; anyone who can view can leave feedback.
function SelfImprovePanel({
  task,
  canManage,
}: {
  task: Task;
  canManage: boolean;
}) {
  const [instructions, setInstructions] = useState<TaskLearnedInstruction[]>(
    [],
  );
  const [critique, setCritique] = useState("");
  const [note, setNote] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [open, setOpen] = useState(false);

  const reload = useCallback(async () => {
    try {
      const res = await orchestratorApi.learnedInstructions(task.id);
      setInstructions(res.learned_instructions ?? []);
    } catch {
      setInstructions([]);
    }
  }, [task.id]);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void reload();
    });
    return () => {
      cancelled = true;
    };
  }, [open, reload]);

  const feedback = async (rating: "up" | "down") => {
    if (busy) return;
    setBusy(true);
    setNote(null);
    try {
      await orchestratorApi.submitFeedback(
        task.id,
        rating,
        rating === "down" ? critique : "",
      );
      setCritique("");
      setNote(
        rating === "up"
          ? "Thanks — recorded."
          : "Recorded. Enough down-votes distills a learned instruction to review.",
      );
      if (open) await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const activate = async (version: number) => {
    setBusy(true);
    try {
      await orchestratorApi.activateLearnedInstruction(task.id, version);
      await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  const deactivate = async () => {
    setBusy(true);
    try {
      await orchestratorApi.deactivateLearnedInstruction(task.id);
      await reload();
    } catch (err) {
      setNote(`Failed: ${(err as Error).message}`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div
      className="self-improve-panel"
      data-testid="self-improve-panel"
      style={{
        borderBottom: "1px solid var(--color-border)",
        paddingBottom: "0.75rem",
        marginBottom: "0.75rem",
      }}
    >
      <div
        style={{
          display: "flex",
          alignItems: "center",
          gap: "0.5rem",
          flexWrap: "wrap",
        }}
      >
        <span
          style={{
            fontSize: "0.8125rem",
            color: "var(--color-text-secondary)",
          }}
        >
          Was this run useful?
        </span>
        <button
          type="button"
          className="btn btn-small"
          disabled={busy}
          onClick={() => void feedback("up")}
        >
          👍
        </button>
        <button
          type="button"
          className="btn btn-small"
          disabled={busy}
          onClick={() => void feedback("down")}
        >
          👎
        </button>
        <input
          aria-label="Critique (optional)"
          placeholder="what to improve (optional)"
          value={critique}
          onChange={(e) => setCritique(e.target.value)}
          style={{
            flex: "1 1 12rem",
            minWidth: 0,
            fontSize: "0.8rem",
            padding: "0.25rem 0.5rem",
            borderRadius: "0.5rem",
            border: "1px solid var(--color-border-strong)",
            background: "transparent",
            color: "var(--color-text-primary)",
          }}
        />
        <button
          type="button"
          className="btn btn-small"
          onClick={() => setOpen((v) => !v)}
        >
          {open ? "Hide" : "Learned instructions"}
        </button>
      </div>
      {note ? (
        <div
          style={{
            marginTop: "0.4rem",
            fontSize: "0.75rem",
            color: "var(--color-text-muted)",
          }}
        >
          {note}
        </div>
      ) : null}
      {open ? (
        instructions.length === 0 ? (
          <div
            style={{
              marginTop: "0.5rem",
              fontSize: "0.78rem",
              color: "var(--color-text-muted)",
            }}
          >
            No learned instructions yet. Down-votes with critique distill into a
            reviewable instruction once self-improvement is enabled.
          </div>
        ) : (
          <ul
            style={{
              marginTop: "0.5rem",
              display: "grid",
              gap: "0.35rem",
              listStyle: "none",
              padding: 0,
            }}
          >
            {instructions.map((li) => (
              <li
                key={li.id}
                style={{
                  fontSize: "0.78rem",
                  color: "var(--color-text-secondary)",
                  display: "flex",
                  gap: "0.5rem",
                  alignItems: "flex-start",
                  justifyContent: "space-between",
                }}
              >
                <span>
                  <span
                    className={`status-badge status-${li.status === "active" ? "success" : li.status === "proposed" ? "running" : "cancelled"}`}
                    style={{ marginRight: 6 }}
                  >
                    v{li.version} {li.status}
                  </span>
                  {li.content}
                </span>
                {canManage ? (
                  <span style={{ whiteSpace: "nowrap" }}>
                    {li.status !== "active" ? (
                      <button
                        type="button"
                        className="btn btn-small"
                        disabled={busy}
                        onClick={() => void activate(li.version)}
                      >
                        Activate
                      </button>
                    ) : (
                      <button
                        type="button"
                        className="btn btn-small"
                        disabled={busy}
                        onClick={() => void deactivate()}
                      >
                        Deactivate
                      </button>
                    )}
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        )
      ) : null}
    </div>
  );
}

export default LogViewer;
