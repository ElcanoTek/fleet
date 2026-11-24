// Which account (credential seat) each connector uses in a conversation.
//
// A row's `account` is the account it runs as ("" / absent = the default).
// A bundled connector can run as several accounts at once
// (docs/MULTI-ACCOUNT-CHATS.md): then `selected_accounts` holds the whole
// choice, default ("") first, and `account` is its first entry. A row on one
// account never carries `selected_accounts`, so everything that only knows
// `account` keeps reading what it always read.

/** At most this many accounts of one connector in one chat. The server
 *  enforces it (store.MaxMCPAccountsPerConnector); the picker stops offering
 *  more so nobody meets the refusal. */
export const MAX_ACCOUNTS_PER_CONNECTOR = 6;

/** The part of a catalog row the account helpers read and write. */
export type AccountRow = {
  name: string;
  account?: string;
  selected_accounts?: string[];
  /** The connector's named accounts, in catalog order. */
  accounts?: string[];
  /** A hosted connection: one account per chat. */
  remote?: boolean;
};

/** One connector's choice on the wire: a name, or a list for several. */
export type AccountChoice = string | string[];

/** The accounts a row runs as; never empty. "" is the default account. */
export function selectedAccounts(row: Pick<AccountRow, "account" | "selected_accounts">): string[] {
  const list = row.selected_accounts;
  if (list && list.length > 1) return list;
  return [row.account ?? ""];
}

/** A hosted connection mounts one account per chat; a bundled connector with
 *  named accounts can run as several. */
export function allowsSeveralAccounts(row: Pick<AccountRow, "remote" | "accounts">): boolean {
  return !row.remote && (row.accounts?.length ?? 0) > 0;
}

/**
 * The row with its choice replaced. The list is put in the one order the
 * server stores (default first, then the catalog's order), repeats dropped,
 * so the pill and the request read the same whatever order boxes were ticked
 * in. An empty list is the default account: a connector that is on always
 * runs as at least one.
 */
export function withSelectedAccounts<T extends AccountRow>(
  row: T,
  accounts: readonly string[],
): T & Pick<AccountRow, "account" | "selected_accounts"> {
  const wanted = new Set(accounts);
  const ordered: string[] = [];
  if (wanted.has("")) ordered.push("");
  for (const a of row.accounts ?? []) {
    if (wanted.has(a) && !ordered.includes(a)) ordered.push(a);
  }
  // A name the catalog row does not list (a seat removed since) stays last,
  // so the server is the one to refuse it.
  for (const a of accounts) {
    if (!ordered.includes(a)) ordered.push(a);
  }
  const next: T & Pick<AccountRow, "account" | "selected_accounts"> = { ...row, account: ordered[0] ?? "" };
  if (ordered.length > 1) next.selected_accounts = ordered;
  else delete next.selected_accounts;
  return next;
}

/** Whether two rows run as the same accounts. */
export function sameAccounts(
  a: Pick<AccountRow, "account" | "selected_accounts">,
  b: Pick<AccountRow, "account" | "selected_accounts">,
): boolean {
  const x = selectedAccounts(a);
  const y = selectedAccounts(b);
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

/**
 * Tick or untick one account. The last ticked account cannot be unticked (the
 * connector's switch is what turns it off), and nothing is added past the
 * cap; both return the choice unchanged.
 */
export function toggleAccount(current: readonly string[], account: string): string[] {
  if (current.includes(account)) {
    return current.length > 1 ? current.filter((a) => a !== account) : [...current];
  }
  if (current.length >= MAX_ACCOUNTS_PER_CONNECTOR) return [...current];
  return [...current, account];
}

// mcpAccountOverrides collapses the Tools-picker state into the wire map the
// backend takes for credential-seat overrides (#988): server name → chosen
// label, or the list of them for a connector on several accounts, omitting
// servers on their default seat. Shared by the per-conversation POST
// /api/conversations/{id}/mcp-servers (`accounts`) and the first-message
// flush in useTurnStream (`mcp_accounts`) so both send the same shape.
// Structural parameter type so neither caller's module has to import the
// other.
export function mcpAccountOverrides(
  servers: readonly Pick<AccountRow, "name" | "account" | "selected_accounts">[],
): Record<string, AccountChoice> {
  const out: Record<string, AccountChoice> = {};
  for (const s of servers) {
    const list = selectedAccounts(s);
    if (list.length > 1) out[s.name] = list;
    else if (list[0]) out[s.name] = list[0];
  }
  return out;
}

/** The `accounts` object of a save's answer, keeping only values of the two
 *  shapes the server sends. */
export function parseAccountChoices(raw: unknown): Record<string, AccountChoice> | null {
  if (!raw || typeof raw !== "object") return null;
  const out: Record<string, AccountChoice> = {};
  for (const [name, value] of Object.entries(raw as Record<string, unknown>)) {
    if (typeof value === "string") out[name] = value;
    else if (Array.isArray(value) && value.every((v) => typeof v === "string")) out[name] = value as string[];
  }
  return out;
}
