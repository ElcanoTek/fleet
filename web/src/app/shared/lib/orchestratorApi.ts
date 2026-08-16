"use client";

// Browser-side client for the orchestrator. Every call goes through the
// /api/orchestrator/* proxy (which injects the user's identity), so this module
// only deals with relative URLs and the bearer token (when the user logged in
// via moc's username/password). Elcano-cookie users carry no bearer; the cookie
// rides along automatically and the proxy resolves it.

import { getStoredToken } from "./orchestratorAuth";
import { parseSseChunk } from "@/app/lib/sse";

// MCPChoice mirrors agentcore.MCPChoice: which optional server is on + which
// credential account backs it. Account === "" means the default/shared seat.
export type MCPChoice = { server: string; account?: string };

// RetryPolicy mirrors models.RetryPolicy (#201): per-task retry backoff + which
// failure classes retry. Set via API/CLI; the orchestrator form does not expose
// it yet.
export type RetryPolicy = {
  backoff?: "exponential" | "fixed";
  initial_delay_seconds?: number;
  max_delay_seconds?: number;
  retry_on?: string[];
  no_retry_on?: string[];
};

export type RunIf = {
  command: string;
  exit_code_is?: number;
  timeout_seconds?: number;
  on_error?: "run" | "skip";
};

export type Task = {
  id: string;
  // Short operator-facing label shown wherever the task is listed. Absent/empty
  // = untitled, and every surface falls back to the prompt's first line (which
  // is why operators used to write a title line at the top of the prompt).
  // Distinct from the server-side `name`, which is the unique import/export
  // identity key and is cleared on every copy.
  title?: string;
  prompt?: string;
  description?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number | null;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  allow_network?: boolean;
  carry_context?: boolean;
  // Sub-agent delegation gate (#1043). Defaults TRUE server-side (opt-out);
  // the server always serializes it, so absence only happens on old payloads —
  // treat undefined as true.
  allow_delegation?: boolean;
  persona?: string;
  tags?: string[];
  retry_policy?: RetryPolicy;
  source_task_id?: string;
  status?: string;
  created_by?: string;
  created_by_username?: string;
  agent_session_id?: string;
  created_at?: string;
  started_at?: string;
  completed_at?: string;
  scheduled_for?: string;
  recurrence?: string;
  // Recurrence end conditions: repeat until an instant and/or for a total
  // remaining-run count. Absent = repeat forever.
  recurrence_until?: string | null;
  recurrence_remaining?: number | null;
  files?: string[];
  run_if?: RunIf | null;
  skip_count?: number;
  last_skip_at?: string | null;
  last_skip_reason?: string | null;
  // SLA monitoring (#274). expected_duration_minutes is null when no SLA is
  // configured; sla_breached is latched true once the fail threshold is crossed;
  // actual_duration_seconds is populated on terminal transition.
  expected_duration_minutes?: number | null;
  sla_warn_multiplier?: number;
  sla_fail_multiplier?: number;
  sla_breached?: boolean;
  actual_duration_seconds?: number | null;
  thinking_budget_tokens?: number | null;
  sandbox_limits?: TaskSandboxLimits | null;
  wake_at?: string | null;
  wake_event_key?: string;
  wake_note?: string;
};

export type TaskCreate = {
  title?: string;
  prompt: string;
  description?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  allow_network?: boolean;
  carry_context?: boolean;
  // Sub-agent delegation gate (#1043): omit for the server default (TRUE);
  // send false explicitly to opt the task out.
  allow_delegation?: boolean;
  // Per-task extended-thinking override (#220): omit to inherit the deployment
  // default, 0 = off, >0 = this task's budget in tokens.
  thinking_budget_tokens?: number | null;
  persona?: string;
  scheduled_for?: string;
  recurrence?: string;
  recurrence_until?: string;
  recurrence_remaining?: number;
  files?: string[];
  tags?: string[];
  retry_policy?: RetryPolicy;
  run_if?: RunIf | null;
  // SLA monitoring (#274). Omit expected_duration_minutes for no SLA.
  expected_duration_minutes?: number | null;
  sla_warn_multiplier?: number;
  sla_fail_multiplier?: number;
  sandbox_limits?: TaskSandboxLimits | null;
};

