import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// POST /api/orchestrator/upload → orchestrator POST /upload (multipart).
// The proxy preserves the incoming Content-Type (multipart boundary) and
// streams the raw body, so the file reaches the orchestrator intact.
export async function POST(request: NextRequest) {
  return proxyToOrchestrator(request, "/upload");
}
