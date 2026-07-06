import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * PUT /api/admin/users/{email}/password — proxy to chat-server's
 * PUT /admin/users/{email}/password: reset one account's password. The
 * upstream handler enforces admin authorization and validates (400 too-short
 * password, 404 unknown user), passed through verbatim. The new password
 * travels one way (browser → server) and never appears in any response or log.
 */
export async function PUT(request: NextRequest, { params }: { params: Promise<{ email: string }> }) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { email } = await params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/admin/users/${encodeURIComponent(email)}/password`,
    { method: "PUT", body },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text.length > 0 ? text : null, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
