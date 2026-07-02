import { NextRequest } from "next/server";
import { proxyToOrchestrator, passThroughQuery } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/admin/usage → orchestrator GET /admin/usage
// (UsageReport, #601 part 1). Admin-only on the orchestrator side; the proxy
// resolves the caller's identity.
export async function GET(request: NextRequest) {
  const qs = passThroughQuery(request, ["group_by", "from", "to"]);
  return proxyToOrchestrator(request, `/admin/usage${qs}`);
}
