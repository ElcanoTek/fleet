import { describe, expect, it } from "vitest";
import {
  buildGraphQueryString,
  datetimeLocalToRFC3339,
  groupRelationsBySubject,
  relationObjectLabel,
  relationValiditySuffix,
  type GraphRelation,
} from "./memoryGraph";

const ada = { id: "e1", name: "Ada", type: "person" };
const corp = { id: "e2", name: "Elcano Corp", type: "organization" };

function relation(overrides: Partial<GraphRelation>): GraphRelation {
  return {
    id: "r1",
    subject: ada,
    predicate: "works at",
    object: { entity: corp },
    memory_id: "m1",
    memory_content_snippet: "Ada works at Elcano Corp",
    learned_at: 1750000000,
    ...overrides,
  };
}

describe("relationObjectLabel", () => {
  it("uses the entity name for entity edges", () => {
    expect(relationObjectLabel(relation({}))).toBe("Elcano Corp");
  });
  it("uses the literal for value edges", () => {
    expect(relationObjectLabel(relation({ object: { value: "tabs" } }))).toBe("tabs");
  });
  it("degrades to empty for a malformed object", () => {
    expect(relationObjectLabel(relation({ object: {} }))).toBe("");
  });
});

describe("groupRelationsBySubject", () => {
  it("buckets by subject id, preserving order", () => {
    const bob = { id: "e3", name: "Bob", type: "person" };
    const groups = groupRelationsBySubject([
      relation({ id: "r1" }),
      relation({ id: "r2", subject: bob }),
      relation({ id: "r3", predicate: "prefers", object: { value: "tabs" } }),
    ]);
    expect(groups.map((g) => g.subject.name)).toEqual(["Ada", "Bob"]);
    expect(groups[0].relations.map((r) => r.id)).toEqual(["r1", "r3"]);
    expect(groups[1].relations.map((r) => r.id)).toEqual(["r2"]);
  });
  it("returns [] for no relations", () => {
    expect(groupRelationsBySubject([])).toEqual([]);
  });
});

describe("datetimeLocalToRFC3339", () => {
  it("converts a datetime-local value to RFC3339", () => {
    const out = datetimeLocalToRFC3339("2026-07-02T14:30");
    expect(out).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/);
    expect(new Date(out as string).getTime()).toBe(new Date("2026-07-02T14:30").getTime());
  });
  it("returns null for empty or garbage input", () => {
    expect(datetimeLocalToRFC3339("")).toBeNull();
    expect(datetimeLocalToRFC3339("   ")).toBeNull();
    expect(datetimeLocalToRFC3339("yesterday")).toBeNull();
  });
});

describe("buildGraphQueryString", () => {
  it("is empty when both inputs are unset", () => {
    expect(buildGraphQueryString("", "")).toBe("");
  });
  it("includes only the set axes", () => {
    const qs = buildGraphQueryString("2026-07-02T14:30", "");
    expect(qs.startsWith("?as_of_valid=")).toBe(true);
    expect(qs).not.toContain("as_of_learned");
    const both = buildGraphQueryString("2026-07-02T14:30", "2026-01-01T00:00");
    expect(both).toContain("as_of_valid=");
    expect(both).toContain("as_of_learned=");
  });
});

describe("relationValiditySuffix", () => {
  const from = Date.UTC(2024, 0, 1) / 1000;
  const to = Date.UTC(2024, 11, 31) / 1000;
  it("renders both bounds", () => {
    expect(relationValiditySuffix(relation({ valid_from: from, valid_to: to }))).toBe(
      "true 2024-01-01 → 2024-12-31",
    );
  });
  it("renders open-ended bounds", () => {
    expect(relationValiditySuffix(relation({ valid_from: from }))).toBe("true since 2024-01-01");
    expect(relationValiditySuffix(relation({ valid_to: to }))).toBe("true until 2024-12-31");
  });
  it("is empty without a window", () => {
    expect(relationValiditySuffix(relation({}))).toBe("");
  });
});
