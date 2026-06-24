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
  const { upstream, error } = await chatServerProxy(session.email, `/memories/${encodeURIComponent(memoryId)}/accept`, {
    method: "POST",
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
