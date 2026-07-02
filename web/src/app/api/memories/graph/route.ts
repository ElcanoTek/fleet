import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// Memory knowledge graph (#523): read-only proxy to GET /memories/graph,
// forwarding the as-of / project query parameters verbatim.
export async function GET(request: NextRequest) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const search = request.nextUrl.search;
  const { upstream, error } = await chatServerProxy(session.email, `/memories/graph${search}`, {
    method: "GET",
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
