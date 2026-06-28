import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/task-templates
//
// Returns the client bundle's read-only task-template catalog (#262) — the
// pre-filled scheduled-task configurations the task-create form offers as a
// starting point. Each entry carries name/description/icon, a partial-TaskCreate
// `task` payload that seeds the form, and the {variable} placeholder names found
// in its prompt. No template is ever persisted and no task is created here; the
// task is created through the ordinary POST /tasks path.
//   [{ name, description, icon, variables: [name...], task: { ...partial } }]
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/task-templates");
}
