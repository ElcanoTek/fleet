// mcpAccountOverrides collapses the Tools-picker state into the wire map the
// backend takes for credential-seat overrides (#988): server name → chosen
// label, omitting servers on their default seat. Shared by the
// per-conversation POST /api/conversations/{id}/mcp-servers (`accounts`) and
// the first-message flush in useTurnStream (`mcp_accounts`) so both send the
// same shape. Structural parameter type so neither caller's module has to
// import the other.
export function mcpAccountOverrides(
  servers: readonly { name: string; account?: string }[],
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const s of servers) {
    if (s.account) out[s.name] = s.account;
  }
  return out;
}
