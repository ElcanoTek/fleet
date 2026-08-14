import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Context = { params: Promise<{ budgetId: string }> };

// DELETE /api/orchestrator/admin/budgets/{id} → orchestrator DELETE /admin/budgets/{id}
export async function DELETE(request: NextRequest, context: Context) {
  const { budgetId } = await context.params;
  return proxyToOrchestrator(request, `/admin/budgets/${encodeURIComponent(budgetId)}`);
}
