import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/me → orchestrator GET /api/me. The dashboard probes
// this on load to detect a cookie session (an elcano user has no bearer).
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/api/me");
}
