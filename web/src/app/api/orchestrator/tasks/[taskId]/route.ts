import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ taskId: string }> };

// GET    /api/orchestrator/tasks/{id} → orchestrator GET /tasks/{id}
// PUT    /api/orchestrator/tasks/{id} → orchestrator PUT /tasks/{id} (edit)
// DELETE /api/orchestrator/tasks/{id} → orchestrator DELETE /tasks/{id} (cancel)
export async function GET(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}`);
}

export async function PUT(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}`);
}

export async function DELETE(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}`);
}
