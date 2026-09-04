import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// The accounts a project can be transferred to: its team's members plus the
// current owner. Membership-gated upstream, emails only — no directory read a
// member does not already have.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(session, `/projects/${encodeURIComponent(projectId)}/members`);
}
