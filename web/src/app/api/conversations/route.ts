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
  const archived = request.nextUrl.searchParams.get("archived") === "true";
  const path = archived ? "/conversations?archived=true" : "/conversations";
  const { upstream, error } = await chatServerProxy(session.email, path, { method: "GET" });
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
  const { upstream, error } = await chatServerProxy(session.email, "/conversations", {
    method: "POST",
    body,
  });
  if (error) return error;
  return passthrough(upstream);
}

/** DELETE /api/conversations → delete every unpinned conversation for the user. */
export async function DELETE(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { upstream, error } = await chatServerProxy(session.email, "/conversations", {
    method: "DELETE",
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
