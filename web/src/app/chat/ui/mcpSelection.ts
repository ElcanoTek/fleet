// The chat catalog contains two kinds of rows: optional connectors whose
// enabled state belongs to this conversation, and locked always-on status rows.
// Only the former may cross either persistence boundary (`enabled_optional` on
// the conversation endpoint or the opening /chat request).
export type ChatConnectorSelectionRow = {
  name: string;
  enabled: boolean;
  always_on?: boolean;
};

export function enabledOptionalMcpServerNames(
  servers: readonly ChatConnectorSelectionRow[],
): string[] {
  return servers.filter((server) => server.enabled && !server.always_on).map((server) => server.name);
}

// The server's POST /conversations/{id}/mcp-servers intersects the names it is
// sent with its own catalog and DROPS anything it does not recognize —
// silently, and with a 200, by design ("the server's catalog is the
// authoritative whitelist"). It returns the surviving set as
// `enabled_optional`. The two helpers below let a client treat that response
// as the truth instead of treating "it answered" as the truth.
//
// Why it matters: keeping the optimistic state past that response is how a
// connector ends up reading ON in the Tools picker while every turn runs
// without it. The system prompt's MCP roster then has no `mcp_<name>_*` entry,
// so the agent correctly reports it has no such tools and tells the user to
// enable a connector they can plainly see is already enabled. Both are
// truthful about different state, and nothing reconciles them short of a
// reload.
//
// Both compare case-insensitively: the server canonicalizes names to
// lowercase, so an exact match would turn every mixed-case connector off and
// make the desync worse rather than better.

const lowerSet = (names: readonly string[]): Set<string> =>
  new Set(names.map((name) => name.toLowerCase()));

/**
 * Fold the server's authoritative `enabled_optional` list back into the local
 * rows. Always-on rows are returned untouched: they are informational status,
 * never part of the conversation's opt-in selection, and the server never
 * echoes them back.
 */
export function reconcileMcpSelection<T extends ChatConnectorSelectionRow>(
  rows: readonly T[],
  persistedEnabledOptional: readonly string[],
): T[] {
  const persisted = lowerSet(persistedEnabledOptional);
  return rows.map((row) =>
    row.always_on
      ? row
      : { ...row, enabled: persisted.has(row.name.toLowerCase()) },
  );
}

/**
 * The requested names the server did not persist — connectors its catalog does
 * not know. A name reaching here is usually one added or renamed in the bundle
 * since boot, which `fleet mcp reload` resolves.
 */
export function droppedOptionalMcpServerNames(
  requested: readonly string[],
  persistedEnabledOptional: readonly string[],
): string[] {
  const persisted = lowerSet(persistedEnabledOptional);
  return requested.filter((name) => !persisted.has(name.toLowerCase()));
}
