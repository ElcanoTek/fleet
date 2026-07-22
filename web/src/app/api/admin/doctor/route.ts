import { NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

/**
 * GET /api/admin/doctor — proxy to chat-server's /admin/doctor (the read-only
 * box-health report behind Settings → Admin → Doctor). `?deep=1` is forwarded
 * verbatim; the upstream then also launches a throwaway sandbox container, so
 * the response can take a couple of minutes — the page requests deep mode via
 * an explicit button, never on load. The upstream enforces the admin gate; a
 * non-admin sees the 403 passed through.
 */
export async function GET(req: Request) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const deep = new URL(req.url).searchParams.get("deep");
  const upstreamPath = deep === "1" ? "/admin/doctor?deep=1" : "/admin/doctor";
  const { upstream, error } = await chatServerProxy(session.email, upstreamPath, { method: "GET" });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
