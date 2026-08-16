import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// Project home Sources panel (ProjectHome.tsx): workspace files across the
// caller's own conversations in the project. Privacy scoping and the listing
// cap live in the Go handler (internal/httpapi/projects.go → projectFiles);
// downloads go through the per-conversation workspace streamer, not here.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/files`,
  );
}
