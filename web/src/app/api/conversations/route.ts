import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

export async function GET(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  // Forward the ?archived=true filter (#282) — the archived sidebar section
  // relies on it. Without this the param is dropped and the backend returns the
  // active list, so the archived view would silently show active conversations.
  // Forward ?scope=team too (ADR-0057): the rail's per-project "N shared by
  // your team" count is reduced from this one read, and without the param the
  // backend would answer with the caller's OWN active list — i.e. a count of
  // the wrong population, silently.
  const archived = request.nextUrl.searchParams.get("archived") === "true";
  const teamScope = request.nextUrl.searchParams.get("scope") === "team";
  const path = teamScope
    ? "/conversations?scope=team"
    : archived
      ? "/conversations?archived=true"
      : "/conversations";
  const { upstream, error } = await chatServerProxy(session, path, { method: "GET" });
  if (error) return error;
  return passthrough(upstream);
}

export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session, "/conversations", {
    method: "POST",
    body,
  });
  if (error) return error;
  return passthrough(upstream);
}

/**
 * DELETE /api/conversations — bulk conversation delete (#279).
 *
 * Forwards the request body (conversation_ids / all_matching / confirm) so the
 * backend's targeted-delete path receives it. With no body, the backend falls
 * back to the legacy "delete all unpinned" behavior (back-compat for older
 * clients). The ?label= query param is already part of the URL and passes
 * through unchanged.
 */
export async function DELETE(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session, "/conversations", {
    method: "DELETE",
    body: body.length > 0 ? body : undefined,
  });
  if (error) return error;
  return passthrough(upstream);
}

async function passthrough(upstream: Response) {
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
