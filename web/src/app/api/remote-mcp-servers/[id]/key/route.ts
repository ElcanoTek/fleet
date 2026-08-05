import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// PUT /api/remote-mcp-servers/{id}/key — rotate / correct an api_key
// connection's key. The key is write-only: it is sealed at rest server-side
// and never appears in any response.
export async function PUT(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { id } = await context.params;
  const body = await request.text();
  const { upstream, error } = await chatServerProxy(
    session,
    `/remote-mcp-servers/${encodeURIComponent(id)}/key`,
    { method: "PUT", body, headers: { "Content-Type": "application/json" } },
  );
  if (error) return error;
  if (upstream.status === 204) return new NextResponse(null, { status: 204 });
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
