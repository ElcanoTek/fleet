import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/stats → orchestrator GET /stats (DashboardStats).
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/stats");
}
