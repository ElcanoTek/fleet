import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ datasetId: string }> };

// GET /api/orchestrator/datasets/{id}/export → CSV download (#514).
export async function GET(request: NextRequest, { params }: Params) {
  const { datasetId } = await params;
  return proxyToOrchestrator(request, `/datasets/${encodeURIComponent(datasetId)}/export`);
}
