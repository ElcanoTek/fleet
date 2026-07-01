import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ datasetId: string }> };

// POST /api/orchestrator/datasets/{id}/rerun → orchestrator POST (#514).
export async function POST(request: NextRequest, { params }: Params) {
  const { datasetId } = await params;
  return proxyToOrchestrator(request, `/datasets/${encodeURIComponent(datasetId)}/rerun`);
}
