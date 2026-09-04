import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ memoryId: string }> };

export async function POST(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const { memoryId } = await params;
  // Body is optional and forwarded verbatim: {"project_id": ...} accepts the
  // proposal into that project's team learnings instead of personal memory
  // (the destination picker on the approval card). Membership is re-checked
  // upstream.
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(session, `/memories/${encodeURIComponent(memoryId)}/accept`, {
    method: "POST",
    body,
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
