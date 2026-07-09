import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string; grantee: string }> };

// DELETE /api/remote-mcp-servers/{id}/shares/{grantee} — revoke a share.
// Companion to the POST proxy in ../route.ts (both were missing while the
// Connections page already called them). Revocation applies immediately
// upstream; owner-scoping is enforced there.
export async function DELETE(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id, grantee } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/remote-mcp-servers/${encodeURIComponent(id)}/shares/${encodeURIComponent(grantee)}`,
    { method: "DELETE" },
  );
  if (error) return error;
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
