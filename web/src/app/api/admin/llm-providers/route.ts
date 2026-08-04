import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy, type SessionIdentity } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/llm-providers — proxy to chat-server's /admin/llm-providers
 * (admin-managed LLM providers). The upstream handler enforces admin
 * authorization (ADMIN_EMAILS OR users.role='admin'), so a non-admin sees a
 * 403 passed through. API-key values travel one way (browser → server) and
 * never appear in any response.
 */

async function proxy(user: SessionIdentity, init?: RequestInit) {
  const { upstream, error } = await chatServerProxy(user, "/admin/llm-providers", init);
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  return proxy(session, { method: "GET" });
}

export async function POST(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  // Defense-in-depth on top of the SameSite=Lax session cookie — provider rows
  // hold write-only credentials and steer all model traffic.
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const body = await request.text();
  return proxy(session, { method: "POST", body });
}
