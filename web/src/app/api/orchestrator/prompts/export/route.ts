import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../_lib/proxy";

export const runtime = "nodejs";

export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/prompts/export");
}
