import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ taskId: string }> };

// GET /api/orchestrator/tasks/{id}/learned-instructions → orchestrator (#516).
export async function GET(request: NextRequest, { params }: Params) {
  const { taskId } = await params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}/learned-instructions`);
}
