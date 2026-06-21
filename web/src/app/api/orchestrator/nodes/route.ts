import { NextRequest } from "next/server";
import { passThroughQuery, proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/nodes → orchestrator GET /nodes (PaginatedNodes).
export async function GET(request: NextRequest) {
  const qs = passThroughQuery(request, ["limit", "offset"]);
  return proxyToOrchestrator(request, `/nodes${qs}`);
}
