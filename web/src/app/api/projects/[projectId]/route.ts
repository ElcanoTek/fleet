import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

async function forward(request: NextRequest, projectId: string, method: string, withBody: boolean) {
  if (method !== "GET") {
    const csrf = verifyOrigin(request);
    if (!csrf.ok) return csrf.response;
  }
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const init: { method: string; body?: string } = { method };
  if (withBody) init.body = await request.text();
  const { upstream, error } = await chatServerProxy(session.email, `/projects/${encodeURIComponent(projectId)}`, init);
  if (error) return error;
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });
  return new NextResponse(await upstream.text(), {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}

export async function GET(request: NextRequest, { params }: Params) {
  const { projectId } = await params;
  return forward(request, projectId, "GET", false);
}

export async function PATCH(request: NextRequest, { params }: Params) {
  const { projectId } = await params;
  return forward(request, projectId, "PATCH", true);
}

export async function DELETE(request: NextRequest, { params }: Params) {
  const { projectId } = await params;
  return forward(request, projectId, "DELETE", false);
}