// Per-task sandbox cgroup override (#205). Zero/omit a field to keep the
// global FLEET_SANDBOX_* default. Floors: memory_mb >= 128, pids >= 16, cpus > 0.
export type TaskSandboxLimits = {
  memory_mb?: number;
  cpus?: number;
  pids?: number;
};

// SLAReport / SLAReportTask mirror models.SLAReport (#274): the
// GET /admin/sla-report response. task_name is the task's title when it has
// one — so a titled job's occurrences collapse into a single row — and the
// prompt's first line otherwise.
export type SLAReportTask = {
  task_name: string;
  expected_minutes: number;
  p50_actual_minutes: number;
  p95_actual_minutes: number;
  breach_rate_pct: number;
  sample_count: number;
};

export type SLAReport = {
  period: string;
  window_days: number;
  tasks: SLAReportTask[];
};

// UsageReport / UsageBucket mirror models.UsageReport (#601 part 1): the
// GET /admin/usage response. key is the grouping value (user email/username,
// API key id, project id, model slug, or the YYYY-MM-DD bucket start for
// day/week); the empty key collects rows without that dimension. Per-source
// splits (task_* / chat_*) ride alongside the combined totals; cached_tokens
// is chat-only. note carries the honest-scope pricing caveat (#289).
export type UsageBucket = {
  key: string;
  label?: string;
  cost_usd: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  task_cost_usd: number;
  chat_cost_usd: number;
  task_iterations: number;
  chat_turns: number;
};

export type UsageGroupBy = "user" | "key" | "project" | "model" | "day" | "week";

export type UsageReport = {
  group_by: UsageGroupBy;
  from: string;
  to: string;
  buckets: UsageBucket[];
  totals: UsageBucket;
  sources: string[];
  note: string;
};

// AdoptionReport and friends mirror models.AdoptionReport: the
// GET /admin/usage/adoption response behind the exec Adoption view. days is
// the full UTC day axis of [from, to); every user's daily_tokens series is
// index-aligned to it. prev_* fields compare against the equal-length window
// starting at prev_from. The empty user collects unattributed spend
// (deleted-user task rows) and never counts as an active user.
export type AdoptionUser = {
  user: string;
  cost_usd: number;
  task_cost_usd: number;
  chat_cost_usd: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  task_iterations: number;
  chat_turns: number;
  active_days: number;
  last_active?: string;
  prev_cost_usd: number;
  prev_tokens: number;
  daily_tokens: number[];
};

export type AdoptionSeat = {
  email: string;
  created_at: string;
};

export type AdoptionDay = {
  day: string;
  cost_usd: number;
  tokens: number;
  actions: number;
  active_users: number;
};

export type AdoptionTotals = {
  active_users: number;
  prev_active_users: number;
  new_active_users: number;
  registered_users: number;
  cost_usd: number;
  prev_cost_usd: number;
  tokens: number;
  prev_tokens: number;
  cached_tokens: number;
  chat_turns: number;
  task_iterations: number;
};

export type AdoptionReport = {
  from: string;
  to: string;
  prev_from: string;
  days: string[];
  users: AdoptionUser[];
  inactive_users: AdoptionSeat[];
  daily: AdoptionDay[];
  totals: AdoptionTotals;
  sources: string[];
  note: string;
};

// BudgetStatus mirrors models.BudgetStatus (#601 part 2): one configured
// per-principal rolling budget plus its live evaluation — current window
// [start, end), spend recomputed from the persisted metering, the effective
// hard bounds after the fail-safe clamp against the live global ceilings, and
// whether this window's one soft alert has fired.
export type BudgetStatus = {
  id: string;
  scope: "user" | "key" | "project";
  principal_id: string;
  window: "day" | "week" | "month";
  soft_usd?: number;
  hard_usd?: number;
  soft_tokens?: number;
  hard_tokens?: number;
  window_start: string;
  window_end: string;
  spend_usd: number;
  spend_tokens: number;
  effective_hard_usd?: number;
  effective_hard_tokens?: number;
  soft_alerted: boolean;
};

// BudgetCreate is the POST /admin/budgets body. Only user|key scopes exist;
// anything else (including the legacy project scope) is rejected server-side.
export type BudgetCreate = {
  scope: "user" | "key";
  principal_id: string;
  window: "day" | "week" | "month";
  soft_usd?: number;
  hard_usd?: number;
  soft_tokens?: number;
  hard_tokens?: number;
};

