import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ server: string }> };

// GET  /api/orchestrator/mcp-servers/{server}/accounts
//   Lists this server's credential-account names (never secret values).
// POST /api/orchestrator/mcp-servers/{server}/accounts
//   Creates a new credential account. Body: { account, secrets: {KEY: value} }.
//   The secret values are WRITE-ONLY — they're forwarded upstream to be stored
//   in the 0600 env file and are NEVER echoed back by any read endpoint.
export async function GET(request: NextRequest, context: RouteContext) {
  const { server } = await context.params;
  return proxyToOrchestrator(
    request,
    `/mcp-servers/${encodeURIComponent(server)}/accounts`,
  );
}

export async function POST(request: NextRequest, context: RouteContext) {
  const { server } = await context.params;
  return proxyToOrchestrator(
    request,
    `/mcp-servers/${encodeURIComponent(server)}/accounts`,
  );
}
