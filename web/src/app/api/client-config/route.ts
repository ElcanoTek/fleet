import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/client-config — proxies chat-server's member-gated /client-config,
// which returns the active client's branding strings and empty-state quick-start
// cards as opaque JSON:
//   { branding: { app_name, login_title, login_tagline, share_title,
//                 share_description },
//     empty_state: { cards: [ ...ProtocolPill JSON... ], protocol_pills: [] } }
// The browser uses this to render client-agnostic branding + empty-state pills
// instead of hardcoded strings. Pure passthrough — no Next.js-side merging.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { upstream, error } = await chatServerProxy(session.email, "/client-config", {
    method: "GET",
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
