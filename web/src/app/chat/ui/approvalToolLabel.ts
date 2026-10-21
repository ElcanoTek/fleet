// approvalToolLabel — the words an approval card uses for the tool a call
// runs, so a trader never reads a raw model-facing id ("mcp_ix_execute_plan")
// on a card.
//
// The approval row carries the tool's id and its connector (mcpServer); the
// connector's human name is the bundle's display_name, which the chat already
// has from the Tools picker catalog. ChatExperience provides those names on
// ConnectorNamesContext; without one (tests, a card rendered outside the
// chat) the label falls back to fleet's known SSP names and then to the
// humanised connector id. The raw id still appears where it is meant to be
// read: a card's Details, and the prompts the agent receives.

import { createContext } from "react";

import { sspConnector } from "./history";

/** Connector id → the bundle's display_name, for the chat's connectors. */
export const ConnectorNamesContext = createContext<ReadonlyMap<string, string>>(new Map());

/** The display-name map ConnectorNamesContext carries, from catalog rows. */
export function connectorNames(rows: readonly { name: string; display_name?: string }[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const r of rows) {
    const label = r.display_name?.trim();
    if (label) out.set(r.name, label);
  }
  return out;
}

function humanize(token: string): string {
  const spaced = token.replace(/[_-]+/g, " ").replace(/\s+/g, " ").trim();
  return spaced ? spaced.charAt(0).toUpperCase() + spaced.slice(1) : "";
}

/** A connector's human name when one is known: the bundle's, else fleet's SSP name. */
export function knownConnectorLabel(server: string, names?: ReadonlyMap<string, string>): string | undefined {
  return names?.get(server) ?? sspConnector(server)?.display;
}

/** A connector's human name: the bundle's, fleet's known SSP name, or the humanised id. */
export function connectorLabel(server: string, names?: ReadonlyMap<string, string>): string {
  return knownConnectorLabel(server, names) ?? humanize(connectorBase(server));
}

// connectorBase drops the conventional `_mcp` suffix a bundle's server id
// carries (`magnite_mcp`, `openx_mcp`): the word a connector's tools repeat
// and its name read from is the part before it.
function connectorBase(server: string): string {
  return server.endsWith("_mcp") && server.length > "_mcp".length ? server.slice(0, -"_mcp".length) : server;
}

// canonicalAccount folds an account name the way fleet registers its seat
// (internal/creds.CanonicalAccount: hyphen and space become underscore), so
// `client-a` finds the `mcp_<server>_client_a_<tool>` registration.
function canonicalAccount(account: string): string {
  return account.trim().replace(/[- ]/g, "_");
}

// isShortPrefixOf reports whether token is the short form SSP tools repeat
// for their connector: 2-3 letters, the connector's first letter, the rest in
// order (ix ⊂ indexexchange, pm ⊂ pubmatic, ox ⊂ openx, tl ⊂ triplelift).
function isShortPrefixOf(token: string, server: string): boolean {
  if (token.length < 2 || token.length > 3 || token[0] !== server[0]) return false;
  let i = 0;
  for (const ch of server) if (ch === token[i]) i++;
  return i === token.length;
}

export type ApprovalToolLabel = {
  /** What the call does, humanised: "Execute plan", "Send email". */
  action: string;
  /** The connector's human name, for an MCP tool: "Index Exchange". */
  connector?: string;
};

/**
 * The connector and tool of an `mcp_<server>_<tool>` id whose row names no
 * connector: the first segment, with the conventional `_mcp` suffix kept on
 * the connector (`mcp_magnite_mcp_magnite_get_deal` is magnite_mcp's
 * `magnite_get_deal`).
 */
export function guessMcpTool(tool: string): { server: string; rest: string } | undefined {
  const m = /^mcp_([^_]+(?:_mcp(?=_.))?)_(.+)$/.exec(tool);
  return m ? { server: m[1], rest: m[2] } : undefined;
}

/**
 * The connector of an `mcp_<server>_<tool>` id: the row's mcpServer when the
 * id carries it; for an older row with none, the longest connector the chat
 * knows that prefixes the id (so `mcp_deal_sheet_…` is deal_sheet); else the
 * first segment (guessMcpTool). Every label on a card reads the connector
 * from here, so the title, the checkbox and Ask again split an id one way.
 */
export function mcpToolServer(tool: string, server?: string, names?: ReadonlyMap<string, string>): string | undefined {
  if (server && tool.startsWith(`mcp_${server}_`)) return server;
  let best: string | undefined;
  for (const k of names?.keys() ?? []) {
    if (tool.startsWith(`mcp_${k}_`) && k.length > (best?.length ?? 0)) best = k;
  }
  return best ?? guessMcpTool(tool)?.server;
}

/**
 * The human label for an approval's tool. `server` is the approval's
 * mcpServer when it has one: it splits `mcp_<server>_<tool>` exactly even
 * when the connector id holds an underscore (`mcp_deal_sheet_next_id`).
 * `account` is the approval's mcpAccount: a named seat registers its tools
 * as `mcp_<server>_<account>_<tool>`, and the seat is not part of the action.
 */
export function approvalToolLabel(
  tool: string,
  server?: string,
  names?: ReadonlyMap<string, string>,
  account?: string,
): ApprovalToolLabel {
  const srv = mcpToolServer(tool, server, names);
  const seatPrefix = srv && account?.trim() ? `mcp_${srv}_${canonicalAccount(account)}_` : "";
  if (srv && seatPrefix && tool.startsWith(seatPrefix) && tool.length > seatPrefix.length) {
    const action = humanize(stripConnectorPrefix(tool.slice(seatPrefix.length), srv)) || "This action";
    return { action, connector: connectorLabel(srv, names) };
  }
  let rest = srv ? tool.slice(`mcp_${srv}_`.length) : tool.replace(/^mcp_/, "");
  if (srv) rest = stripConnectorPrefix(rest, srv);
  const action = humanize(rest) || "This action";
  return srv ? { action, connector: connectorLabel(srv, names) } : { action };
}

// stripConnectorPrefix drops the connector name SSP tools repeat as a prefix
// (mcp_indexexchange_ix_create_deal, mcp_magnite_mcp_magnite_get_deal),
// matched against the server id without its `_mcp` suffix.
function stripConnectorPrefix(rest: string, server: string): string {
  const base = connectorBase(server);
  const first = rest.split("_")[0];
  const prefixes = [...(sspConnector(server)?.toolPrefixes ?? []), `${server}_`, `${base}_`];
  if (first !== rest && isShortPrefixOf(first, base)) prefixes.push(`${first}_`);
  for (const p of prefixes) {
    if (rest.startsWith(p) && rest.length > p.length) return rest.slice(p.length);
  }
  return rest;
}

/**
 * The batch-approval checkbox's words (#300): the choice covers every call
 * of this tool in the conversation, so it names the tool, never one call.
 */
export function applyAllText(label: ApprovalToolLabel): string {
  const what = label.connector ? `${label.connector} “${label.action}”` : `“${label.action}”`;
  return `Apply my choice to every ${what} request in this chat`;
}
