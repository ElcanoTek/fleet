import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/tasks/recipients → orchestrator GET /tasks/recipients:
// the addresses visible tasks already email their reports to, which the task
// form's Email results field offers as suggestions.
//
// Like `tags`, the static `recipients` segment wins over the sibling
// `[taskId]` route, so this does not shadow GET /tasks/{id}.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/tasks/recipients");
}
