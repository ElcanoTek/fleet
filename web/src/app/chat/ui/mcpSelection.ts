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
