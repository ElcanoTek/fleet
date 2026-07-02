import { NextRequest } from "next/server";
import { passThroughQuery, proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/tasks/upcoming → orchestrator upcoming-runs feed (#504).
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, `/tasks/upcoming${passThroughQuery(request, ["limit"])}`);
}
