import { describe, expect, it } from "vitest";
import {
  droppedOptionalMcpServerNames,
  enabledOptionalMcpServerNames,
  reconcileMcpSelection,
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
