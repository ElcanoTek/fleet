import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

// Admin-only storage reclaim: deletes old unpinned conversations and/or
// sweeps aged upload + temp files. The upstream handler enforces the
// protections (pinned/archived/shared/project chats are never touched);
// this proxy just forwards the JSON body.
export async function POST(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const body = await request.text();
  return chatServerPassthrough(session.email, "/admin/storage/cleanup", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
  });
}
