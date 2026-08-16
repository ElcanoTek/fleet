import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// Project home chat list with previews (ProjectHome.tsx). Scoping to the
// caller's OWN conversations is enforced by the Go handler
// (internal/httpapi/projects.go → projectConversations); this proxy only
// authenticates and forwards.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/conversations`,
  );
}
