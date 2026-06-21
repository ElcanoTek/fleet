import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// GET /api/orchestrator/logs/{id} → orchestrator GET /logs/{id} (LogSession).
export async function GET(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(request, `/logs/${encodeURIComponent(taskId)}`);
}
