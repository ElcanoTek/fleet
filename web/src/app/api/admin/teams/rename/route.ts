import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/teams/rename — proxy to chat-server's POST /admin/teams/rename:
 * relabel a team across users AND team-shared projects in one transaction.
 * Upstream enforces admin authorization; a non-admin sees the 403 pass
 * through. Mutating route → same-origin check like every other write proxy.
 */
export async function POST(req: NextRequest) {
  const origin = verifyOrigin(req);
  if (origin) return origin;
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { upstream, error } = await chatServerProxy(
    session,
    "/admin/teams/rename",
    {
      method: "POST",
      body: await req.text(),
      headers: { "Content-Type": "application/json" },
    },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: {
      "Content-Type":
        upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}
