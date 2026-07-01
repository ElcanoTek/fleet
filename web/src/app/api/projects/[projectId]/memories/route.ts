import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// Shared project memory (#509): list + add.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  const { upstream, error } = await chatServerProxy(session.email, `/projects/${encodeURIComponent(projectId)}/memories`);
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function POST(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  const { upstream, error } = await chatServerProxy(session.email, `/projects/${encodeURIComponent(projectId)}/memories`, {
    method: "POST",
    body: await request.text(),
  });
  if (error) return error;
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
