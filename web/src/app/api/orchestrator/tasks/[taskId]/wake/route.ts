import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// POST /api/orchestrator/tasks/{id}/wake → orchestrator POST /tasks/{id}/wake
// Fire a named event at a task parked by wake_on_event (docs/SELF-WAKE.md).
export async function POST(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}/wake`);
}
