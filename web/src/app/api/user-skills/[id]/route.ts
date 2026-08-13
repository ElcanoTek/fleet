import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { verifyOrigin } from "@/app/lib/csrf";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// /api/user-skills/{id} — PUT updates (full replace incl. status), DELETE removes.
async function proxy(method: "PUT" | "DELETE", id: string, req?: Request) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const init: { method: string; body?: string; headers?: Record<string, string> } = { method };
  if (req && method === "PUT") {
    init.body = await req.text();
    init.headers = { "Content-Type": "application/json" };
  }
  const { upstream, error } = await chatServerProxy(
    session,
    `/user-skills/${encodeURIComponent(id)}`,
    init,
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text.length > 0 ? text : null, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function PUT(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  // Same-origin only — see the POST route's note; a forged PUT could rewrite
  // an existing skill's body, which the agent reads mid-turn.
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  return proxy("PUT", id, req);
}

export async function DELETE(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  return proxy("DELETE", id);
}
