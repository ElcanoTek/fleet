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
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { useToast } from "@/app/shared/ui/Toast";
import { createdByLabel, scheduleLabel, taskRunLabel, TaskSlaBadge } from "./taskDisplay";
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
  // Permanently delete this task. The parent owns the confirm + API call and
  // closes the modal, so this component stays presentational (same contract as
  // onEdit). Omitted = no affordance.
  onDelete?: (task: Task) => void;
};

export function LogViewer({ task, onClose, canStop, onResubmitted, onEdit, onSelectTask, onDelete }: LogViewerProps) {
  if (!task) return null;
  // Key the inner body on the task id so switching tasks remounts the fetch
  // hook — that reproduces the old "reset session to null then refetch on task
  // change" behavior cleanly, without a manual reset effect.
  const live = task.status === "running" || task.status === "leased";
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
      onDelete={onDelete}
    />
  );
}

// Terminal statuses that make sense to resubmit as a fresh one-off run.
const RESUBMITTABLE = new Set(["success", "error", "cancelled", "dead_lettered"]);

// Statuses that can be kicked off on demand: the terminal ones (resubmit) plus
// the not-yet-run ones (run now). A pending/scheduled task does run on its own
// eventually — but "eventually" is the next cron tick, which is why a freshly
// created daily job could not be exercised until the following day. Only an
// in-flight task is excluded; it is already running.
const RUNNABLE = new Set([...RESUBMITTABLE, "pending", "scheduled"]);

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
  if (task.status === "paused_awaiting_wake") {
    items.push({
      label: "Wake",
      node: (
        <span data-testid="wake-summary">
          {task.wake_event_key ? `event “${task.wake_event_key}”` : "timer"}
          {task.wake_at ? ` · by ${formatTimeFirst(task.wake_at)}` : ""}
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

function deferredToolDisplay(name: string | undefined, input: string | undefined) {
  const fallback = { name: name || "tool", input: input ?? "" };
  if (name !== "tool_call" || !input) return fallback;
  try {
    const envelope = JSON.parse(input) as { name?: unknown; arguments?: unknown };
    if (typeof envelope.name !== "string" || !envelope.name) return fallback;
    let args = envelope.arguments;
    // Calls emitted from the old incorrect schema wrapped the argument object
    // in a singleton array. Display the nested call as it will be repaired by
    // the server during a rolling deployment.
    if (Array.isArray(args) && args.length === 1 && args[0] && typeof args[0] === "object") {
      args = args[0];
    }
    return { name: envelope.name, input: JSON.stringify(args ?? {}, null, 2) };
  } catch {
    return fallback;
  }
}

function readableToolError(raw: string) {
  if (/cannot unmarshal array into Go value of type map\[string\]interface/i.test(raw)) {
    return "Invalid tool arguments: Fleet expected a JSON object but received an array.";
  }
  return raw;
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
      if (tc.id && tc.name) {
        callName.set(tc.id, deferredToolDisplay(tc.name, tc.arguments).name);
      }
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
    for (const tc of m.tool_calls ?? []) {
      if (tc.name) names.add(deferredToolDisplay(tc.name, tc.arguments).name);
    }
    if (role === "tool") {
      const n = (m.tool_call_id ? callName.get(m.tool_call_id) : undefined) || m.tool_name;
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
            {msg.tool_calls!.map((tc, i) => {
              const display = deferredToolDisplay(tc.name, tc.arguments);
              return (
                <details key={tc.id ?? i} className="log-tool-call" data-testid="stored-tool-call">
                  <summary>{display.name}</summary>
                  <pre className="log-pre">{display.input}</pre>
                </details>
              );
            })}
          </div>
        ) : null}
        {content ? (
          role === "tool" ? (
            msg.is_error ? (
              <p className="log-tool-error" data-testid="stored-tool-result">
                {readableToolError(content)}
              </p>
            ) : content.length > 1200 ? (
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

// ── #1043 sub-agent child cards ──────────────────────────────────────────────
// A spawn is productized as a card (id, role, status, spend, workdir) instead
// of a raw JSON blob. Two sources feed it: the parent log's `subagent_spawned`
// linkage entries (stored transcript) and the spawn_subagent tool call/result
// frames (live stream).

type SubagentInfo = {
  childSessionId?: string;
  role?: string;
  workdir?: string;
  costUsd?: number;
  tokens?: number;
  success?: boolean;
  result?: string;
  pending?: boolean;
  /** Live step count + current action while the child is still running. */
  steps?: number;
  current?: string;
};

// parseSubagentPayload reads either shape — the linkage entry
// {child_session_id, role, workdir, cost_usd, tokens, success} or the tool
// result {…same…, result} — returning null on unparseable content.
function parseSubagentPayload(raw: string | undefined): SubagentInfo | null {
  if (!raw) return null;
  try {
    const p = JSON.parse(raw) as Record<string, unknown>;
    if (typeof p !== "object" || p === null) return null;
    return {
      childSessionId:
        typeof p.child_session_id === "string" ? p.child_session_id : undefined,
      role: typeof p.role === "string" ? p.role : undefined,
      workdir: typeof p.workdir === "string" ? p.workdir : undefined,
      costUsd: typeof p.cost_usd === "number" ? p.cost_usd : undefined,
      tokens: typeof p.tokens === "number" ? p.tokens : undefined,
      success: typeof p.success === "boolean" ? p.success : undefined,
      result: typeof p.result === "string" ? p.result : undefined,
    };
  } catch {
    return null;
  }
}

function subagentStatus(info: SubagentInfo): {
  label: string;
  tone: "running" | "done" | "failed";
} {
  if (info.pending) return { label: "running", tone: "running" };
  if (info.success) return { label: "done", tone: "done" };
  // A refusal never built a child, so it has no session id.
  if (!info.childSessionId) return { label: "refused", tone: "failed" };
  if ((info.result ?? "").includes("timed out")) return { label: "timed out", tone: "failed" };
  return { label: "failed", tone: "failed" };
}

function SubagentCard({ info, taskId }: { info: SubagentInfo; taskId?: string }) {
  const status = subagentStatus(info);
  const shortId = info.childSessionId
    ? info.childSessionId.replace(/^subagent-/, "").slice(0, 8)
    : null;
  return (
    <article
      className={`subagent-card subagent-card--${status.tone}`}
      data-testid="subagent-card"
    >
      <div className="subagent-card-head">
        <span className="subagent-card-title">
          Sub-agent{shortId ? <code> {shortId}…</code> : null}
        </span>
        {info.role ? <span className="subagent-card-role">{info.role}</span> : null}
        <span className={`subagent-card-status subagent-card-status--${status.tone}`}>
          {status.label}
        </span>
      </div>
      {/* What the child is doing right now — the live half of the card. */}
      {info.current ? (
        <div className="subagent-card-activity" data-testid="subagent-card-activity">
          {info.current}
        </div>
      ) : null}
      <div className="subagent-card-meta">
        {typeof info.steps === "number" && info.steps > 0 ? (
          <span>{info.steps} steps</span>
        ) : null}
        {typeof info.costUsd === "number" && info.costUsd > 0 ? (
          <span>${info.costUsd.toFixed(4)}</span>
        ) : null}
        {typeof info.tokens === "number" && info.tokens > 0 ? (
          <span>{info.tokens.toLocaleString()} tokens</span>
        ) : null}
        {info.workdir ? (
          <code className="subagent-card-workdir" title={info.workdir}>
            {info.workdir}
          </code>
        ) : null}
      </div>
      {info.result ? (
        <details className="subagent-card-result">
          <summary>Result</summary>
          <pre className="log-pre">{stripAnsiCodes(info.result)}</pre>
        </details>
      ) : null}
      {taskId && info.childSessionId && !info.pending ? (
        <SubagentTranscript taskId={taskId} childSessionId={info.childSessionId} />
      ) : null}
    </article>
  );
}

// SubagentTranscript lazy-loads a child's own session log (#1043) the first
// time its disclosure opens and renders it with the same message cards the
// parent transcript uses. The transcript file is best-effort (it lives on the
// serving host, not in the DB) — a 404 renders as an inline note, never an
// error state for the whole card.
function SubagentTranscript({
  taskId,
  childSessionId,
}: {
  taskId: string;
  childSessionId: string;
}) {
  const [state, setState] = useState<"idle" | "loading" | "error" | "loaded">("idle");
  const [error, setError] = useState("");
  const [session, setSession] = useState<LogSession | null>(null);
  const load = useCallback(async () => {
    setState((prev) => (prev === "idle" || prev === "error" ? "loading" : prev));
    try {
      const s = await orchestratorApi.taskSubagentLog(taskId, childSessionId);
      setSession(s);
      setState("loaded");
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setState("error");
    }
  }, [taskId, childSessionId]);
  return (
    <details
      className="subagent-card-transcript"
      data-testid="subagent-transcript"
      onToggle={(e) => {
        if ((e.target as HTMLDetailsElement).open && state !== "loaded" && state !== "loading") {
          void load();
        }
      }}
    >
      <summary>Transcript</summary>
      {state === "loading" ? (
        <p className="subagent-card-transcript-note">Loading transcript…</p>
      ) : null}
      {state === "error" ? (
        <p className="subagent-card-transcript-note">Transcript unavailable: {error}</p>
      ) : null}
      {state === "loaded" && session ? (
        <div className="log-session">
          {(session.messages ?? []).map((m, i) => (
            <LogMessageCard key={m.id ?? i} msg={m} taskId={taskId} toolName={m.tool_name} />
          ))}
        </div>
      ) : null}
    </details>
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
  /**
   * Live sub-agent activity for a spawn_subagent entry (#1043 follow-up), fed
   * by subagent_progress frames. Without it a delegation showed "running…" for
   * its whole (multi-minute) life with nothing underneath.
   */
  subagent?: LiveSubagentActivity;
};

type LiveSubagentActivity = {
  childSessionId?: string;
  role?: string;
  steps: number;
  current?: string;
};

/**
 * liveSubagentActivity folds one subagent_progress frame into an entry's live
 * activity. Pure, and tolerant of a frame that arrives before the spawn entry's
 * own tool_call (the caller drops those).
 */
export function liveSubagentActivity(
  prev: LiveSubagentActivity | undefined,
  frame: TaskStreamFrame,
): LiveSubagentActivity {
  const base: LiveSubagentActivity = prev ?? { steps: 0 };
  const next: LiveSubagentActivity = {
    childSessionId: frame.child_session_id || base.childSessionId,
    role: frame.role || base.role,
    steps:
      typeof frame.steps === "number"
        ? frame.steps
        : typeof frame.step === "number"
          ? Math.max(base.steps, frame.step)
          : base.steps,
    current: base.current,
  };
  switch (frame.phase) {
    case "started":
      next.current = "starting…";
      break;
    case "tool":
      next.current = frame.tool
        ? `${frame.tool}${frame.detail ? ` · ${frame.detail}` : ""}`
        : base.current;
      break;
    case "tool_result":
      next.current = frame.tool ? `${frame.tool} returned` : base.current;
      break;
    case "text":
      next.current = frame.detail ? `writing: ${frame.detail}` : "writing…";
      break;
    case "thinking":
      next.current = frame.detail ? `thinking: ${frame.detail}` : "thinking…";
      break;
    case "note":
      next.current = frame.detail || base.current;
      break;
    case "finished":
      next.current = undefined;
      break;
  }
  return next;
}

function sameFailedCall(a: ActivityEntry, b: ActivityEntry) {
  return a.kind === "tool" && b.kind === "tool" && a.isError && b.isError &&
    a.name === b.name && a.text === b.text && a.result === b.result;
}

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
  const [checklistOpen, setChecklistOpen] = useState(false);
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
      if (frame.type === "agent_message" && frame.content) {
        const content = frame.content;
        // Coalesce adjacent text deltas into one readable assistant entry.
        setEntries((prev) => {
          const last = prev[prev.length - 1];
          if (last?.kind === "message") {
            return [...prev.slice(0, -1), { ...last, text: clampText(last.text + content) }];
          }
          const entry: ActivityEntry = { key: `e${seq.current++}`, kind: "message", text: content };
          return [...prev, entry].slice(-1000);
        });
        return;
      } else if (frame.type === "subagent_progress") {
        // A spawned child's step, attached to the spawn entry that produced it.
        // A frame whose call we have not rendered yet (reconnect mid-child) is
        // dropped rather than inventing an entry with no task text.
        if (!frame.tool_call_id) return;
        setEntries((prev) =>
          prev.map((e) =>
            e.kind === "tool" && e.callId === frame.tool_call_id
              ? { ...e, subagent: liveSubagentActivity(e.subagent, frame) }
              : e,
          ),
        );
        return;
      } else if (frame.type === "tool_call") {
        // task_tracker is represented once by the dedicated progress panel.
        if (frame.name === "task_tracker") return;
        const display = deferredToolDisplay(frame.name, frame.input);
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
      <div className="modal modal-log live-activity-modal">
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
            <CloseButton label="Close modal" onClick={onClose} />
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
                  Progress · {checklist.summary.done}/{checklist.summary.total} done
                </span>
                <span className="live-progress-active">
                  {checklist.activeTask || "Plan complete"}
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
            <div className="table-error">Live stream error: {streamError}</div>
          ) : null}
          {entries.length === 0 && !streamError ? (
            <div className="loading">
              <p>Waiting for activity…</p>
            </div>
          ) : (
            <div className="log-session" aria-live="polite">
              {entries.map((e) => e.kind === "message" ? (
                <div key={e.key} className="live-activity-message" data-testid="live-assistant-message">
                  {e.text}
                </div>
              ) : e.name === "spawn_subagent" ? (
                // Live child card (#1043): pending until the spawn's result
                // frame lands, then status/spend from the tool's JSON result —
                // never a raw JSON blob.
                <SubagentCard
                  key={e.key}
                  taskId={task.id}
                  info={
                    e.pending
                      ? {
                          pending: true,
                          ...(parseSubagentPayload(e.text) ?? {}),
                          childSessionId: e.subagent?.childSessionId,
                          role: e.subagent?.role,
                          steps: e.subagent?.steps,
                          current: e.subagent?.current,
                        }
                      : (parseSubagentPayload(e.result) ?? { success: !e.isError })
                  }
                />
              ) : (
                <article
                  key={e.key}
                  className={`live-tool-entry${e.isError ? " live-tool-entry--error" : ""}`}
                  data-testid="live-tool-entry"
                >
                  <div className="live-tool-heading">
                    <code>{e.name ?? "tool"}</code>
                    <span className={`live-tool-status${e.isError ? " live-tool-status--error" : ""}`}>
                      {e.pending ? "Running…" : e.isError ? "Failed" : "Done"}
                      {(e.repeats ?? 1) > 1 ? ` · ${e.repeats} attempts` : ""}
                    </span>
                  </div>
                  {e.isError && e.result ? (
                    <p className="live-tool-error">{readableToolError(e.result)}</p>
                  ) : null}
                  {e.text || (!e.isError && e.result) ? (
                    <details className="live-tool-details">
                      <summary>Details</summary>
                      {e.text ? <pre>{e.text}</pre> : null}
                      {!e.isError && e.result ? <pre>{e.result}</pre> : null}
                    </details>
                  ) : null}
                </article>
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
  onDelete,
}: {
  task: Task;
  onClose: () => void;
  canStop: boolean;
  onResubmitted?: () => void;
  onEdit?: (task: Task) => void;
  onSelectTask?: (task: Task) => void;
  onDelete?: (task: Task) => void;
}) {
  // The shared hook owns the cancelled-ref guard and the lone setState after
  // the await, so this component no longer needs its own one-shot load-flag
  // setState-in-effect disable.
  // Per-attempt history: attemptId null = the latest transcript (logs row);
  // a number = one superseded transcript (run_logs row). Switching refetches
  // through the same cancellable hook, so loading/error handling is shared.
  const [attemptId, setAttemptId] = useState<number | null>(null);
  const {
    data: session,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(
      () =>
        attemptId === null
          ? orchestratorApi.taskLogs(task.id)
          : orchestratorApi.taskLogHistoryEntry(task.id, attemptId),
      [task.id, attemptId],
    ),
    [task.id, attemptId],
  );
  // The superseded-attempts list is metadata-only and cheap; most tasks have
  // none, in which case the picker never renders.
  const { data: attemptHistory } = useCancellableFetch(
    useCallback(() => orchestratorApi.taskLogHistory(task.id), [task.id]),
    [task.id],
  );
  const attempts = attemptHistory?.entries ?? [];
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

  // hasRun distinguishes the two shapes of the same action: a finished task is
  // RESUBMITTED, a not-yet-run one is kicked off NOW. Both post the same
  // one-off copy and leave the source (and its schedule) untouched; only the
  // wording differs, because "Resubmit" on a job that has never run reads as
  // though something went wrong.
  const hasRun = RESUBMITTABLE.has(task.status ?? "");
  const actionLabel = hasRun ? "Resubmit" : "Run now";

  const resubmit = async () => {
    if (resubmitting) return;
    setResubmitting(true);
    try {
      const created = await orchestratorApi.rerunTask(task.id);
      showToast(
        hasRun
          ? `Resubmitted as task ${created.id.slice(0, 8)}… — running now`
          : `Started run ${created.id.slice(0, 8)}… — the task keeps its schedule`,
        "success",
      );
      onResubmitted?.();
    } catch (err) {
      showToast(
        `${actionLabel} failed: ${err instanceof Error ? err.message : "unknown error"}`,
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

  const canResubmit = RUNNABLE.has(task.status ?? "");
  const [historyOpen, setHistoryOpen] = useState(false);
  const [wakeNote, setWakeNote] = useState("");
  const [waking, setWaking] = useState(false);
  const canFireWake =
    task.status === "paused_awaiting_wake" && Boolean(task.wake_event_key);

  const fireWake = async () => {
    if (waking || !task.wake_event_key) return;
    setWaking(true);
    try {
      await orchestratorApi.wakeTask(task.id, task.wake_event_key, wakeNote.trim());
      showToast(`Fired “${task.wake_event_key}” — task is pending again`, "success");
      onResubmitted?.();
    } catch (err) {
      showToast(
        `Fire event failed: ${err instanceof Error ? err.message : "unknown error"}`,
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
        throw new Error(body?.error ?? `HTTP ${res.status}`);
      }
      const { conversation_id } = (await res.json()) as { conversation_id: string | null };
      if (!conversation_id) throw new Error("no conversation id returned");
      window.location.href = `/chat?c=${encodeURIComponent(conversation_id)}`;
    } catch (err) {
      showToast(
        `Discuss failed: ${err instanceof Error ? err.message : "unknown error"}`,
        "error",
      );
      setDiscussing(false);
    }
  };

  return (
    <div
      className="modal-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label="Task Logs"
    >
      <div className="modal modal-log">
        <div className="modal-header">
          {/* Lead with the operator's own title when the job has one; the
              full prompt stays in the tooltip either way. */}
          <h3 className="modal-log-title" title={task.prompt ?? ""}>
            Task: {taskRunLabel(task, 90)}
          </h3>
          <CloseButton label="Close modal" onClick={onClose} />
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
              {attempts.length > 0 ? (
                <select
                  className="btn btn-secondary"
                  data-testid="attempt-picker"
                  aria-label="Transcript attempt"
                  value={attemptId ?? "latest"}
                  onChange={(e) =>
                    setAttemptId(
                      e.target.value === "latest" ? null : Number(e.target.value),
                    )
                  }
                >
                  <option value="latest">Latest transcript</option>
                  {attempts.map((a) => (
                    <option key={a.id} value={a.id}>
                      Superseded {formatTimeFirst(a.superseded_at)}
                    </option>
                  ))}
                </select>
              ) : null}
              {onEdit && RUNNABLE.has(task.status ?? "") ? (
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
                  {resubmitting ? (hasRun ? "Resubmitting…" : "Starting…") : actionLabel}
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
                  title="Permanently delete this task and its run history"
                  onClick={() => onDelete(task)}
                >
                  Delete
                </button>
              ) : null}
              {canFireWake ? (
                <span className="wake-fire" data-testid="wake-fire">
                  <input
                    type="text"
                    aria-label="Wake note"
                    placeholder="optional note"
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
                    {waking ? "Firing…" : `Fire “${task.wake_event_key}”`}
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
                History
              </button>
              {session && session.messages && session.messages.length > 0 ? (
                <button
                  type="button"
                  className="btn btn-secondary"
                  data-testid="discuss-run-button"
                  disabled={discussing}
                  onClick={() => void discuss()}
                >
                  {discussing ? "Opening chat…" : "Discuss in chat"}
                </button>
              ) : null}
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
                {visibleIdx.map((i) => {
                  // Sub-agent linkage entries (#1043) render as child cards,
                  // not raw JSON tool messages.
                  if (messages[i].message_type === "subagent_spawned") {
                    const info = parseSubagentPayload(messages[i].content);
                    if (info) {
                      return (
                        <SubagentCard key={messages[i].id ?? i} info={info} taskId={task.id} />
                      );
                    }
                  }
                  return (
                    <LogMessageCard
                      key={messages[i].id ?? i}
                      msg={messages[i]}
                      taskId={task.id}
                      toolName={toolNames.get(i)}
                    />
                  );
                })}
              </div>
            </>
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
          aria-label="Mark run as helpful"
          data-tip-top="Helpful"
          disabled={busy}
          onClick={() => void feedback("up")}
        >
          👍
        </button>
        <button
          type="button"
          className="btn btn-small"
          aria-label="Mark run as not helpful"
          data-tip-top="Not helpful"
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
