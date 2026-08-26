import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

/**
 * /api/shared-files/{id} — proxy to chat-server's PATCH/DELETE
 * /shared-files/{id}: rename, move, re-describe, or delete one shared
 * library file. Upstream enforces admin authorization and validation
 * (400 bad name/folder, 404 unknown id, 409 name collision), passed
 * through verbatim.
 */

// Both mutations share one body: session check, CSRF (defense-in-depth on top
// of the SameSite=Lax cookie — the library is staged into every conversation's
// sandbox, so a forged write reaches every agent), then the passthrough.
async function mutate(
  request: NextRequest,
  params: Promise<{ id: string }>,
  method: "PATCH" | "DELETE",
): Promise<NextResponse> {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const { id } = await params;
  const body = method === "PATCH" ? await request.text() : undefined;
  return chatServerPassthrough(session, `/shared-files/${encodeURIComponent(id)}`, {
    method,
    body,
  });
}

export async function PATCH(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  return mutate(request, params, "PATCH");
}

export async function DELETE(
  request: NextRequest,
  { params }: { params: Promise<{ id: string }> },
) {
  return mutate(request, params, "DELETE");
}
