import { NextRequest } from "next/server";
import { proxyToOrchestrator, passThroughQuery } from "../../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/admin/usage/adoption → orchestrator
// GET /admin/usage/adoption (AdoptionReport). Admin-only on the orchestrator
// side; the proxy resolves the caller's identity.
export async function GET(request: NextRequest) {
  const qs = passThroughQuery(request, ["from", "to", "format"]);
  return proxyToOrchestrator(request, `/admin/usage/adoption${qs}`);
}
