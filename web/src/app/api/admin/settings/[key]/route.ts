import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/settings/{key} — proxy to chat-server's PUT/DELETE
 * /admin/settings/{key}: set or reset one workspace feature setting override.
 * Upstream enforces admin authorization and registry validation (404 unknown
 * key, 400 invalid value), passed through verbatim.
 */

async function proxy(email: string, key: string, init?: RequestInit) {
  const { upstream, error } = await chatServerProxy(
    email,
    `/admin/settings/${encodeURIComponent(key)}`,
    init,
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function PUT(request: NextRequest, { params }: { params: Promise<{ key: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  // Defense-in-depth on top of the SameSite=Lax session cookie — these
  // settings steer workspace-wide agent behavior (PII redaction, delegation).
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { key } = await params;
  const body = await request.text();
  return proxy(session.email, key, { method: "PUT", body });
}

export async function DELETE(
  request: NextRequest,
  { params }: { params: Promise<{ key: string }> },
) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { key } = await params;
  return proxy(session.email, key, { method: "DELETE" });
}
