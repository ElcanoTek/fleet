import { describe, expect, it } from "vitest";
import {
  droppedOptionalMcpServerNames,
  enabledOptionalMcpServerNames,
  reconcileMcpSelection,
  setOptionalMcpServersEnabled,
} from "./mcpSelection";

describe("enabledOptionalMcpServerNames", () => {
  it("persists selected optional connectors but never locked always-on rows", () => {
    expect(
      enabledOptionalMcpServerNames([
        { name: "email", enabled: true, always_on: true },
        { name: "broken", enabled: false, always_on: true },
        { name: "gamma", enabled: true },
        { name: "xandr", enabled: false },
      ]),
    ).toEqual(["gamma"]);
  });
});

describe("reconcileMcpSelection", () => {
  // The bug this exists to prevent: the server answers 200 having dropped a
  // name its catalog does not know, the client keeps its optimistic state, and
  // the picker then shows a connector ON that no turn will ever load.
  it("turns off a connector the server did not persist", () => {
    expect(
      reconcileMcpSelection(
        [
          { name: "gamma", enabled: true },
          { name: "email_reports", enabled: true },
        ],
        ["gamma"],
      ),
    ).toEqual([
      { name: "gamma", enabled: true },
      { name: "email_reports", enabled: false },
    ]);
  });

  it("turns on a connector the server persisted that we thought was off", () => {
    expect(
      reconcileMcpSelection([{ name: "gamma", enabled: false }], ["gamma"]),
    ).toEqual([{ name: "gamma", enabled: true }]);
  });

  // The server canonicalizes to lowercase. Matching exactly would switch every
  // mixed-case connector off and make the desync worse than doing nothing.
  it("matches case-insensitively", () => {
    expect(
      reconcileMcpSelection([{ name: "Elcano_Email", enabled: true }], [
        "elcano_email",
      ]),
    ).toEqual([{ name: "Elcano_Email", enabled: true }]);
  });

  // Always-on rows are informational status, never part of the opt-in
  // selection, and the server never echoes them back — so an empty response
  // must not switch them off.
  it("leaves always-on rows untouched", () => {
    expect(
      reconcileMcpSelection(
        [
          { name: "fastio", enabled: true, always_on: true },
          { name: "gamma", enabled: true },
        ],
        [],
      ),
    ).toEqual([
      { name: "fastio", enabled: true, always_on: true },
      { name: "gamma", enabled: false },
    ]);
  });

  it("preserves fields it does not own", () => {
    const rows = [{ name: "gamma", enabled: true, account: "work" }];
    expect(reconcileMcpSelection(rows, ["gamma"])).toEqual([
      { name: "gamma", enabled: true, account: "work" },
    ]);
  });
});

describe("droppedOptionalMcpServerNames", () => {
  it("names what the server refused to persist", () => {
    expect(
      droppedOptionalMcpServerNames(["gamma", "email_reports"], ["gamma"]),
    ).toEqual(["email_reports"]);
  });

  it("reports nothing when everything stuck, case aside", () => {
    expect(
      droppedOptionalMcpServerNames(["Gamma"], ["gamma"]),
    ).toEqual([]);
  });
});

describe("setOptionalMcpServersEnabled", () => {
  const rows = [
    { name: "email", enabled: true, always_on: true },
    { name: "broken", enabled: false, always_on: true },
    { name: "gamma", enabled: true },
    { name: "xandr", enabled: false },
  ];
  const all = rows.map((r) => r.name);

  it("turns every named optional row on and leaves always-on rows as they are", () => {
    expect(setOptionalMcpServersEnabled(rows, all, true)).toEqual([
      { name: "email", enabled: true, always_on: true },
      { name: "broken", enabled: false, always_on: true },
      { name: "gamma", enabled: true },
      { name: "xandr", enabled: true },
    ]);
  });

  it("turns every named optional row off and leaves always-on rows as they are", () => {
    expect(setOptionalMcpServersEnabled(rows, all, false)).toEqual([
      { name: "email", enabled: true, always_on: true },
      { name: "broken", enabled: false, always_on: true },
      { name: "gamma", enabled: false },
      { name: "xandr", enabled: false },
    ]);
  });

  // The popover's "All 2 on" with a search active covers only the rows shown.
  it("only touches the rows it is given", () => {
    expect(setOptionalMcpServersEnabled(rows, ["gamma"], false)).toEqual([
      { name: "email", enabled: true, always_on: true },
      { name: "broken", enabled: false, always_on: true },
      { name: "gamma", enabled: false },
      { name: "xandr", enabled: false },
    ]);
  });

  // A connector with no switch can never be opted in, even when named.
  it("skips rows that need setup or are unavailable", () => {
    const withStatus = [
      { name: "bq", enabled: false, status: "needs_setup" as const },
      { name: "tl", enabled: false, status: "unavailable" as const },
      { name: "ix", enabled: false, status: "available" as const },
    ];
    const out = setOptionalMcpServersEnabled(withStatus, ["bq", "tl", "ix"], true);
    expect(out.map((r) => r.enabled)).toEqual([false, false, true]);
    expect(out[0]).toBe(withStatus[0]);
  });

  // The handler skips the state write and the POST when every row came back
  // by identity — a keyboard user can still activate a no-op button.
  it("keeps the identity of rows it does not change, so a no-op flip is detectable", () => {
    const out = setOptionalMcpServersEnabled(rows, all, true);
    expect(out[0]).toBe(rows[0]);
    expect(out[2]).toBe(rows[2]);
    expect(out[3]).not.toBe(rows[3]);
    const again = setOptionalMcpServersEnabled(out, all, true);
    expect(again.every((row, i) => row === out[i])).toBe(true);
  });

  // What the full-state POST is built from: the optional names only, and the
  // seat overrides carried through untouched.
  it("composes into the persisted selection with seats preserved and always-on rows excluded", () => {
    const seated = [
      { name: "email", enabled: true, always_on: true },
      { name: "github", enabled: false, account: "work" },
      { name: "notion", enabled: true, account: "" },
    ];
    const names = seated.map((r) => r.name);
    const next = setOptionalMcpServersEnabled(seated, names, true);
    expect(enabledOptionalMcpServerNames(next)).toEqual(["github", "notion"]);
    expect(next[1]).toEqual({ name: "github", enabled: true, account: "work" });
    expect(enabledOptionalMcpServerNames(setOptionalMcpServersEnabled(seated, names, false))).toEqual([]);
  });
});
