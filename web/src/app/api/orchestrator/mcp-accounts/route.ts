import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/mcp-accounts
//
// Flat catalog of every (server, account) credential seat across all servers,
// names only — never secret values. Convenience companion to
// /api/orchestrator/mcp-servers for the credential-account admin table.
//   { accounts: [{ server, account }] }
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/mcp-accounts");
}
