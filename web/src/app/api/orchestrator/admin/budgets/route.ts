import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/admin/budgets → orchestrator GET /admin/budgets
// (BudgetStatus list, #601 part 2). Admin-only on the orchestrator side; the
// proxy resolves the caller's identity. The Usage panel renders the list
// read-only — create/delete are API-only for now.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/admin/budgets");
}
