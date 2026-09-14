import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/tasks/tags → orchestrator GET /tasks/tags (#212): the
// distinct tags in use, busiest first. Feeds the board's tag filter, which
// needs the tags that exist rather than only those on the page in front of you.
//
// The static `tags` segment wins over the sibling `[taskId]` route, so this
// does not shadow GET /tasks/{id} — the same ordering the Go router spells out
// explicitly in cmd/fleet/main.go.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/tasks/tags");
}
