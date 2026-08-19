import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// DELETE /api/orchestrator/tasks/{id}/permanent → orchestrator
// DELETE /tasks/{id}/permanent (permanently remove a task + its run history;
// a deliberately separate route from the cancel DELETE on the parent path).
//
// This proxy is per-route by design — there is no catch-all — so the backend
// route shipping (#1174) without this file made every click 404 at the Next
// tier before the orchestrator ever saw it.
export async function DELETE(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(
    request,
    `/tasks/${encodeURIComponent(taskId)}/permanent`,
  );
}
