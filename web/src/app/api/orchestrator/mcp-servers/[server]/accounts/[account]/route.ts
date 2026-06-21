import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../../../../_lib/proxy";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ server: string; account: string }> };

// PUT    /api/orchestrator/mcp-servers/{server}/accounts/{account}
//   Sets/updates the account's secrets. Body: { secrets: {KEY: value} }.
//   WRITE-ONLY: secret values go upstream into the 0600 env file and are never
//   read back.
// DELETE /api/orchestrator/mcp-servers/{server}/accounts/{account}
//   Removes the account's <VAR>_<ACCOUNT> keys.
export async function PUT(request: NextRequest, context: RouteContext) {
  const { server, account } = await context.params;
  return proxyToOrchestrator(
    request,
    `/mcp-servers/${encodeURIComponent(server)}/accounts/${encodeURIComponent(account)}`,
  );
}

export async function DELETE(request: NextRequest, context: RouteContext) {
  const { server, account } = await context.params;
  return proxyToOrchestrator(
    request,
    `/mcp-servers/${encodeURIComponent(server)}/accounts/${encodeURIComponent(account)}`,
  );
}
