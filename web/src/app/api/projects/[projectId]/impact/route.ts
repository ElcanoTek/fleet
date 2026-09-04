import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// What deleting this project would cost — the counts the delete confirm quotes
// (team learnings lost, chats detached, members affected). Membership-gated
// upstream like every other project subresource.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(session, `/projects/${encodeURIComponent(projectId)}/impact`);
}
