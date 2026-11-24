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
  /** The user's Settings default for this connector; "" / absent = the
   *  deployment's own account. */
  default_account?: string;
  always_on?: boolean;
  status?: "available" | "needs_setup" | "unavailable";
};

/** A row a group may switch: a bundled connector with a switch. */
function switchable(row: GroupRow | undefined): row is GroupRow {
  return !!row && !row.remote && !row.always_on && (!row.status || row.status === "available");
}

const key = (server: string, account: string) => `${server}\u0000${account}`;

/**
 * The accounts a row runs as, by name. A row's "" is "my default", and a user
 * who chose a named default under Settings → Connections runs as THAT account,
 * so it is the name a group's member has to be compared with. "" stays ""
 * only for a user with no named default: the deployment's own account.
 */
export function effectiveAccounts(row: GroupRow): string[] {
  const named = row.default_account ?? "";
  const out: string[] = [];
  for (const a of selectedAccounts(row)) {
    const account = a === "" ? named : a;
    if (!out.includes(account)) out.push(account);
  }
  return out;
}

/**
 * Everything the chips need to know about the groups against the picker's
 * rows, worked out once: which members can be used, which groups are on, and
 * how many on-groups set each connector and each account. A bundle may list
 * hundreds of groups and the chip row asks about each on every render, so
 * each question below is answered from this in time proportional to one
 * group's members, never by walking the other groups again.
 */
export type AccountGroupState<T extends GroupRow = GroupRow> = {
  rows: readonly T[];
  rowsByName: ReadonlyMap<string, T>;
  /** Per group id: the members that can be used here. */
  usable: ReadonlyMap<string, readonly AccountGroupMember[]>;
  /** Ids of the groups that are on. */
  on: ReadonlySet<string>;
  /** Per connector: how many on-groups set it. */
  onByServer: ReadonlyMap<string, number>;
  /** Per connector account (key): how many on-groups name it. */
  onByMember: ReadonlyMap<string, number>;
};

function usableIn(group: AccountGroup, rowsByName: ReadonlyMap<string, GroupRow>): AccountGroupMember[] {
  return group.accounts.filter((m) => {
    if (m.status && m.status !== "ok") return false;
    const row = rowsByName.get(m.server);
    if (!switchable(row)) return false;
    // The deployment's own account cannot be picked by a user whose default
    // is a named one: "" would run as that default, another account under
    // this group's name.
    if (m.account === "") return !row.default_account;
    return (row.accounts ?? []).includes(m.account);
  });
}

export function accountGroupState<T extends GroupRow>(
  rows: readonly T[],
  groups: readonly AccountGroup[],
): AccountGroupState<T> {
  const rowsByName = new Map(rows.map((row) => [row.name, row]));
  const chosen = new Map<string, Set<string>>();
  const runsAs = (server: string, account: string) => {
    const row = rowsByName.get(server);
    if (!row || !row.enabled) return false;
    let set = chosen.get(server);
    if (!set) {
      set = new Set(effectiveAccounts(row));
      chosen.set(server, set);
    }
    return set.has(account);
  };
  const usable = new Map<string, AccountGroupMember[]>();
  const on = new Set<string>();
  const onByServer = new Map<string, number>();
  const onByMember = new Map<string, number>();
  for (const group of groups) {
    const members = usableIn(group, rowsByName);
    usable.set(group.id, members);
    if (members.length === 0 || !members.every((m) => runsAs(m.server, m.account))) continue;
    on.add(group.id);
    for (const server of new Set(members.map((m) => m.server))) {
      onByServer.set(server, (onByServer.get(server) ?? 0) + 1);
    }
    for (const k of new Set(members.map((m) => key(m.server, m.account)))) {
      onByMember.set(k, (onByMember.get(k) ?? 0) + 1);
    }
  }
  return { rows, rowsByName, usable, on, onByServer, onByMember };
}

/**
 * The members of a group that can be used here: the connector is in the
 * picker with a switch and has the account. The server's status and the rows
 * are both checked, so a chip is never more optimistic than the rows beside
 * it.
 */
export function usableMembers(group: AccountGroup, rows: readonly GroupRow[]): AccountGroupMember[] {
  return usableIn(group, new Map(rows.map((row) => [row.name, row])));
}

/** The members that cannot be used here, for the chip's warning. */
export function unusableMembers(group: AccountGroup, rows: readonly GroupRow[]): AccountGroupMember[] {
  const usable = new Set(usableMembers(group, rows));
  return group.accounts.filter((m) => !usable.has(m));
}

