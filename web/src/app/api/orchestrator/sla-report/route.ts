import { NextRequest } from "next/server";
import { proxyToOrchestrator, passThroughQuery } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/sla-report → orchestrator GET /sla-report (SLAReport, #274).
// Admin-only on the orchestrator side; the proxy resolves the caller's identity.
export async function GET(request: NextRequest) {
  const qs = passThroughQuery(request, ["days"]);
  return proxyToOrchestrator(request, `/sla-report${qs}`);
}
