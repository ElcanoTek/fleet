import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/skills
// Returns the client bundle's Agent Skill roster (name + description per
// skill) from the chat server's member-gated /skills endpoint (#513 phase 1).
// The composer fetches this once at startup to drive the "/" autocomplete;
// the roster is bundle-owned deployment content, not per-user state.
export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { upstream, error } = await chatServerProxy(session.email, "/skills", {
    method: "GET",
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
