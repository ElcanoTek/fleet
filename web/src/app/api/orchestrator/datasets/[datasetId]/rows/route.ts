import { NextRequest } from "next/server";
import { passThroughQuery, proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type Params = { params: Promise<{ datasetId: string }> };

// GET /api/orchestrator/datasets/{id}/rows → orchestrator row list (#514).
export async function GET(request: NextRequest, { params }: Params) {
  const { datasetId } = await params;
  const qs = passThroughQuery(request, ["status", "limit", "offset"]);
  return proxyToOrchestrator(request, `/datasets/${encodeURIComponent(datasetId)}/rows${qs}`);
}

// POST /api/orchestrator/datasets/{id}/rows → row import (JSON or text/csv —
// the proxy preserves Content-Type, so CSV bodies pass through untouched).
export async function POST(request: NextRequest, { params }: Params) {
  const { datasetId } = await params;
  return proxyToOrchestrator(request, `/datasets/${encodeURIComponent(datasetId)}/rows`);
}
