// Account groups in the Tools picker (docs/ACCOUNT-GROUPS.md): the bundle's
// named sets of connector accounts ("The Weather Company" = this account on
// each of these connectors), shown as chips. One click sets every connector
// in a group to the group's accounts. The rules live here, as pure functions
// over the catalog rows, so the popover and the library agree and the rules
// are unit-tested in one place (accountGroups.test.ts).

import {
  MAX_ACCOUNTS_PER_CONNECTOR,
  selectedAccounts,
  withSelectedAccounts,
  type AccountRow,
} from "./mcpAccounts";

/** One {connector, account} of a group. "" is the default account. */
export type AccountGroupMember = {
  server: string;
  account: string;
  /** The server's verdict for this box and user; absent = ok. */
  status?: "ok" | "account_missing" | "connector_missing";
};

export type AccountGroup = {
  id: string;
  name: string;
  accounts: AccountGroupMember[];
};

/** The `account_groups` value of a catalog response. */
export type AccountGroups = {
  /** The bundle's heading for the chips ("Clients"); "" = the generic one. */
  label: string;
  groups: AccountGroup[];
};

/** The part of a catalog row the group rules read and write. */
export type GroupRow = AccountRow & {
  enabled: boolean;
  always_on?: boolean;
  status?: "available" | "needs_setup" | "unavailable";
};

/** A row a group may switch: a bundled connector with a switch. */
function switchable(row: GroupRow | undefined): row is GroupRow {
  return !!row && !row.remote && !row.always_on && (!row.status || row.status === "available");
}

/**
 * The members of a group that can be used here: the connector is in the
 * picker with a switch and has the account. The server's status and the rows
 * are both checked, so a chip is never more optimistic than the rows beside
 * it.
 */
export function usableMembers(group: AccountGroup, rows: readonly GroupRow[]): AccountGroupMember[] {
  const byName = new Map(rows.map((row) => [row.name, row]));
  return group.accounts.filter((m) => {
    if (m.status && m.status !== "ok") return false;
    const row = byName.get(m.server);
    if (!switchable(row)) return false;
    return m.account === "" || (row.accounts ?? []).includes(m.account);
  });
}

/** The members that cannot be used here, for the chip's warning. */
export function unusableMembers(group: AccountGroup, rows: readonly GroupRow[]): AccountGroupMember[] {
  const usable = new Set(usableMembers(group, rows));
  return group.accounts.filter((m) => !usable.has(m));
}

/**
 * A group is on when every member that can be used here is: its connector is
 * on in this chat and runs as that account. A group with nothing usable is
 * never on.
 */
export function isGroupOn(group: AccountGroup, rows: readonly GroupRow[]): boolean {
  const usable = usableMembers(group, rows);
  if (usable.length === 0) return false;
  const byName = new Map(rows.map((row) => [row.name, row]));
  return usable.every((m) => {
    const row = byName.get(m.server);
    return !!row && row.enabled && selectedAccounts(row).includes(m.account);
  });
}

const key = (server: string, account: string) => `${server}\u0000${account}`;

export type GroupChange<T> =
  | { ok: true; rows: T[] }
  /** Turning the group on would put `server` past the account cap. */
  | { ok: false; reason: "cap"; server: string }
  /** No member of the group can be used here. */
  | { ok: false; reason: "unusable" };

/**
 * The rows after a click on a group's chip.
 *
 * Turning a group ON sets each of its connectors on with the group's
 * accounts. On a connector no other group that is on names, or one that was
 * off, the group's accounts REPLACE whatever it had (the first client
 * replaces the deployment's own account). On a connector another group that
 * is on already set, they are ADDED, so the chat runs it as both clients.
 *
 * Turning a group OFF removes its accounts, except one another group that is
 * still on also names. A connector left with no account is switched off: it
 * is never put back on the default account by a click that only asked to
 * remove a client.
 *
 * Connectors the group does not name are not touched either way.
 */
export function toggleAccountGroup<T extends GroupRow>(
  rows: readonly T[],
  groups: readonly AccountGroup[],
  group: AccountGroup,
): GroupChange<T> {
  const usable = usableMembers(group, rows);
  if (usable.length === 0) return { ok: false, reason: "unusable" };
  const wanted = new Map<string, string[]>();
  for (const m of usable) wanted.set(m.server, [...(wanted.get(m.server) ?? []), m.account]);
  const others = groups.filter((g) => g.id !== group.id && isGroupOn(g, rows));

  if (isGroupOn(group, rows)) {
    const kept = new Set(others.flatMap((g) => usableMembers(g, rows).map((m) => key(m.server, m.account))));
    return {
      ok: true,
      rows: rows.map((row) => {
        const accounts = wanted.get(row.name);
        if (!accounts) return row;
        const left = selectedAccounts(row).filter(
          (a) => !accounts.includes(a) || kept.has(key(row.name, a)),
        );
        return left.length > 0
          ? withSelectedAccounts(row, left)
          : withSelectedAccounts({ ...row, enabled: false }, [""]);
      }),
    };
  }

  // Connectors another group that is on already set: there the new accounts
  // are added. Anywhere else the connector holds no group's account (the
  // default one, or one picked by hand), and the group replaces it.
  const othersOn = new Set(others.flatMap((g) => usableMembers(g, rows).map((m) => m.server)));
  const next: T[] = [];
  for (const row of rows) {
    const accounts = wanted.get(row.name);
    if (!accounts) {
      next.push(row);
      continue;
    }
    const replace = !row.enabled || !othersOn.has(row.name);
    const merged = replace ? accounts : [...new Set([...selectedAccounts(row), ...accounts])];
    if (merged.length > MAX_ACCOUNTS_PER_CONNECTOR) return { ok: false, reason: "cap", server: row.name };
    next.push(withSelectedAccounts({ ...row, enabled: true }, merged));
  }
  return { ok: true, rows: next };
}

/** Groups whose name matches a picker search; all of them without one. */
export function matchingGroups(groups: readonly AccountGroup[], query: string): AccountGroup[] {
  const q = query.trim().toLowerCase();
  return q ? groups.filter((g) => g.name.toLowerCase().includes(q)) : [...groups];
}
