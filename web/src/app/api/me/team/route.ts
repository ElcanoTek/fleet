import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/me/team — proxy to chat-server's /me/team (#1157): the caller's own
 * role/team, and the self-serve team write. GET returns
 * { email, role, team_id, admin } plus what LEAVING would cost —
 * { shared_projects, shared_chats } — so the Leave confirm can state the
 * consequences before acting rather than reporting them afterwards
 * (ADR-0057). PUT { team_id } creates a team (or leaves one with ""), and
 * leaving also unshares the chats this user shared into that team's projects.
 * Joining a team that already has members is refused upstream with 409 — team
 * membership is what exposes team-shared projects and team-visible
 * conversations, so it stays admin-granted (ADR-0047). Every status and
 * message is passed through verbatim.
 */

function passthrough(upstream: Response, body: string) {
  return new NextResponse(body.length > 0 ? body : null, {
    status: upstream.status,
    headers: {
      "Content-Type": upstream.headers.get("Content-Type") ?? "application/json",
    },
  });
}

export async function GET() {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { upstream, error } = await chatServerProxy(session, "/me/team");
  if (error) return error;
  return passthrough(upstream, await upstream.text());
}

export async function PUT(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { upstream, error } = await chatServerProxy(session, "/me/team", {
    method: "PUT",
    body: await request.text(),
  });
  if (error) return error;
  return passthrough(upstream, await upstream.text());
}
