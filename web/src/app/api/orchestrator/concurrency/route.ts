import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET  /api/orchestrator/concurrency → orchestrator GET /concurrency
//   Returns the global concurrency cap: { max_concurrent_agents, warm_pool_size }.
// PUT  /api/orchestrator/concurrency → orchestrator PUT /concurrency
//   Body: { max_concurrent_agents: number } — the single global cap setting
//   (FLEET_MAX_CONCURRENT_AGENTS) bounding simultaneous agents across both
//   interactive and scheduled modes.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/concurrency");
}

export async function PUT(request: NextRequest) {
  return proxyToOrchestrator(request, "/concurrency");
}
