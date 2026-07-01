"use client";

// Browser-side client for the orchestrator. Every call goes through the
// /api/orchestrator/* proxy (which injects the user's identity), so this module
// only deals with relative URLs and the bearer token (when the user logged in
// via moc's username/password). Elcano-cookie users carry no bearer; the cookie
// rides along automatically and the proxy resolves it.

import { getStoredToken } from "./orchestratorAuth";

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
  prompt?: string;
  description?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number | null;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  allow_network?: boolean;
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
};

export type TaskCreate = {
  prompt: string;
  description?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  allow_network?: boolean;
  persona?: string;
  scheduled_for?: string;
  recurrence?: string;
  files?: string[];
  tags?: string[];
  retry_policy?: RetryPolicy;
  run_if?: RunIf | null;
  // SLA monitoring (#274). Omit expected_duration_minutes for no SLA.
  expected_duration_minutes?: number | null;
  sla_warn_multiplier?: number;
  sla_fail_multiplier?: number;
};

// SLAReport / SLAReportTask mirror models.SLAReport (#274): the
// GET /admin/sla-report response. task_name is the prompt's first line (fleet
// has no separate `name` column).
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
  prompt?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number;
  max_retries?: number;
  recurrence?: string;
  timezone?: string;
  priority?: number;
  allow_network?: boolean;
  instruction_self_improve?: boolean;
  persona?: string;
  description?: string;
  tags?: string[];
  // SLA expectation (#274); omit for no SLA.
  expected_duration_minutes?: number | null;
  sla_warn_multiplier?: number;
  sla_fail_multiplier?: number;
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

export type DashboardStats = {
  pending_tasks?: number;
  running_tasks?: number;
  completed_tasks_today?: number;
  failed_tasks_today?: number;
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

export type ConcurrencyConfig = {
  max_concurrent_agents: number;
  warm_pool_size?: number;
};

export type LogMessage = {
  id?: string;
  role?: string;
  content?: string;
  model?: string;
  provider?: string;
  created_at?: number;
  finished_at?: number;
};

export type LogSession = {
  id?: string;
  title?: string;
  prompt_tokens?: number;
  completion_tokens?: number;
  cost?: number;
  messages?: LogMessage[];
};


// #508 live task activity stream frames (GET /tasks/{id}/stream).
export type TaskStreamFrame = {
  type: "agent_message" | "tool_call" | "tool_result" | "status" | string;
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
};

// createSSEParser returns a chunk-feeder that assembles SSE frames (split on
// blank lines, `data:` JSON payloads) and invokes onFrame per parsed frame.
// Exported for unit tests; used by streamTaskActivity below.
export function createSSEParser(onFrame: (frame: TaskStreamFrame) => void): (chunk: string) => void {
  let buffer = "";
  return (chunk: string) => {
    buffer += chunk;
    for (;;) {
      const sep = buffer.indexOf("\n\n");
      if (sep < 0) break;
      const raw = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      let data = "";
      for (const line of raw.split("\n")) {
        if (line.startsWith("data:")) data += line.slice(5).trim();
      }
      if (!data) continue; // heartbeat / comment frame
      try {
        onFrame(JSON.parse(data) as TaskStreamFrame);
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
  // checkpoint. Admin permission required server-side.
  cancelTask: (taskId: string) =>
    request<Task>(`/tasks/${encodeURIComponent(taskId)}`, { method: "DELETE" }),
  // Attach to a task's live activity stream (#508). Resolves when the stream
  // ends (terminal frame or server close); rejects on transport errors. Abort
  // via the signal to detach.
  streamTaskActivity: async (
    taskId: string,
    onFrame: (frame: TaskStreamFrame) => void,
    signal: AbortSignal,
  ): Promise<void> => {
    const res = await fetch(`/api/orchestrator/tasks/${encodeURIComponent(taskId)}/stream`, {
      headers: authHeaders({ Accept: "text/event-stream" }),
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
  config: () => request<{ version?: string; timezone?: string }>("/config"),
  me: () => request<{ authenticated: boolean; username?: string; role?: string }>("/me"),

  // SLA report (#274): admin-only per-task actual-duration p50/p95 + breach
  // rate over a window. days defaults to 7 (clamped to [1, 90] server-side).
  slaReport: (days = 7) =>
    request<SLAReport>(`/sla-report?days=${encodeURIComponent(days)}`),

  // Read-only task-template catalog for "new task from a template" (#262).
  taskTemplates: () => request<TaskTemplate[]>("/task-templates"),

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

  // Global concurrency cap.
  concurrency: () => request<ConcurrencyConfig>("/concurrency"),
  setConcurrency: (max: number) =>
    request<ConcurrencyConfig>("/concurrency", {
      method: "PUT",
      body: JSON.stringify({ max_concurrent_agents: max }),
    }),

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
