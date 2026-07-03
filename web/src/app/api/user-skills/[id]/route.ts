import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
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
    session.email,
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

export async function PUT(req: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return proxy("PUT", id, req);
}

export async function DELETE(_req: Request, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  return proxy("DELETE", id);
}
