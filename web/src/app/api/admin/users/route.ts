import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy, type SessionIdentity } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/users — proxy to chat-server's /admin/users (#237). GET lists
 * accounts; POST creates one (full user management moved into the UI). The
 * upstream handler enforces admin authorization (ADMIN_EMAILS env allowlist OR
 * a users.role='admin' account), so we don't duplicate that check here; a
 * non-admin simply sees a 403 passed through. The create password travels one
 * way (browser → server) and never appears in any response.
 */

async function proxy(user: SessionIdentity, init?: RequestInit) {
  const { upstream, error } = await chatServerProxy(user, "/admin/users", init);
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
  // Defense-in-depth on top of the SameSite=Lax session cookie — account
  // creation is a security-sensitive write.
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const body = await request.text();
  return proxy(session, { method: "POST", body });
}
