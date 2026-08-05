import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// Auditable project export (#509): config + runtime-state references.
//
// The Content-Disposition filename comes from the Go handler now
// (internal/httpapi/projects.go → exportFilename), which knows the project's
// name; this route used to synthesize `project-<uuid>.json` from the path,
// naming the download after an opaque id. One owner of the filename, and every
// export endpoint behaves the same way.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/export`,
  );
}
