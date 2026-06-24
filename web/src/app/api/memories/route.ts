import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

export async function GET() {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const { upstream, error } = await chatServerProxy(session.email, "/memories", { method: "GET" });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });

  const bodyText = await request.text();
  const { upstream, error } = await chatServerProxy(session.email, "/memories", {
    method: "POST",
    body: bodyText,
  });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
