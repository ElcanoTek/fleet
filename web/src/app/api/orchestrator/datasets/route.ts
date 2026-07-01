import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/datasets → orchestrator GET /datasets (#514).
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/datasets");
}

// POST /api/orchestrator/datasets → orchestrator POST /datasets (create).
export async function POST(request: NextRequest) {
  return proxyToOrchestrator(request, "/datasets");
}
