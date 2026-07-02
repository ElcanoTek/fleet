import { describe, expect, it } from "vitest";
import { completeSkill, filterSkills, skillSlashQuery, type SkillInfo } from "./skillSlash";

const ROSTER: SkillInfo[] = [
  { name: "deploy", description: "roll a release out" },
  { name: "deep-dive", description: "long-form investigation" },
  { name: "research-report", description: "write a cited research report" },
];

describe("skillSlashQuery", () => {
  it("returns the partial token while typing a leading slash command", () => {
    expect(skillSlashQuery("/")).toBe("");
    expect(skillSlashQuery("/d")).toBe("d");
    expect(skillSlashQuery("/deploy")).toBe("deploy");
    expect(skillSlashQuery("/research-report")).toBe("research-report");
  });

  it("is closed for drafts that do not start with a slash", () => {
    expect(skillSlashQuery("")).toBeNull();
    expect(skillSlashQuery("hello")).toBeNull();
    expect(skillSlashQuery("run /deploy")).toBeNull();
    expect(skillSlashQuery(" /deploy")).toBeNull(); // slash must be char 0
  });

  it("closes once whitespace follows the token (user is typing args)", () => {
    expect(skillSlashQuery("/deploy ")).toBeNull();
    expect(skillSlashQuery("/deploy staging")).toBeNull();
    expect(skillSlashQuery("/deploy\nnotes")).toBeNull();
  });

  it("does not close on a path-like token — the popover just has no matches", () => {
    // "/etc/hosts" keeps the popover context open (no whitespace yet), but no
    // skill name contains "/", so filterSkills returns nothing to show.
    expect(skillSlashQuery("/etc/hosts")).toBe("etc/hosts");
    expect(filterSkills(ROSTER, "etc/hosts")).toEqual([]);
  });
});

describe("filterSkills", () => {
  it("filters by name prefix", () => {
    expect(filterSkills(ROSTER, "de").map((s) => s.name)).toEqual(["deploy", "deep-dive"]);
    expect(filterSkills(ROSTER, "research").map((s) => s.name)).toEqual(["research-report"]);
  });

  it("returns the full roster for an empty query (bare '/')", () => {
    expect(filterSkills(ROSTER, "")).toHaveLength(ROSTER.length);
  });

  it("is case-sensitive, matching the server's exact-match invocation rule", () => {
    expect(filterSkills(ROSTER, "Dep")).toEqual([]);
  });

  it("returns nothing when nothing matches", () => {
    expect(filterSkills(ROSTER, "zzz")).toEqual([]);
    expect(filterSkills([], "de")).toEqual([]);
  });
});

describe("completeSkill", () => {
  it("completes to '/name ' so the user can keep typing arguments", () => {
    expect(completeSkill("deploy")).toBe("/deploy ");
  });
});