// CostForecast mirrors agentcore.CostForecast (#233): the pre-submission token +
// cost forecast returned by POST /tasks/estimate. Cost fields are null when the
// model's pricing is unknown; the token estimates are always present.
export type CostRange = { min: number; max: number };
export type CostForecast = {
  model: string;
  estimated_prompt_tokens: number;
  system_prompt_tokens: number;
  tool_definitions_tokens: number;
  avg_output_tokens: number;
  max_iterations: number;
  pricing_known: boolean;
  per_iteration_cost_usd: number | null;
  estimated_total_cost_usd: number | null;
  estimated_total_cost_range: CostRange | null;
  max_cost_ceiling_usd: number;
  would_hit_ceiling: boolean;
  note: string;
};

// TaskTemplateTask is the partial TaskCreate a template carries — the subset of
// editable fields the create form pre-fills (#262). Mirrors
// clientconfig.TaskTemplateTask. Omitted fields leave the form at its default.
export type TaskTemplateTask = {
  // An explicit title for the seeded task. Omitted, the form falls back to the
  // template's own name.
  title?: string;
  prompt?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number;
  max_retries?: number;
  recurrence?: string;
  timezone?: string;
  priority?: number;
  allow_network?: boolean;
  // Omitted = the form default (true, #1043); an explicit false opts out.
  allow_delegation?: boolean;
  carry_context?: boolean;
  instruction_self_improve?: boolean;
  persona?: string;
  description?: string;
  tags?: string[];
  // SLA expectation (#274); omit for no SLA.
  expected_duration_minutes?: number | null;
  sla_warn_multiplier?: number;
  sla_fail_multiplier?: number;
  // Per-task extended-thinking override (#220): omit to inherit the default,
  // 0 = off, >0 = this task's budget in tokens.
  thinking_budget_tokens?: number | null;
  sandbox_limits?: TaskSandboxLimits | null;
};

// TaskTemplate is one "new task from a template" entry from the bundle's
// read-only catalog. `variables` are the {token} placeholder names extracted from
// the prompt server-side, so the UI can prompt for values before applying.
export type TaskTemplate = {
  name: string;
  description?: string;
  icon?: string;
  variables: string[];
  task: TaskTemplateTask;
};

// One entry in the hybrid prompt library. Git entries are bundle-owned and
// read-only; workspace entries may be private or shared and are editable only
// by their owner (or an admin).
export type PromptLibraryItem = {
  id: string;
  name: string;
  description?: string;
  content: string;
  source: "git" | "workspace";
  visibility: "private" | "workspace";
  read_only: boolean;
  owned_by_caller: boolean;
  owner_username?: string;
  path?: string;
  created_at?: string;
  updated_at?: string;
};

export type PromptLibraryWrite = {
  name: string;
  description: string;
  content: string;
  visibility: "private" | "workspace";
};

export type DashboardStats = {
  pending_tasks?: number;
  running_tasks?: number;
  completed_tasks_today?: number;
  failed_tasks_today?: number;
  // Live agent-pool occupancy (absent on servers that predate the field or
  // when the pool isn't wired): agents executing scheduled tasks right now,
  // and the pool's schedulable slot count.
  active_agents?: number;
  agent_slots?: number;
};

export type Paginated<T> = { data: T[]; total: number; limit: number; offset: number };

// Dataset / table agent (#514).
export type DatasetColumn = {
  name: string;
  type: "text" | "number" | "boolean";
  output?: boolean;
  description?: string;
};

export type Dataset = {
  id: string;
  name: string;
  goal: string;
  columns: DatasetColumn[];
  model?: string;
  persona?: string;
  status: "idle" | "running" | "paused";
  concurrency: number;
  created_at: string;
  updated_at: string;
  row_counts?: Record<string, number>;
};

export type DatasetRow = {
  id: string;
  dataset_id: string;
  row_index: number;
  cells: Record<string, unknown>;
  status: "pending" | "running" | "proposed" | "approved" | "failed";
  proposed?: Record<string, unknown>;
  result_note?: string;
  error?: string;
  attempts: number;
  cost_usd: number;
  updated_at: string;
};

