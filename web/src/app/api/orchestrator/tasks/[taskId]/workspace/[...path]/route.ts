import { NextRequest, NextResponse } from "next/server";
import { orchestratorFetch } from "@/app/lib/mocServer";
import { resolveOrchestratorAuth } from "../../../../_lib/auth";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ taskId: string; path: string[] }>;
};

/**
 * GET /api/orchestrator/tasks/:taskId/workspace/:...path
 *
 * Streams a single file from a scheduled task's per-run workspace dir on
 * the orchestrator (its existing GET /tasks/{id}/workspace/* endpoint,
 * #287). Used by the task-log viewer's markdown <img>/<a> interceptors:
 * when the agent produces an image with the generate_image tool and the
 * log references it as `![chart](weekly.png)`, ReactMarkdown rewrites the
 * relative src to this URL (via resolveTaskWorkspaceHref) and the browser
 * fetches the bytes through here so the image renders inline (#271).
 *
 * This is deliberately NOT routed through proxyToOrchestrator: that helper
 * buffers the upstream body as text() and re-emits it, which would corrupt
 * binary image bytes. Like the chat workspace proxy
 * (api/conversations/[id]/workspace/[...path]), this streams upstream.body
 * straight through and preserves the content/cache headers so images
 * render without sniffing.
 *
 * The orchestrator handler owns the real security: it gates the workspace
 * to the task's creator (taskWorkspaceOwned, stricter than ordinary task
 * visibility) and runs every path through SafeWorkspaceJoin. This route
 * only forwards the caller's identity and pipes the stream back.
 */
export async function GET(request: NextRequest, context: RouteContext) {
  const auth = await resolveOrchestratorAuth(request);
  if (!auth) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  const { taskId, path } = await context.params;

  // Next.js hands us the decoded segments; the orchestrator expects the
  // URL-encoded form on the wire (its handler percent-decodes once before
  // the path-traversal guard), so re-encode each segment.
  const upstreamPath = path.map((seg) => encodeURIComponent(seg)).join("/");

  let upstream: Response;
  try {
    upstream = await orchestratorFetch(
      auth,
      `/tasks/${encodeURIComponent(taskId)}/workspace/${upstreamPath}`,
      { method: "GET" },
    );
  } catch (err) {
    return NextResponse.json(
      { error: `orchestrator unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  if (!upstream.ok || !upstream.body) {
    const text = await upstream.text();
    return new NextResponse(text, { status: upstream.status });
  }

  const headers = new Headers();
  for (const name of ["Content-Type", "Content-Length", "Cache-Control", "Last-Modified", "ETag"]) {
    const v = upstream.headers.get(name);
    if (v) headers.set(name, v);
  }

  // Workspace files are agent-written content served from OUR origin.
  // Anything the browser would execute as an active document (HTML, SVG,
  // XML…) is a stored-XSS primitive: a prompt-injected agent writes
  // report.html with a <script>, the user opens it in a new tab, and the
  // script runs with the session's cookies. Force those to download
  // instead; images/CSV/JSON still render inline so the markdown <img>
  // path keeps working. nosniff stops the browser from "upgrading" a
  // mislabeled type into one of the active ones. This mirrors the chat
  // workspace proxy's defence verbatim.
  headers.set("X-Content-Type-Options", "nosniff");
  const contentType = (headers.get("Content-Type") ?? "").toLowerCase();
  const activeContent = ["text/html", "image/svg+xml", "application/xhtml+xml", "text/xml", "application/xml"];
  if (activeContent.some((t) => contentType.startsWith(t))) {
    const filename = path.at(-1) ?? "download";
    headers.set(
      "Content-Disposition",
      `attachment; filename*=UTF-8''${encodeURIComponent(filename)}`,
    );
  }
  return new NextResponse(upstream.body, { status: 200, headers });
}