/** unusableMembers, from a state already worked out. */
export function unusableMembersIn(state: AccountGroupState, group: AccountGroup): AccountGroupMember[] {
  const usable = new Set(state.usable.get(group.id) ?? []);
  return group.accounts.filter((m) => !usable.has(m));
}

/**
 * A group is on when every member that can be used here is: its connector is
 * on in this chat and runs as that account. A group with nothing usable is
 * never on.
 */
export function isGroupOn(group: AccountGroup, rows: readonly GroupRow[]): boolean {
  return accountGroupState(rows, [group]).on.has(group.id);
}

export type GroupChange<T> =
  | { ok: true; rows: T[] }
  /** Turning the group on would put `server` past the account cap. */
  | { ok: false; reason: "cap"; server: string }
  /** No member of the group can be used here. */
  | { ok: false; reason: "unusable" };

/** What a click on a group would set, per connector, or why it cannot. */
type GroupPlan =
  | { ok: true; turnOff: boolean; accounts: Map<string, string[]> }
  | { ok: false; reason: "cap"; server: string }
  | { ok: false; reason: "unusable" };

function planGroupChange(state: AccountGroupState, group: AccountGroup): GroupPlan {
  const usable = state.usable.get(group.id) ?? [];
  if (usable.length === 0) return { ok: false, reason: "unusable" };
  const wanted = new Map<string, string[]>();
  for (const m of usable) wanted.set(m.server, [...(wanted.get(m.server) ?? []), m.account]);
  const accounts = new Map<string, string[]>();

  if (state.on.has(group.id)) {
    for (const [server, mine] of wanted) {
      const row = state.rowsByName.get(server);
      if (!row) continue;
      // An account another on-group also names stays: this group's own count
      // is one of those recorded, so "another" is more than one.
      accounts.set(
        server,
        effectiveAccounts(row).filter((a) => !mine.includes(a) || (state.onByMember.get(key(server, a)) ?? 0) > 1),
      );
    }
    return { ok: true, turnOff: true, accounts };
  }

  for (const [server, mine] of wanted) {
    const row = state.rowsByName.get(server);
    if (!row) continue;
    // A connector no on-group set holds no group's account (the default one,
    // or one picked by hand), and the group replaces it. The group itself is
    // off, so every on-group counted here is another one.
    const replace = !row.enabled || (state.onByServer.get(server) ?? 0) === 0;
    const merged = replace ? mine : [...new Set([...effectiveAccounts(row), ...mine])];
    if (merged.length > MAX_ACCOUNTS_PER_CONNECTOR) return { ok: false, reason: "cap", server };
    accounts.set(server, merged);
  }
  return { ok: true, turnOff: false, accounts };
}

/**
 * Why a click on a group's chip would do nothing, or null when it would act.
 * Cheap: it builds no rows, so the chip row can ask it of every chip.
 */
export function groupChangeBlock(
  state: AccountGroupState,
  group: AccountGroup,
): { reason: "cap"; server: string } | { reason: "unusable" } | null {
  const plan = planGroupChange(state, group);
  if (plan.ok) return null;
  return plan.reason === "cap" ? { reason: "cap", server: plan.server } : { reason: "unusable" };
}

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
export function toggleAccountGroupIn<T extends GroupRow>(
  state: AccountGroupState<T>,
  group: AccountGroup,
): GroupChange<T> {
  const plan = planGroupChange(state, group);
  if (!plan.ok) return plan;
  return {
    ok: true,
    rows: state.rows.map((row) => {
      const accounts = plan.accounts.get(row.name);
      if (!accounts) return row;
      if (plan.turnOff) {
        return accounts.length > 0
          ? withSelectedAccounts(row, accounts)
          : withSelectedAccounts({ ...row, enabled: false }, [""]);
      }
      return withSelectedAccounts({ ...row, enabled: true }, accounts);
    }),
  };
}

/** toggleAccountGroupIn for a caller with no state at hand. */
export function toggleAccountGroup<T extends GroupRow>(
  rows: readonly T[],
  groups: readonly AccountGroup[],
  group: AccountGroup,
): GroupChange<T> {
  return toggleAccountGroupIn(accountGroupState(rows, groups), group);
}

/** Groups whose name matches a picker search; all of them without one. */
export function matchingGroups(groups: readonly AccountGroup[], query: string): AccountGroup[] {
  const q = query.trim().toLowerCase();
  return q ? groups.filter((g) => g.name.toLowerCase().includes(q)) : [...groups];
}
