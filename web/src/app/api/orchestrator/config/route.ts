import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/config → orchestrator GET /api/config ({version, timezone}).
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/api/config");
}
