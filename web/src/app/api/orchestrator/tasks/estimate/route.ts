import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// POST /api/orchestrator/tasks/estimate → orchestrator POST /tasks/estimate
// (issue #233). Same body as POST /tasks; returns a token + cost forecast
// without creating the task. Pure proxy — no transformation.
export async function POST(request: NextRequest) {
  return proxyToOrchestrator(request, "/tasks/estimate");
}
