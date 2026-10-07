import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// Project home Sources panel (ProjectHome.tsx), grouped by chat (ADR-0079):
// the caller's own chats (every non-upload file, flagged output/shared/your
// copy) and teammates' shared chats in this project (their SHARED outputs
// only), plus the legacy flat `files` list of the caller's own files. Privacy
// scoping, the share gates and the listing cap live in the Go handler
// (internal/httpapi/projects.go → projectFiles); downloads go through the
// owner's workspace streamer or the teammate team-files route, not here.
//
// `?focus=<conversationId>` (optional) is forwarded: the chat a "Manage in
// Sources" link is sending the caller to, listed even past the group cap when
// the Go handler's gates show it to them. Only an id-shaped value is
// forwarded; anything else is refused here rather than spliced into the path.
const FOCUS_ID = /^[A-Za-z0-9_-]{1,64}$/;

export async function GET(request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  const focus = request.nextUrl.searchParams.get("focus");
  if (focus !== null && !FOCUS_ID.test(focus)) {
    return NextResponse.json({ error: "invalid focus" }, { status: 400 });
  }
  const query = focus ? `?focus=${encodeURIComponent(focus)}` : "";
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/files${query}`,
  );
}
