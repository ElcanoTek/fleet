import { describe, expect, it } from "vitest";
import {
  MAX_ACCOUNTS_PER_CONNECTOR,
  allowsSeveralAccounts,
  mcpAccountOverrides,
  parseAccountChoices,
  sameAccounts,
  selectedAccounts,
  toggleAccount,
  withSelectedAccounts,
} from "./mcpAccounts";

describe("selectedAccounts", () => {
  it("reads one account from `account` and several from `selected_accounts`", () => {
    expect(selectedAccounts({})).toEqual([""]);
    expect(selectedAccounts({ account: "twc" })).toEqual(["twc"]);
    expect(selectedAccounts({ account: "", selected_accounts: ["", "twc"] })).toEqual(["", "twc"]);
    // A one-entry list is not a list: `account` is the truth.
    expect(selectedAccounts({ account: "twc", selected_accounts: ["arena"] })).toEqual(["twc"]);
  });
});

describe("withSelectedAccounts", () => {
  const row = { name: "ix", accounts: ["arena", "raptive", "twc"] };

  it("orders the choice default first, then as the catalog lists accounts", () => {
    expect(withSelectedAccounts(row, ["twc", "", "arena", "twc"])).toMatchObject({
      account: "",
      selected_accounts: ["", "arena", "twc"],
    });
  });

  it("keeps a one-account row free of the list field", () => {
    const one = withSelectedAccounts({ ...row, account: "", selected_accounts: ["", "twc"] }, ["twc"]);
    expect(one.account).toBe("twc");
    expect("selected_accounts" in one).toBe(false);
    expect(withSelectedAccounts(row, []).account).toBe("");
  });

  it("does not change the row it was given", () => {
    const before = { ...row, account: "twc" };
    withSelectedAccounts(before, ["", "arena"]);
    expect(before).toEqual({ ...row, account: "twc" });
  });
});

describe("toggleAccount", () => {
  it("adds and removes, but never removes the last account", () => {
    expect(toggleAccount([""], "twc")).toEqual(["", "twc"]);
    expect(toggleAccount(["", "twc"], "")).toEqual(["twc"]);
    expect(toggleAccount(["twc"], "twc")).toEqual(["twc"]);
  });

  it("adds nothing past the cap", () => {
    const full = Array.from({ length: MAX_ACCOUNTS_PER_CONNECTOR }, (_, i) => `a${i}`);
    expect(toggleAccount(full, "one-more")).toEqual(full);
    expect(toggleAccount(full, "a0")).toHaveLength(MAX_ACCOUNTS_PER_CONNECTOR - 1);
  });
});

describe("the wire shape", () => {
  it("sends a string for one account and a list for several, and nothing for the default", () => {
    expect(
      mcpAccountOverrides([
        { name: "ix", account: "", selected_accounts: ["", "twc"] },
        { name: "ox", account: "twc" },
        { name: "pm", account: "" },
        { name: "mg" },
      ]),
    ).toEqual({ ix: ["", "twc"], ox: "twc" });
  });

  it("reads both shapes back and ignores anything else", () => {
    expect(parseAccountChoices({ ix: ["", "twc"], ox: "twc", bad: 3, worse: ["a", 1] })).toEqual({
      ix: ["", "twc"],
      ox: "twc",
    });
    expect(parseAccountChoices(undefined)).toBeNull();
  });
});

describe("sameAccounts / allowsSeveralAccounts", () => {
  it("compares whole choices", () => {
    expect(sameAccounts({ account: "twc" }, { account: "twc" })).toBe(true);
    expect(sameAccounts({ account: "" }, {})).toBe(true);
    expect(sameAccounts({ account: "", selected_accounts: ["", "twc"] }, { account: "" })).toBe(false);
  });

  it("offers several accounts only on a bundled connector that has named accounts", () => {
    expect(allowsSeveralAccounts({ accounts: ["twc"] })).toBe(true);
    expect(allowsSeveralAccounts({ accounts: ["work"], remote: true })).toBe(false);
    expect(allowsSeveralAccounts({})).toBe(false);
  });
});
