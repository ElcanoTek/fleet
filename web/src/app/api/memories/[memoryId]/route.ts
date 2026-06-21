import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ memoryId: string }> };

export async function PATCH(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const { memoryId } = await params;
  const bodyText = await request.text();
  const upstream = await chatServerFetch(session.email, `/memories/${encodeURIComponent(memoryId)}`, {
    method: "PATCH",
    body: bodyText,
  });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function DELETE(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const { memoryId } = await params;
  const upstream = await chatServerFetch(session.email, `/memories/${encodeURIComponent(memoryId)}`, {
    method: "DELETE",
  });
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });

  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "text/plain" },
  });
}