export type DatasetCreate = {
  name: string;
  goal: string;
  columns: DatasetColumn[];
  model: string;
  persona?: string;
  concurrency?: number;
};


// The MCP server catalog row. Mirrors chat's /mcp-servers row + the per-server
// credential-account names (never secret values).
export type McpServer = {
  name: string;
  display_name?: string;
  description?: string;
  tool_count?: number;
  enabled?: boolean;
  accounts?: string[];
  // remote marks a per-user remote (hosted) MCP server the caller connected via
  // OAuth (#443/#466). The orchestrator overlay auto-applies ALL of the owner's
  // connected remote servers to every scheduled run, so the picker shows them as
  // connected/auto-available (read-only) rather than a per-task toggle.
  remote?: boolean;
};

export type LogToolCall = {
  id?: string;
  name?: string;
  arguments?: string;
};

export type LogMessage = {
  id?: string;
  role?: string;
  content?: string;
  reasoning?: string;
  model?: string;
  provider?: string;
  created_at?: number;
  finished_at?: number;
  message_type?: string;
  tool_calls?: LogToolCall[];
  tool_call_id?: string;
  tool_name?: string;
  is_error?: boolean;
};

// Token fields are CUMULATIVE across the whole session (billing/display
// numbers — see agentcore.LogSession). cached_tokens is the cache-read subset
// of prompt_tokens.
export type LogSession = {
  id?: string;
  title?: string;
  prompt_tokens?: number;
  completion_tokens?: number;
  cached_tokens?: number;
  cache_creation_tokens?: number;
  cost?: number;
  created_at?: number;
  updated_at?: number;
  messages?: LogMessage[];
};

// One superseded transcript in a task's per-attempt run log history
// (GET /logs/{task_id}/history): the entry id + when a newer transcript
// replaced it. The payload is fetched per-entry.
export type RunLogMeta = {
  id: number;
  superseded_at: string;
};

// #508 live task activity stream frames (GET /tasks/{id}/stream).
export type TaskStreamFrame = {
  type:
    | "agent_message"
    | "tool_call"
    | "tool_result"
    | "status"
    | "subagent_progress"
    | string;
  role?: string;
  content?: string;
  call_id?: string;
  name?: string;
  input?: string;
  output?: string;
  error?: boolean;
  status?: string;
  task_id?: string;
  cost_usd?: number;
  stopped_by?: string;
  // subagent_progress (#1043 follow-up): a spawned child's relabeled step,
  // attached to its spawn entry via tool_call_id.
  tool_call_id?: string;
  child_session_id?: string;
  phase?: string;
  tool?: string;
  detail?: string;
  step?: number;
  is_err?: boolean;
  steps?: number;
  // SSE transport id, attached client-side (not part of the JSON payload).
  // Used to resume a dropped live-log connection without duplicate activity.
  _event_id?: string;
};

// createSSEParser returns a chunk-feeder that assembles SSE frames and invokes
// onFrame per parsed `data:` JSON payload. Frame assembly is delegated to the
// hardened chat parser (parseSseChunk, #589) rather than reimplemented here, so
// both SSE consumers share ONE parser: it accepts CRLF frame delimiters (a
// proxy that normalizes line endings would otherwise leave a stream with no
// "\n\n" — zero frames emitted, buffer growing forever) and joins multi-line
// `data:` values with "\n" per the SSE spec instead of corrupting them by bare
// concatenation. Heartbeat/comment frames carry no data and are dropped by
// parseSseChunk itself.
// Exported for unit tests; used by streamTaskActivity below.
export function createSSEParser(onFrame: (frame: TaskStreamFrame) => void): (chunk: string) => void {
  let buffer = "";
  return (chunk: string) => {
    const { events, remainder } = parseSseChunk(buffer + chunk);
    buffer = remainder;
    for (const ev of events) {
      try {
        const frame = JSON.parse(ev.data) as TaskStreamFrame;
        if (ev.id) frame._event_id = ev.id;
        onFrame(frame);
      } catch {
        // tolerate a malformed frame rather than killing the stream
      }
    }
  };
}


// #516 self-improving memory.
export type TaskLearnedInstruction = {
  id: string;
  task_id: string;
  version: number;
  content: string;
  status: "proposed" | "active" | "archived";
  signal_count: number;
  created_at: number;
  activated_at?: number;
  activated_by?: string;
};


