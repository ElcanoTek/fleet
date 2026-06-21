import { NextRequest } from "next/server";
import { passThroughQuery, proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/tasks → orchestrator GET /tasks (PaginatedTasks).
export async function GET(request: NextRequest) {
  const qs = passThroughQuery(request, [
    "limit",
    "offset",
    "status",
    "q",
    "scheduled_only",
    "completed_today",
    "completed_status",
    "created_by",
  ]);
  return proxyToOrchestrator(request, `/tasks${qs}`);
}

// POST /api/orchestrator/tasks → orchestrator POST /tasks (create task).
// Body carries the unified shape (prompt, model, mcp_selection, ...).
export async function POST(request: NextRequest) {
  return proxyToOrchestrator(request, "/tasks");
}
