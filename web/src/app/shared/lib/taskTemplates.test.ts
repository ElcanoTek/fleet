import { describe, expect, it } from "vitest";
import {
  applyTemplateVars,
  extractTemplateVars,
  humanizeVarName,
  promptableVars,
  resolveBuiltinVar,
} from "./taskTemplates";

const FIXED_DATE = new Date("2026-06-28T12:00:00Z");

describe("extractTemplateVars", () => {
  it("returns [] for a placeholder-free prompt", () => {
    expect(extractTemplateVars("just a plain prompt")).toEqual([]);
  });
  it("extracts, de-duplicates, and sorts tokens", () => {
    expect(extractTemplateVars("use {b} then {a} then {b} again")).toEqual(["a", "b"]);
  });
  it("ignores empty braces", () => {
    expect(extractTemplateVars("nothing {} or { } here")).toEqual([]);
  });
});

describe("resolveBuiltinVar", () => {
  it("fills {date} as ISO YYYY-MM-DD", () => {
    expect(resolveBuiltinVar("date", { today: FIXED_DATE })).toBe("2026-06-28");
  });
  it("fills {user_name} when a display name is present", () => {
    expect(resolveBuiltinVar("user_name", { userName: "Ada Lovelace" })).toBe("Ada Lovelace");
  });
  it("returns undefined for {user_name} when the name is unknown", () => {
    expect(resolveBuiltinVar("user_name", {})).toBeUndefined();
    expect(resolveBuiltinVar("user_name", { userName: "   " })).toBeUndefined();
  });
  it("returns undefined for non-built-in tokens", () => {
    expect(resolveBuiltinVar("repo_path", {})).toBeUndefined();
  });
});

describe("promptableVars", () => {
  it("excludes auto-filled built-ins, keeps custom + unresolvable tokens", () => {
    const vars = ["date", "user_name", "repo_path"];
    // user_name is unknown -> still promptable; date is auto-filled -> excluded.
    expect(promptableVars(vars, { today: FIXED_DATE })).toEqual(["user_name", "repo_path"]);
    // With a known user name, only the custom token remains.
    expect(promptableVars(vars, { today: FIXED_DATE, userName: "Ada" })).toEqual(["repo_path"]);
  });
  it("is empty when every variable is an auto-filled built-in", () => {
    expect(promptableVars(["date"], { today: FIXED_DATE })).toEqual([]);
  });
});

describe("humanizeVarName", () => {
  it("turns snake_case into a friendly label", () => {
    expect(humanizeVarName("repo_path")).toBe("Repo path");
    expect(humanizeVarName("date")).toBe("Date");
  });
});

describe("applyTemplateVars", () => {
  it("substitutes built-ins automatically", () => {
    const out = applyTemplateVars("Report for {user_name} on {date}.", {}, {
      today: FIXED_DATE,
      userName: "Ada",
    });
    expect(out).toBe("Report for Ada on 2026-06-28.");
  });
  it("prefers an explicit user value over the built-in", () => {
    const out = applyTemplateVars("on {date}", { date: "1999-12-31" }, { today: FIXED_DATE });
    expect(out).toBe("on 1999-12-31");
  });
  it("substitutes custom tokens from user values", () => {
    const out = applyTemplateVars("Review {repo_path} now", { repo_path: "./service" }, {});
    expect(out).toBe("Review ./service now");
  });
  it("leaves a placeholder intact when nothing fills it", () => {
    const out = applyTemplateVars("Review {repo_path}", {}, {});
    expect(out).toBe("Review {repo_path}");
  });
  it("substitutes every occurrence of a repeated token", () => {
    const out = applyTemplateVars("{x} and {x}", { x: "Q" }, {});
    expect(out).toBe("Q and Q");
  });
});
