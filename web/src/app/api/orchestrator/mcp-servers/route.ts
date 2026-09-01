import { NextRequest } from "next/server";
import { proxyToOrchestrator } from "../_lib/proxy";

export const runtime = "nodejs";

// GET /api/orchestrator/mcp-servers
//
// Returns the orchestrator's Optional controls and always-on status, mirroring chat's
// GET /mcp-servers so task and chat connector surfaces share one row contract.
// Response (read-only — NEVER includes secret values):
//   { servers: [{ name, description, tool_count, enabled, accounts: [name...] }] }
// `accounts` is the per-server credential-account catalog (names derived from
// the <VAR>_<ACCOUNT> suffix scan); secret values are never returned.
export async function GET(request: NextRequest) {
  return proxyToOrchestrator(request, "/mcp-servers");
}
