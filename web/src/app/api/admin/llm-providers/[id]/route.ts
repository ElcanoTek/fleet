import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/llm-providers/{id} — proxy to chat-server's
 * PUT/DELETE /admin/llm-providers/{id}. Upstream enforces admin authorization
 * and validation (400 invalid row, 404 unknown id), passed through verbatim.
 */

async function proxy(email: string, id: string, init?: RequestInit) {
  const { upstream, error } = await chatServerProxy(
    email,
    `/admin/llm-providers/${encodeURIComponent(id)}`,
    init,
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function PUT(request: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  const body = await request.text();
  return proxy(session.email, id, { method: "PUT", body });
}

export async function DELETE(request: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  return proxy(session.email, id, { method: "DELETE" });
}