// Scheduler UX 2.0 (#504): a projected upcoming run.
export type UpcomingRun = {
  task_id: string;
  name?: string;
  title?: string;
  prompt: string;
  recurrence?: string;
  next_run: string;
  recurring: boolean;
};

class OrchestratorError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

function authHeaders(extra?: Record<string, string>): Record<string, string> {
  const headers: Record<string, string> = { ...(extra ?? {}) };
  const token = getStoredToken();
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return headers;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/orchestrator${path}`, {
    ...init,
    headers: authHeaders(init?.headers as Record<string, string> | undefined),
    cache: "no-store",
  });
  if (!res.ok) {
    let detail = `Request failed (${res.status})`;
    try {
      const body = await res.json();
      detail = body.detail || body.error || detail;
    } catch {
      /* non-JSON body */
    }
    throw new OrchestratorError(detail, res.status);
  }
  // 204 / empty bodies
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

export const orchestratorApi = {
  stats: () => request<DashboardStats>("/stats"),
  tasks: (qs: string) => request<Paginated<Task>>(`/tasks${qs ? `?${qs}` : ""}`),
  createTask: (body: TaskCreate) =>
    request<Task>("/tasks", { method: "POST", body: JSON.stringify(body) }),
  // Pre-submission cost forecast (#233): same body as createTask, creates
  // nothing. The endpoint returns 200 (pricing known) or 202 (pricing unknown);
  // both carry a CostForecast, so request() resolves either as success.
  estimateTask: (body: TaskCreate) =>
    request<CostForecast>("/tasks/estimate", { method: "POST", body: JSON.stringify(body) }),
  taskLogs: (taskId: string) => request<LogSession>(`/logs/${encodeURIComponent(taskId)}`),
  // A spawned child's own transcript (#1043) — 404 when the transcript file is
  // no longer on the host (the linkage entry on the parent log stays the
  // durable record either way).
  taskSubagentLog: (taskId: string, childSessionId: string) =>
    request<LogSession>(
      `/logs/${encodeURIComponent(taskId)}/subagents/${encodeURIComponent(childSessionId)}`,
    ),
  // Per-attempt run log history: transcripts superseded by a retry or an
  // ask-pause/wake resume of the SAME task id. Metadata list + one entry.
  taskLogHistory: (taskId: string) =>
    request<{ entries: RunLogMeta[] }>(`/logs/${encodeURIComponent(taskId)}/history`),
  taskLogHistoryEntry: (taskId: string, entryId: number) =>
    request<LogSession>(`/logs/${encodeURIComponent(taskId)}/history/${entryId}`),
  // Edit (PUT /tasks/{id}): rewrites a pending/scheduled task's definition —
  // for a recurring task that means every future run. The server re-checks
  // editability transactionally (409 when the task started meanwhile).
  updateTask: (taskId: string, body: TaskCreate) =>
    request<Task>(`/tasks/${encodeURIComponent(taskId)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    }),
  // Resubmit (#TBD): POST /tasks/{id}/rerun creates a NEW one-time task copied
  // from this one (recurrence cleared, runs now); optional overrides replace
  // fields on the copy. The source task is untouched.
  rerunTask: (taskId: string, overrides?: Record<string, unknown>) =>
    request<Task>(`/tasks/${encodeURIComponent(taskId)}/rerun`, {
      method: "POST",
      body: JSON.stringify(overrides ? { overrides } : {}),
    }),
  // Fire a named event at a task parked by wake_on_event (docs/SELF-WAKE.md).
  // The key must match the one the task is waiting for.
  wakeTask: (taskId: string, event: string, note?: string) =>
    request<{ status: string }>(`/tasks/${encodeURIComponent(taskId)}/wake`, {
      method: "POST",
      body: JSON.stringify({ event, note: note ?? "" }),
    }),
  upcomingRuns: (limit = 50, until?: string) =>
    request<{ upcoming: UpcomingRun[] }>(
      `/tasks/upcoming?limit=${limit}${until ? `&until=${encodeURIComponent(until)}` : ""}`,
    ),
  // #516 self-improving memory: feedback + versioned learned instructions.
  submitFeedback: (taskId: string, rating: "up" | "down", critique?: string) =>
    request<unknown>(`/tasks/${encodeURIComponent(taskId)}/feedback`, {
      method: "POST",
      body: JSON.stringify({ rating, critique: critique ?? "" }),
    }),
  learnedInstructions: (taskId: string) =>
    request<{ learned_instructions: TaskLearnedInstruction[] }>(
      `/tasks/${encodeURIComponent(taskId)}/learned-instructions`,
    ),
  activateLearnedInstruction: (taskId: string, version: number) =>
    request<TaskLearnedInstruction>(
      `/tasks/${encodeURIComponent(taskId)}/learned-instructions/${version}/activate`,
      { method: "POST" },
    ),
  deactivateLearnedInstruction: (taskId: string) =>
    request<{ deactivated: boolean }>(
      `/tasks/${encodeURIComponent(taskId)}/learned-instructions/active`,
      { method: "DELETE" },
    ),
  // Cancel/stop a task (#508): flips the row to cancelled with who-stopped-it
  // attribution and interrupts a live run at the governed loop's next
  // checkpoint. Server-side authorization permits admins/scoped cancel keys
  // and the user who created the task.
  cancelTask: (taskId: string) =>
    request<Task>(`/tasks/${encodeURIComponent(taskId)}`, { method: "DELETE" }),
  // Attach to a task's live activity stream (#508). Resolves when the stream
  // ends (terminal frame or server close); rejects on transport errors. Abort
  // via the signal to detach.
  streamTaskActivity: async (
    taskId: string,
    onFrame: (frame: TaskStreamFrame) => void,
    signal: AbortSignal,
    lastEventID?: string,
  ): Promise<void> => {
    const headers = authHeaders({ Accept: "text/event-stream" });
    if (lastEventID) headers["Last-Event-ID"] = lastEventID;
    const res = await fetch(`/api/orchestrator/tasks/${encodeURIComponent(taskId)}/stream`, {
      headers,
      cache: "no-store",
      signal,
    });
    if (!res.ok || !res.body) {
      throw new OrchestratorError(`stream failed (${res.status})`, res.status);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    const feed = createSSEParser(onFrame);
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      feed(decoder.decode(value, { stream: true }));
    }
  },
  // server_time is the orchestrator's own clock, formatted in ITS location so
  // the offset rides along; default_task_timezone is the zone a cron recurrence
  // fires in when a task names none (distinct from `timezone`, the server clock).
  config: () =>
    request<{
      version?: string;
      timezone?: string;
      server_time?: string;
      default_task_timezone?: string;
    }>("/config"),
  me: () => request<{ authenticated: boolean; username?: string; role?: string }>("/me"),

  // SLA report (#274): admin-only per-task actual-duration p50/p95 + breach
  // rate over a window. days defaults to 7 (clamped to [1, 90] server-side).
  slaReport: (days = 7) =>
    request<SLAReport>(`/sla-report?days=${encodeURIComponent(days)}`),

  // Usage analytics (#601 part 1): admin-only cost/token roll-up by
  // principal / project / model / time bucket over [from, to). from/to are
  // RFC 3339 or YYYY-MM-DD; both optional (default: trailing 30 days).
  usage: (params: { groupBy?: UsageGroupBy; from?: string; to?: string } = {}) => {
    const qs = new URLSearchParams();
    if (params.groupBy) qs.set("group_by", params.groupBy);
    if (params.from) qs.set("from", params.from);
    if (params.to) qs.set("to", params.to);
    const suffix = qs.size > 0 ? `?${qs.toString()}` : "";
    return request<UsageReport>(`/admin/usage${suffix}`);
  },

  // Adoption analytics: admin-only per-user AI-adoption audit — merged
  // per-user totals with daily series, previous-window trend deltas, and the
  // inactive-seat roster over [from, to) (default: trailing 30 days).
  adoption: (params: { from?: string; to?: string } = {}) => {
    const qs = new URLSearchParams();
    if (params.from) qs.set("from", params.from);
    if (params.to) qs.set("to", params.to);
    const suffix = qs.size > 0 ? `?${qs.toString()}` : "";
    return request<AdoptionReport>(`/admin/usage/adoption${suffix}`);
  },

  // Per-principal rolling budgets (#601 part 2): admin-only list + upsert/delete.
  budgets: () => request<{ budgets: BudgetStatus[] }>("/admin/budgets"),
  createBudget: (body: BudgetCreate) =>
    request<BudgetStatus>("/admin/budgets", { method: "POST", body: JSON.stringify(body) }),
  deleteBudget: (budgetId: string) =>
    request<{ status: string }>(`/admin/budgets/${encodeURIComponent(budgetId)}`, {
      method: "DELETE",
    }),

  // Read-only task-template catalog for "new task from a template" (#262).
  taskTemplates: () => request<TaskTemplate[]>("/task-templates"),

  // Hybrid prompt library shared by chat + Operations Center.
  prompts: () => request<PromptLibraryItem[]>("/prompts"),
  createPrompt: (body: PromptLibraryWrite) =>
    request<PromptLibraryItem>("/prompts", { method: "POST", body: JSON.stringify(body) }),
  updatePrompt: (id: string, body: PromptLibraryWrite) =>
    request<PromptLibraryItem>(`/prompts/${encodeURIComponent(id)}`, {
      method: "PUT",
      body: JSON.stringify(body),
    }),
  deletePrompt: (id: string) =>
    request<void>(`/prompts/${encodeURIComponent(id)}`, { method: "DELETE" }),

  // MCP catalog + credential accounts.
  mcpServers: () => request<{ servers: McpServer[] }>("/mcp-servers"),
  createAccount: (server: string, body: { account: string; secrets: Record<string, string> }) =>
    request<{ server: string; account: string }>(
      `/mcp-servers/${encodeURIComponent(server)}/accounts`,
      { method: "POST", body: JSON.stringify(body) },
    ),
  updateAccount: (server: string, account: string, body: { secrets: Record<string, string> }) =>
    request<{ server: string; account: string }>(
      `/mcp-servers/${encodeURIComponent(server)}/accounts/${encodeURIComponent(account)}`,
      { method: "PUT", body: JSON.stringify(body) },
    ),
  deleteAccount: (server: string, account: string) =>
    request<{ deleted: boolean }>(
      `/mcp-servers/${encodeURIComponent(server)}/accounts/${encodeURIComponent(account)}`,
      { method: "DELETE" },
    ),

  // Dataset / table agent (#514).
  datasets: () => request<{ datasets: Dataset[] }>("/datasets"),
  dataset: (id: string) => request<Dataset>(`/datasets/${encodeURIComponent(id)}`),
  createDataset: (body: DatasetCreate) =>
    request<Dataset>("/datasets", { method: "POST", body: JSON.stringify(body) }),
  deleteDataset: (id: string) =>
    request<void>(`/datasets/${encodeURIComponent(id)}`, { method: "DELETE" }),
  datasetRows: (id: string, qs = "") =>
    request<{ rows: DatasetRow[]; row_counts: Record<string, number> }>(
      `/datasets/${encodeURIComponent(id)}/rows${qs}`,
    ),
  importDatasetRowsCSV: (id: string, csv: string) =>
    request<{ imported: number }>(`/datasets/${encodeURIComponent(id)}/rows`, {
      method: "POST",
      headers: { "Content-Type": "text/csv" },
      body: csv,
    }),
  runDataset: (id: string) =>
    request<{ status: string }>(`/datasets/${encodeURIComponent(id)}/run`, { method: "POST" }),
  pauseDataset: (id: string) =>
    request<{ status: string }>(`/datasets/${encodeURIComponent(id)}/pause`, { method: "POST" }),
  approveDatasetRows: (id: string, rowIds?: string[]) =>
    request<{ approved: number }>(`/datasets/${encodeURIComponent(id)}/approve`, {
      method: "POST",
      body: JSON.stringify({ row_ids: rowIds ?? [] }),
    }),
  rerunDatasetRows: (id: string, rowIds?: string[]) =>
    request<{ reset: number }>(`/datasets/${encodeURIComponent(id)}/rerun`, {
      method: "POST",
      body: JSON.stringify({ row_ids: rowIds ?? [] }),
    }),

  uploadFile: async (file: File): Promise<{ filename: string }> => {
    const form = new FormData();
    form.append("file", file);
    const res = await fetch("/api/orchestrator/upload", {
      method: "POST",
      headers: authHeaders(),
      body: form,
    });
    if (!res.ok) throw new OrchestratorError(`Upload failed (${res.status})`, res.status);
    return res.json();
  },
};

export { OrchestratorError };
