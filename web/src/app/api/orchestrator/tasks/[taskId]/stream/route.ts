import { NextRequest, NextResponse } from "next/server";
import { orchestratorFetch } from "@/app/lib/mocServer";
import { resolveOrchestratorAuth } from "../../../_lib/auth";

export const runtime = "nodejs";
// SSE must not be cached or statically optimized.
export const dynamic = "force-dynamic";

type RouteContext = { params: Promise<{ taskId: string }> };

// GET /api/orchestrator/tasks/{id}/stream → orchestrator GET /tasks/{id}/stream
// (#508 live activity). Deliberately NOT proxyToOrchestrator: that funnel
// buffers the whole upstream body (`await upstream.text()`), which would hold
// the SSE stream open forever without delivering a byte. This route pipes the
// upstream ReadableStream straight through, forwarding Last-Event-ID so a
// reconnecting client resumes without losing frames.
export async function GET(request: NextRequest, context: RouteContext) {
  const { taskId } = await context.params;

  const auth = await resolveOrchestratorAuth(request);
  if (!auth) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const headers: Record<string, string> = { Accept: "text/event-stream" };
  const lastEventID = request.headers.get("last-event-id");
  if (lastEventID) headers["Last-Event-ID"] = lastEventID;

  let upstream: Response;
  try {
    upstream = await orchestratorFetch(auth, `/tasks/${encodeURIComponent(taskId)}/stream`, {
      headers,
      signal: request.signal,
    });
  } catch (err) {
    return NextResponse.json(
      { error: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text();
    return new NextResponse(text, {
      status: upstream.status,
      headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
    });
  }

  return new NextResponse(upstream.body, {
    status: 200,
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}
