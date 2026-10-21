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

/** A connector's human name: the bundle's, fleet's known SSP name, or the humanised id. */
export function connectorLabel(server: string, names?: ReadonlyMap<string, string>): string {
  return names?.get(server) ?? sspConnector(server)?.display ?? humanize(server);
}

export type ApprovalToolLabel = {
  /** What the call does, humanised: "Execute plan", "Send email". */
  action: string;
  /** The connector's human name, for an MCP tool: "Index Exchange". */
  connector?: string;
};

/**
 * The human label for an approval's tool. `server` is the approval's
 * mcpServer when it has one: it splits `mcp_<server>_<tool>` exactly even
 * when the connector id holds an underscore (`mcp_deal_sheet_next_id`).
 */
export function approvalToolLabel(
  tool: string,
  server?: string,
  names?: ReadonlyMap<string, string>,
): ApprovalToolLabel {
  let srv = server && tool.startsWith(`mcp_${server}_`) ? server : undefined;
  let rest = srv ? tool.slice(`mcp_${srv}_`.length) : tool;
  if (!srv) {
    const m = /^mcp_([^_]+)_(.+)$/.exec(tool);
    if (m) {
      srv = m[1];
      rest = m[2];
    } else {
      rest = tool.replace(/^mcp_/, "");
    }
  }
  if (srv) {
    // SSP tools repeat their connector as a short prefix
    // (mcp_indexexchange_ix_create_deal, mcp_magnite_magnite_get_deal).
    for (const p of [...(sspConnector(srv)?.toolPrefixes ?? []), `${srv}_`]) {
      if (rest.startsWith(p) && rest.length > p.length) {
        rest = rest.slice(p.length);
        break;
      }
    }
  }
  const action = humanize(rest) || "This action";
  return srv ? { action, connector: connectorLabel(srv, names) } : { action };
}

/**
 * The batch-approval checkbox's words (#300): the choice covers every call
 * of this tool in the conversation, so it names the tool, never one call.
 */
export function applyAllText(label: ApprovalToolLabel): string {
  const what = label.connector ? `${label.connector} “${label.action}”` : `“${label.action}”`;
  return `Apply my choice to every ${what} request in this chat`;
}
