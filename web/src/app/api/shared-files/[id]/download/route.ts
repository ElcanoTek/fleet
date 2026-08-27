import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ id: string }>;
};

/**
 * GET /api/shared-files/{id}/download
 *
 * Streams a shared library file's bytes from chat-server. Any member may
 * download; upstream sets Content-Disposition: attachment with the stored
 * filename, and forwarding it (rather than letting the browser derive a
 * name from the URL) is what makes the save dialog say "report.pdf"
 * instead of "download". Content-Length is safe to forward here because
 * the file stream negotiates no encoding (the same reasoning as the
 * orchestrator workspace download proxy).
 */
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id } = await context.params;

  let upstream: Response;
  try {
    upstream = await chatServerFetch(
      session,
      `/shared-files/${encodeURIComponent(id)}/download`,
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
  for (const name of ["Content-Type", "Content-Disposition", "Content-Length"]) {
    const v = upstream.headers.get(name);
    if (v) headers.set(name, v);
  }
  // Library files are member-visible uploads served from OUR origin, and
  // upstream already forces attachment disposition; nosniff keeps a browser
  // from second-guessing a mislabeled type into something executable.
  headers.set("X-Content-Type-Options", "nosniff");
  return new NextResponse(upstream.body, { status: 200, headers });
}
