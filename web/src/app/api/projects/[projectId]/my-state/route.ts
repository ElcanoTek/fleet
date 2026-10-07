import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// The caller's own UI state for one project (ADR-0079): the getting-started
// card's "Keep personal" and has-shared-a-chat, and which Sources groups are
// open. Stored server-side per user so it follows them across devices.
// Membership is the Go handler's gate (internal/httpapi/projects.go →
// projectMyState); this proxy only authenticates and forwards.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/my-state`,
  );
}

// PUT body: any subset of { kept_personal, sources_open } → the full state.
export async function PUT(request: NextRequest, { params }: Params) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/my-state`,
    { method: "PUT", body: await request.text() },
  );
}
