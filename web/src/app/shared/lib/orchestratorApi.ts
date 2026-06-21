"use client";

// Browser-side client for the orchestrator. Every call goes through the
// /api/orchestrator/* proxy (which injects the user's identity), so this module
// only deals with relative URLs and the bearer token (when the user logged in
// via moc's username/password). Elcano-cookie users carry no bearer; the cookie
// rides along automatically and the proxy resolves it.

import { getStoredToken } from "./orchestratorAuth";

export type Node = {
  id: string;
  hostname?: string;
  name?: string;
  os_type?: string;
  status?: "idle" | "busy" | "offline" | "error";
  last_heartbeat?: string;
};

// MCPChoice mirrors agentcore.MCPChoice: which optional server is on + which
// credential account backs it. Account === "" means the default/shared seat.
export type MCPChoice = { server: string; account?: string };

export type Task = {
  id: string;
  prompt?: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number | null;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  status?: string;
  created_by?: string;
  created_by_username?: string;
  agent_session_id?: string;
  created_at?: string;
  scheduled_for?: string;
  recurrence?: string;
  files?: string[];
};

export type TaskCreate = {
  prompt: string;
  model?: string;
  fallback_model?: string;
  max_iterations?: number;
  mcp_selection?: MCPChoice[];
  instruction_self_improve?: boolean;
  scheduled_for?: string;
  recurrence?: string;
  files?: string[];
};

export type DashboardStats = {
  total_nodes?: number;
  active_nodes?: number;
  pending_tasks?: number;
  running_tasks?: number;
  completed_tasks_today?: number;
  failed_tasks_today?: number;
};

export type Paginated<T> = { data: T[]; total: number; limit: number; offset: number };

// The MCP server catalog row. Mirrors chat's /mcp-servers row + the per-server
// credential-account names (never secret values).
export type McpServer = {
  name: string;
  description?: string;
  tool_count?: number;
  enabled?: boolean;
  accounts?: string[];
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
  nodes: () => request<Paginated<Node>>("/nodes?limit=100&offset=0"),
  tasks: (qs: string) => request<Paginated<Task>>(`/tasks${qs ? `?${qs}` : ""}`),
  createTask: (body: TaskCreate) =>
    request<Task>("/tasks", { method: "POST", body: JSON.stringify(body) }),
  taskLogs: (taskId: string) => request<LogSession>(`/logs/${encodeURIComponent(taskId)}`),
  config: () => request<{ version?: string; timezone?: string }>("/config"),
  me: () => request<{ authenticated: boolean; username?: string; role?: string }>("/me"),

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
