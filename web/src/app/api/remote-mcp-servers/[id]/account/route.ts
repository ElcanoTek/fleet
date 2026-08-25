import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ id: string }> };

// PUT /api/remote-mcp-servers/{id}/account — rename a seat's account label
// (#988). Body: { "account": "work" }. Owner-only; the backend canonicalizes
// the label (trim, lowercase, spaces/hyphens → _) and answers 204, or 400
// with a readable message the page shows verbatim.
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
    `/remote-mcp-servers/${encodeURIComponent(id)}/account`,
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
