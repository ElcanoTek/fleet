import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { verifyOrigin } from "@/app/lib/csrf";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// /api/connector-prefs — per-user connector availability preferences (unified
// connector UX). GET lists the caller's explicit choices; PUT upserts one;
// DELETE (kind, id query params) reverts a connector to the operator default.
async function proxy(method: "GET" | "PUT" | "DELETE", req?: Request) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  let path = "/connector-prefs";
  const init: { method: string; body?: string; headers?: Record<string, string> } = { method };
  if (method === "PUT" && req) {
    init.body = await req.text();
    init.headers = { "Content-Type": "application/json" };
  }
  if (method === "DELETE" && req) {
    const q = new URL(req.url).searchParams;
    path += `?kind=${encodeURIComponent(q.get("kind") ?? "")}&id=${encodeURIComponent(q.get("id") ?? "")}`;
  }
  const { upstream, error } = await chatServerProxy(session, path, init);
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text.length > 0 ? text : null, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function GET() {
  return proxy("GET");
}

export async function PUT(req: NextRequest) {
  // Mutating route: same-origin only, matching every other write route.
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;
  return proxy("PUT", req);
}

export async function DELETE(req: NextRequest) {
  const csrf = verifyOrigin(req);
  if (!csrf.ok) return csrf.response;
  return proxy("DELETE", req);
}
