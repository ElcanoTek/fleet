import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

/**
 * PATCH /api/admin/users/{email} — proxy to chat-server's
 * PATCH /admin/users/{email} (#237) to assign a role and/or team. The upstream
 * handler enforces admin authorization and validates the body (400 invalid
 * role, 404 unknown user), passed through verbatim.
 */
export async function PATCH(request: NextRequest, { params }: { params: Promise<{ email: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { email } = await params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/admin/users/${encodeURIComponent(email)}`,
    { method: "PATCH", body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
