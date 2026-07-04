import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/pii-redaction/install — proxy to chat-server's
 * /admin/pii-redaction/install: one-click build + run + supervise of the
 * Rampart detection service. GET polls the job status; POST starts the
 * install; DELETE removes the managed container.
 */

export async function GET() {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  return chatServerPassthrough(session.email, "/admin/pii-redaction/install", { method: "GET" });
}

async function mutate(request: NextRequest, method: "POST" | "DELETE"): Promise<NextResponse> {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  return chatServerPassthrough(session.email, "/admin/pii-redaction/install", { method });
}

export async function POST(request: NextRequest) {
  return mutate(request, "POST");
}

export async function DELETE(request: NextRequest) {
  return mutate(request, "DELETE");
}
