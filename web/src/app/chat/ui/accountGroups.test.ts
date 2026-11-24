import { describe, expect, it } from "vitest";
import {
  isGroupOn,
  matchingGroups,
  toggleAccountGroup,
  unusableMembers,
  usableMembers,
  type AccountGroup,
  type GroupRow,
} from "./accountGroups";
import { selectedAccounts } from "./mcpAccounts";

const rows = (): GroupRow[] => [
  { name: "ix", enabled: true, accounts: ["arena", "twc", "usd", "gbp"] },
  { name: "ox", enabled: false, accounts: ["twc"] },
  { name: "pm", enabled: true, accounts: ["arena"] },
  { name: "mail", enabled: true, always_on: true },
  { name: "tl", enabled: false, status: "needs_setup" },
];

const twc: AccountGroup = {
  id: "twc",
  name: "The Weather Company",
  accounts: [
    { server: "ix", account: "twc", status: "ok" },
    { server: "ox", account: "twc", status: "ok" },
  ],
};
const arena: AccountGroup = {
  id: "arena",
  name: "The Arena Group",
  accounts: [
    { server: "ix", account: "arena" },
    { server: "pm", account: "arena" },
  ],
};
const groups = [twc, arena];

const state = (list: readonly GroupRow[]) =>
  Object.fromEntries(list.map((r) => [r.name, r.enabled ? selectedAccounts(r) : "off"]));

function apply(list: readonly GroupRow[], group: AccountGroup): GroupRow[] {
  const change = toggleAccountGroup(list, groups, group);
  if (!change.ok) throw new Error(`refused: ${change.reason}`);
  return change.rows;
}

describe("usable members", () => {
  it("needs a switchable connector that has the account", () => {
    const g: AccountGroup = {
      id: "g",
      name: "G",
      accounts: [
        { server: "ix", account: "twc" },
        { server: "ix", account: "nope" },
        { server: "pm", account: "twc", status: "account_missing" },
        { server: "mail", account: "" },
        { server: "tl", account: "" },
        { server: "gone", account: "", status: "connector_missing" },
        { server: "ox", account: "" },
      ],
    };
    expect(usableMembers(g, rows())).toEqual([
      { server: "ix", account: "twc" },
      { server: "ox", account: "" },
    ]);
    expect(unusableMembers(g, rows()).map((m) => `${m.server}:${m.account}`)).toEqual([
      "ix:nope",
      "pm:twc",
      "mail:",
      "tl:",
      "gone:",
    ]);
  });
});

describe("toggleAccountGroup", () => {
  it("the first group replaces the default account and turns its connectors on", () => {
    const after = apply(rows(), twc);
    expect(state(after)).toMatchObject({ ix: ["twc"], ox: ["twc"], pm: [""] });
    expect(isGroupOn(twc, after)).toBe(true);
    expect(isGroupOn(arena, after)).toBe(false);
  });

  it("a second group adds its accounts beside the first", () => {
    const after = apply(apply(rows(), twc), arena);
    // ix runs as both clients. pm was on the default account, which is no
    // group's, so there the second group is the first and replaces it.
    expect(state(after)).toMatchObject({ ix: ["arena", "twc"], ox: ["twc"], pm: ["arena"] });
    expect(isGroupOn(twc, after)).toBe(true);
    expect(isGroupOn(arena, after)).toBe(true);
  });

  it("turning a group off removes only its accounts", () => {
    const both = apply(apply(rows(), twc), arena);
    const after = apply(both, twc);
    expect(state(after)).toMatchObject({ ix: ["arena"], ox: "off", pm: ["arena"] });
    expect(isGroupOn(arena, after)).toBe(true);
  });

  it("a connector left with no account is switched off, never put on the default", () => {
    const after = apply(apply(rows(), twc), twc);
    expect(state(after)).toMatchObject({ ix: "off", ox: "off" });
    expect(after.find((r) => r.name === "ix")?.account).toBe("");
  });

  it("keeps an account another group that is on also names", () => {
    const shared: AccountGroup = { id: "shared", name: "Shared", accounts: [{ server: "ix", account: "twc" }] };
    const all = [twc, shared];
    const on = toggleAccountGroup(rows(), all, twc);
    if (!on.ok) throw new Error("refused");
    // `shared` is on too now: it names only ix/twc.
    expect(isGroupOn(shared, on.rows)).toBe(true);
    const off = toggleAccountGroup(on.rows, all, twc);
    if (!off.ok) throw new Error("refused");
    expect(state(off.rows)).toMatchObject({ ix: ["twc"], ox: "off" });
  });

  it("leaves connectors the group does not name alone", () => {
    const before = rows();
    const after = apply(before, twc);
    for (const name of ["pm", "mail", "tl"]) {
      expect(after.find((r) => r.name === name)).toBe(before.find((r) => r.name === name));
    }
  });

  it("a client with two accounts on one connector turns both on", () => {
    const omg: AccountGroup = {
      id: "omg",
      name: "Outcomes",
      accounts: [
        { server: "ix", account: "usd" },
        { server: "ix", account: "gbp" },
      ],
    };
    const change = toggleAccountGroup(rows(), [omg], omg);
    if (!change.ok) throw new Error("refused");
    expect(state(change.rows).ix).toEqual(["usd", "gbp"]);
  });

  it("refuses past the account cap and names the connector", () => {
    const wide: GroupRow = {
      name: "ix",
      enabled: true,
      accounts: ["a", "b", "c", "d", "e", "f", "g"],
      account: "a",
      selected_accounts: ["a", "b", "c", "d", "e", "f"],
    };
    const six: AccountGroup = {
      id: "six",
      name: "Six",
      accounts: ["a", "b", "c", "d", "e", "f"].map((account) => ({ server: "ix", account })),
    };
    const seventh: AccountGroup = { id: "g", name: "G", accounts: [{ server: "ix", account: "g" }] };
    expect(isGroupOn(six, [wide])).toBe(true);
    expect(toggleAccountGroup([wide], [six, seventh], seventh)).toEqual({ ok: false, reason: "cap", server: "ix" });
  });

  it("refuses a group with nothing usable here", () => {
    const none: AccountGroup = { id: "n", name: "N", accounts: [{ server: "gone", account: "x" }] };
    expect(toggleAccountGroup(rows(), [none], none)).toEqual({ ok: false, reason: "unusable" });
    expect(isGroupOn(none, rows())).toBe(false);
  });
});

describe("matchingGroups", () => {
  it("filters by name, case-insensitively, and returns all without a search", () => {
    expect(matchingGroups(groups, "")).toHaveLength(2);
    expect(matchingGroups(groups, " arena ").map((g) => g.id)).toEqual(["arena"]);
    expect(matchingGroups(groups, "zzz")).toEqual([]);
  });
});
