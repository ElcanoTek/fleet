import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; path: string[] }>;
};

/**
 * GET /api/conversations/:id/workspace/:...path
 *
 * Streams a file from the conversation's per-turn workspace dir on
 * chat-server. Used by the markdown img interceptor in the chat UI:
 * when the agent writes `![chart](spend_chart.png)` and saves the
 * file via run_python, ReactMarkdown's <img> rewrites the relative
 * src to this URL and the browser fetches it through here.
 *
 * Auth + path validation live in chat-server (handleWorkspaceFile);
 * this route is a thin proxy that forwards the user identity and
 * preserves Content-Type / Cache-Control / Last-Modified so the
 * browser can cache and the image renders without sniffing.
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, path } = await context.params;

  // Re-encode each segment so spaces / parens / unicode in agent-chosen
  // filenames survive — Next.js gives us the decoded segments already,
  // and chat-server expects the URL-encoded form on the wire.
  const upstreamPath = path.map((seg) => encodeURIComponent(seg)).join("/");

  let upstream: Response;
  try {
    upstream = await chatServerFetch(
      session.email,
      `/conversations/${encodeURIComponent(conversationId)}/workspace/${upstreamPath}`,
      { method: "GET" },
    );
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
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
  // report.html with a <script>, links it, the user opens it in a new
  // tab — and the script runs with the chat session's cookies. Force
  // those to download instead. Images/CSV/JSON/etc. still render inline
  // so the markdown <img> path keeps working. nosniff stops the browser
  // from "upgrading" a mislabeled type into one of the active ones.
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
