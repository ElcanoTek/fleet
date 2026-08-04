import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/users/{email} — proxy to chat-server's /admin/users/{email}
 * (#237). PATCH assigns a role and/or team; DELETE removes the account (and
 * its Operations Center row — the upstream two-plane semantics). The upstream
 * handler enforces admin authorization and validates (400 invalid role /
 * self-demote / self-delete, 404 unknown user), all passed through verbatim.
 */

async function mutate(request: NextRequest, params: Promise<{ email: string }>, method: "PATCH" | "DELETE") {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  // Defense-in-depth on top of the SameSite=Lax session cookie — role changes
  // and account deletion are security-sensitive writes.
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { email } = await params;
  const body = method === "PATCH" ? await request.text() : undefined;
  const { upstream, error } = await chatServerProxy(
    session,
    `/admin/users/${encodeURIComponent(email)}`,
    { method, body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text.length > 0 ? text : null, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function PATCH(request: NextRequest, { params }: { params: Promise<{ email: string }> }) {
  return mutate(request, params, "PATCH");
}

export async function DELETE(request: NextRequest, { params }: { params: Promise<{ email: string }> }) {
  return mutate(request, params, "DELETE");
}
