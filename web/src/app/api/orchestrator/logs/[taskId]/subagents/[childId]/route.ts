import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string; childId: string }> };

// GET /api/orchestrator/logs/{id}/subagents/{childId} → orchestrator
// GET /logs/{id}/subagents/{childId} (#1043): a spawned child's own transcript
// (LogSession), gated server-side by the task transcript gate + linkage check.
export async function GET(request: NextRequest, context: RouteContext) {
  const { taskId, childId } = await context.params;
  return proxyToOrchestrator(
    request,
    `/logs/${encodeURIComponent(taskId)}/subagents/${encodeURIComponent(childId)}`,
  );
}
