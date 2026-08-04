import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/admin/settings/{key} — proxy to chat-server's PUT/DELETE
 * /admin/settings/{key}: set or reset one workspace feature setting override.
 * Upstream enforces admin authorization and registry validation (404 unknown
 * key, 400 invalid value), passed through verbatim.
 */

// Both mutations share one body: session check, CSRF (defense-in-depth on top
// of the SameSite=Lax cookie — these settings steer workspace-wide agent
// behavior), then the verbatim passthrough.
async function mutate(
  request: NextRequest,
  params: Promise<{ key: string }>,
  method: "PUT" | "DELETE",
): Promise<NextResponse> {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { key } = await params;
  const body = method === "PUT" ? await request.text() : undefined;
  return chatServerPassthrough(session, `/admin/settings/${encodeURIComponent(key)}`, {
    method,
    body,
  });
}

export async function PUT(request: NextRequest, { params }: { params: Promise<{ key: string }> }) {
  return mutate(request, params, "PUT");
}

export async function DELETE(
  request: NextRequest,
  { params }: { params: Promise<{ key: string }> },
) {
  return mutate(request, params, "DELETE");
}
