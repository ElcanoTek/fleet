import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ taskId: string }> };

// POST /api/orchestrator/tasks/{id}/feedback → orchestrator (#516).
export async function POST(request: NextRequest, { params }: Params) {
  const { taskId } = await params;
  return proxyToOrchestrator(request, `/tasks/${encodeURIComponent(taskId)}/feedback`);
}
