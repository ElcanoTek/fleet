import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; path: string[] }>;
};

/**
 * GET /api/conversations/:id/team-files/:...path
 *
 * A teammate's download of one SHARED output of a chat shared with their team
 * (docs/TEAM-SHARING.md, ADR-0079) — the first cross-user file read. Every
 * gate lives in the Go handler (internal/httpapi/team_files.go →
 * handleTeamFile): team-readable right now, a current output, not excluded by
 * the owner, read without following any symlink. Any refusal is a 404.
 *
 * This route is a thin streaming proxy, with the same hardening as the owner's
 * workspace route — re-applied here rather than trusted from upstream, because
 * these bytes were written by SOMEONE ELSE'S agent: nosniff, a sandboxing CSP,
 * and active document types (HTML, SVG, XML) forced to download.
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  return proxyTeamFile("GET", context);
}

/**
 * HEAD /api/conversations/:id/team-files/:...path — the same gate and the same
 * headers, no body (the Go handler serves HEAD too). Forwarded as a HEAD
 * rather than left to Next's GET fallback, which would pull the whole file
 * from chat-server only to drop it.
 */
export async function HEAD(_request: NextRequest, context: RouteContext) {
  return proxyTeamFile("HEAD", context);
}

async function proxyTeamFile(method: "GET" | "HEAD", context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, path } = await context.params;

  // Next.js hands us decoded segments; chat-server expects the encoded form.
  const upstreamPath = path.map((seg) => encodeURIComponent(seg)).join("/");

  let upstream: Response;
  try {
    upstream = await chatServerFetch(
      session,
      `/conversations/${encodeURIComponent(conversationId)}/team-files/${upstreamPath}`,
      { method },
    );
  } catch (err) {
    return NextResponse.json(
      { error: `chat-server unreachable: ${(err as Error).message}` },
      { status: 502 },
    );
  }

  if (!upstream.ok || (method === "GET" && !upstream.body)) {
    const text = method === "HEAD" ? null : await upstream.text();
    return new NextResponse(text, { status: upstream.status });
  }

  const headers = new Headers();
  for (const name of ["Content-Type", "Content-Length", "Cache-Control", "Last-Modified", "ETag", "Content-Disposition"]) {
    const v = upstream.headers.get(name);
    if (v) headers.set(name, v);
  }
  headers.set("X-Content-Type-Options", "nosniff");
  headers.set("Content-Security-Policy", "sandbox");
  const contentType = (headers.get("Content-Type") ?? "").toLowerCase();
  const activeContent = ["text/html", "image/svg+xml", "application/xhtml+xml", "text/xml", "application/xml"];
  if (!contentType || activeContent.some((t) => contentType.startsWith(t))) {
    const filename = path.at(-1) ?? "download";
    headers.set(
      "Content-Disposition",
      `attachment; filename*=UTF-8''${encodeURIComponent(filename)}`,
    );
  }
  return new NextResponse(method === "HEAD" ? null : upstream.body, { status: 200, headers });
}
