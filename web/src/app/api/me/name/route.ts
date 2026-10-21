import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/me/name — proxy to chat-server's /me/name (ADR-0087): the caller's own
 * first name, the value a bundle connector receives as ${FLEET_USER_NAME}
 * (for example the "Hi Elyse," of a deal-sheet email). GET returns the /me
 * record { email, role, team_id, admin, given_name }; PUT { given_name } saves
 * it ("" clears it, and connectors then say "Hi team,"). Fleet's sign-in
 * carries no name, so this is the only place one comes from. Every status and
 * message is passed through verbatim (a refused value is a 400 whose text says
 * why).
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
  const { upstream, error } = await chatServerProxy(session, "/me/name");
  if (error) return error;
  return passthrough(upstream, await upstream.text());
}

export async function PUT(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { upstream, error } = await chatServerProxy(session, "/me/name", {
    method: "PUT",
    body: await request.text(),
  });
  if (error) return error;
  return passthrough(upstream, await upstream.text());
}
