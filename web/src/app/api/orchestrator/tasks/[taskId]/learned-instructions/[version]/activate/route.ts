import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ taskId: string; version: string }> };

// POST .../learned-instructions/{version}/activate → orchestrator (#516).
export async function POST(request: NextRequest, { params }: Params) {
  const { taskId, version } = await params;
  return proxyToOrchestrator(
    request,
    `/tasks/${encodeURIComponent(taskId)}/learned-instructions/${encodeURIComponent(version)}/activate`,
  );
}
