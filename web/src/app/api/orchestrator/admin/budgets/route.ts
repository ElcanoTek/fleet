import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET  /api/orchestrator/admin/budgets → orchestrator GET  /admin/budgets
// POST /api/orchestrator/admin/budgets → orchestrator POST /admin/budgets
// Admin-only on the orchestrator side; the proxy resolves the caller's identity.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/admin/budgets");
}

export async function POST(request: NextRequest) {
  return proxyToOrchestrator(request, "/admin/budgets");
}
