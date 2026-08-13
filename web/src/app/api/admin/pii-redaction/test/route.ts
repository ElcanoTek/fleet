import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/pii-redaction/test — proxy to chat-server's
 * POST /admin/pii-redaction/test: run the live PII redactor over a synthetic
 * sample and report engine/mode/findings/latency. No operator data involved.
 */

export async function POST(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  return chatServerPassthrough(session, "/admin/pii-redaction/test", { method: "POST" });
}
