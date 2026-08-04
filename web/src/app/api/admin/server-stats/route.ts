import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// Admin-only read-through to the chat server's dependency-free host snapshot.
// The upstream admin middleware remains the authorization boundary.
export async function GET() {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const { upstream, error } = await chatServerProxy(session, "/admin/server-stats", {
    method: "GET",
  });
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
