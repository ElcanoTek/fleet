import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ taskId: string }> };

// DELETE .../learned-instructions/active → orchestrator (#516 deactivate).
export async function DELETE(request: NextRequest, { params }: Params) {
  const { taskId } = await params;
  return proxyToOrchestrator(
    request,
    `/tasks/${encodeURIComponent(taskId)}/learned-instructions/active`,
  );
}
