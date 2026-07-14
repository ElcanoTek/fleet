import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// POST /api/orchestrator/tasks/{id}/rerun → orchestrator POST /tasks/{id}/rerun
// (resubmit: a new one-time task copied from this one, optional overrides).
export async function POST(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(
    request,
    `/tasks/${encodeURIComponent(taskId)}/rerun`,
  );
}
