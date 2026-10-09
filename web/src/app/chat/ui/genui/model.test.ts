import { describe, expect, it } from "vitest";
import {
  buildSubmissionMessage,
  checkField,
  MAX_LIST_ITEMS,
  MAX_REPEATER_ITEMS,
  MAX_CHOICE_ITEMS,
  collect,
  initialValues,
  normalizeValues,
  isCalendarDate,
  isWebUrl,
  parseCardSpec,
  parseSubmissionMessage,
  UI_SUBMISSION_PREFIX,
  type CardSpec,
  type Component,
} from "./model";
import { loadFixture } from "./fixtures";

const spec: CardSpec = {
  title: "T",
  components: [
    { type: "text_input", id: "name", required: true },
    { type: "toggle", id: "advanced" },
    { type: "number", id: "budget", visible_if: "advanced", required: true, min: 10 },
    {
      type: "tabs",
      tabs: [{ label: "A", children: [{ type: "multi_select", id: "tags", options: ["x", "y"], value: ["x"] }] }],
    },
    {
      type: "repeater",
      id: "lines",
      min_items: 1,
      value: [{ channel: "CTV", cpm: 3 }],
      fields: [
        { type: "choice", id: "channel", options: ["Display", "CTV"], required: true },
        { type: "number", id: "cpm" },
        { type: "list_input", id: "domains", visible_if: "channel != 'CTV'" },
      ],
    },
  ],
  actions: [{ id: "go", label: "Go" }],
};

describe("initialValues", () => {
  it("seeds every input, including through tabs, with its default", () => {
    const v = initialValues(spec);
    expect(v).toEqual({
      name: "",
      advanced: false,
      budget: null,
      tags: ["x"],
      lines: [{ channel: "CTV", cpm: 3, domains: [] }],
    });
  });

  it("starts a repeater at min_items (at least one) when it has no value", () => {
    const v = initialValues({
      title: "x",
      components: [{ type: "repeater", id: "r", min_items: 2, fields: [{ type: "toggle", id: "t" }] }],
    });
    expect(v.r).toEqual([{ t: false }, { t: false }]);
  });
});

describe("collect", () => {
  it("submits only visible inputs and reports errors by path", () => {
    const { values, errors } = collect(spec, initialValues(spec));
    // budget is hidden (advanced off) → neither submitted nor required.
    expect(values).toEqual({ name: "", advanced: false, tags: ["x"], lines: [{ channel: "CTV", cpm: 3 }] });
    expect(errors).toEqual({ name: "Required" });
  });

  it("validates a field once it becomes visible, and repeater fields by item path", () => {
    const v = initialValues(spec);
    v.advanced = true;
    v.budget = 5;
    v.lines = [{ channel: "", cpm: null, domains: [] }];
    const { values, errors } = collect(spec, v);
    expect(errors).toEqual({ name: "Required", budget: "Must be at least 10", "lines[0].channel": "Required" });
    expect(values.lines).toEqual([{ channel: "", cpm: null, domains: [] }]);
  });
});

describe("submission message", () => {
  it("round-trips through the transcript format", () => {
    const msg = buildSubmissionMessage("call_9", "create", { a: 1, b: ["x"], c: { include: [], exclude: ["z"] } });
    expect(msg.startsWith(`${UI_SUBMISSION_PREFIX} card=call_9 action=create\n`)).toBe(true);
    expect(parseSubmissionMessage(msg)).toEqual({
      cardId: "call_9",
      actionId: "create",
      values: { a: 1, b: ["x"], c: { include: [], exclude: ["z"] } },
    });
  });

  it("survives the composer's trim", () => {
    const msg = buildSubmissionMessage("c", "a", { s: "  spaced  " });
    expect(parseSubmissionMessage(msg.trim())?.values).toEqual({ s: "  spaced  " });
  });

  it("ignores ordinary messages and malformed ones", () => {
    expect(parseSubmissionMessage("hello")).toBeNull();
    expect(parseSubmissionMessage("[UI submission] card=x action=y\n```json\nnot json\n```")).toBeNull();
    expect(parseSubmissionMessage("[UI submission] card=x action=y\n```json\n[1]\n```")).toBeNull();
  });
});

describe("parseCardSpec", () => {
  it("rejects non-specs and drops junk components", () => {
    expect(parseCardSpec("nope")).toBeNull();
    expect(parseCardSpec("[]")).toBeNull();
    expect(parseCardSpec('{"title":1,"components":[]}')).toBeNull();
    const s = parseCardSpec('{"title":"t","components":[{"type":"text","text":"a"}, 3, null, {"no":"type"}]}');
    expect(s?.components).toEqual([{ type: "text", text: "a" }]);
  });
});

describe("list_input protocol cap", () => {
  it("applies even when the card sets no max_items, and max_items cannot raise it", () => {
    const big = Array.from({ length: MAX_LIST_ITEMS + 1 }, (_, i) => `d${i}.com`);
    expect(checkField({ type: "list_input", id: "l" }, big)).toContain("At most");
    expect(checkField({ type: "list_input", id: "l", max_items: MAX_LIST_ITEMS * 2 }, big)).toContain("At most");
    expect(checkField({ type: "list_input", id: "l" }, big.slice(0, MAX_LIST_ITEMS))).toBe("");
  });
});

describe("normalizeValues", () => {
  it("drops saved values the inputs could not have produced", () => {
    const s: CardSpec = {
      title: "N",
      components: [
        { type: "select", id: "region", options: ["US", "CA"] },
        { type: "multi_select", id: "tags", options: ["a", "b"] },
        { type: "date", id: "day" },
        { type: "include_exclude", id: "geo", options: ["US", "CA"] },
        { type: "table", id: "row", select: "single", row_key: "id", columns: [{ key: "id" }], rows: [{ id: "r1" }] },
      ],
    };
    const v = normalizeValues(s, {
      region: "MX",
      tags: ["a", "zzz", "a"],
      day: "next tuesday",
      geo: { include: ["US", "MX"], exclude: ["US", "CA"] },
      row: "r9",
      unknown: 1,
    });
    expect(v).toEqual({ region: "", tags: ["a"], day: "", geo: { include: ["US"], exclude: ["CA"] }, row: "" });
  });
});

describe("isCalendarDate", () => {
  it("accepts real dates only", () => {
    expect(isCalendarDate("2026-02-28")).toBe(true);
    expect(isCalendarDate("2028-02-29")).toBe(true);
    expect(isCalendarDate("2026-02-31")).toBe(false);
    expect(isCalendarDate("2026-99-99")).toBe(false);
    expect(isCalendarDate("26-1-1")).toBe(false);
  });
});

describe("list_input defaults", () => {
  it("are trimmed, blank-free and deduped like typed text", () => {
    const s: CardSpec = { title: "L", components: [{ type: "list_input", id: "d" }, { type: "list_input", id: "k", dedupe: false }] };
    expect(normalizeValues(s, { d: [" a.com", "a.com", "", "b.com "], k: ["x", "x"] })).toEqual({ d: ["a.com", "b.com"], k: ["x", "x"] });
  });
});

describe("url inputs", () => {
  it("need a real http(s) URL with a host", () => {
    expect(isWebUrl("https://example.com/x")).toBe(true);
    expect(isWebUrl("https://?")).toBe(false);
    expect(isWebUrl("https://#")).toBe(false);
    expect(isWebUrl("ftp://example.com")).toBe(false);
    expect(checkField({ type: "text_input", id: "u", format: "url" }, "https://?")).toContain("URL");
  });
});

describe("parseCardSpec cache", () => {
  it("parses a given input once and returns the same spec", () => {
    const input = JSON.stringify({ title: "c", components: [{ type: "text", text: "x" }] });
    expect(parseCardSpec(input)).toBe(parseCardSpec(input));
  });
});

describe("email format", () => {
  const email = { type: "text_input", id: "e", format: "email" } as const;
  it("rejects what the browser's email control rejects", () => {
    for (const bad of ["a@.com", "a@b..com", "a@-b.com", "a b@c.com", "a@b"]) {
      expect(checkField(email, bad)).toBe("Enter an email address");
    }
  });
  it("accepts ordinary addresses", () => {
    for (const ok of ["a@b.com", "first.last+tag@sub.example.co.uk", " a@b.io "]) {
      expect(checkField(email, ok)).toBe("");
    }
  });
});

describe("calendar dates", () => {
  it("accepts years below 100, which Date.UTC would remap", () => {
    expect(isCalendarDate("0001-01-01")).toBe(true);
    expect(isCalendarDate("0099-12-31")).toBe(true);
    expect(isCalendarDate("0099-02-30")).toBe(false);
    // An HTML date input has no year 0.
    expect(isCalendarDate("0000-01-01")).toBe(false);
    expect(isCalendarDate("2024-02-29")).toBe(true);
    expect(isCalendarDate("2023-02-29")).toBe(false);
  });
});


describe("restored repeater values", () => {
  it("are capped at the protocol limit", () => {
    const spec = parseCardSpec(JSON.stringify({
      title: "R",
      components: [{ type: "repeater", id: "lines", fields: [{ type: "number", id: "n" }] }],
      actions: [{ id: "go", label: "Go" }],
    }));
    const restored = normalizeValues(spec!, { lines: Array.from({ length: 5000 }, () => ({})) });
    expect((restored.lines as unknown[]).length).toBe(MAX_REPEATER_ITEMS);
  });
});

describe("restored values the user could not fix", () => {
  const card = (components: unknown[]) =>
    parseCardSpec(JSON.stringify({ title: "R", components, actions: [{ id: "go", label: "Go" }] }))!;

  it("a disabled input keeps the card default", () => {
    const spec = card([{ type: "number", id: "n", label: "N", disabled: true, min: 0, max: 10, value: 5 }]);
    expect(normalizeValues(spec, { n: 100 }).n).toBe(5);
  });

  it("an input that can be disabled keeps only a valid value", () => {
    const spec = card([
      { type: "toggle", id: "lock", label: "Lock" },
      { type: "number", id: "n", label: "N", disabled_if: "lock", min: 0, max: 10, step: 2, value: 4 },
    ]);
    expect(normalizeValues(spec, { n: 100 }).n).toBe(4);
    expect(normalizeValues(spec, { n: 3 }).n).toBe(4);
    expect(normalizeValues(spec, { n: 8 }).n).toBe(8);
  });

  it("a slider keeps only a value its range control can hold", () => {
    const spec = card([{ type: "slider", id: "s", label: "S", min: 0, max: 10, value: 3 }]);
    expect(normalizeValues(spec, { s: 100 }).s).toBe(3);
    expect(normalizeValues(spec, { s: 7 }).s).toBe(7);
  });

  it("a disabled field in a restored repeater item falls back to the field default", () => {
    const spec = card([
      {
        type: "repeater",
        id: "r",
        label: "R",
        fields: [
          { type: "toggle", id: "ok", label: "OK", required: true, disabled: true, value: true },
          { type: "number", id: "q", label: "Q", disabled: true, min: 0, max: 10, value: 1 },
          { type: "text_input", id: "t", label: "T" },
        ],
      },
    ]);
    const items = normalizeValues(spec, { r: [{ ok: false, q: 50, t: "a" }, { ok: true, q: 7, t: "b" }] }).r as Record<string, unknown>[];
    expect(items).toEqual([
      { ok: true, q: 1, t: "a" },
      { ok: true, q: 1, t: "b" },
    ]);
  });

  it("a disabled field in a restored repeater item keeps only values the card put there", () => {
    const spec = card([
      {
        type: "repeater",
        id: "r",
        label: "R",
        value: [{ q: 3 }, { q: 4 }],
        fields: [
          { type: "number", id: "q", label: "Q", disabled: true, value: 5 },
          { type: "text_input", id: "t", label: "T" },
        ],
      },
    ]);
    const items = normalizeValues(spec, { r: [{ q: 4, t: "a" }, { q: 6, t: "b" }, { q: 5, t: "c" }] }).r as Record<string, unknown>[];
    expect(items.map((it) => it.q)).toEqual([4, 5, 5]);
  });
});

describe("disabled defaults (shared with internal/genui TestDisabledDefaultsFixture)", () => {
  type DCase = { name: string; component: Record<string, unknown>; valid: boolean };
  const { cases } = loadFixture<{ cases: DCase[] }>("disabled_defaults.json");
  for (const c of cases) {
    it(c.name, () => {
      const comp = { ...c.component, disabled: true } as unknown as Component;
      const spec = { title: "x", components: [comp], actions: [{ id: "go", label: "Go" }] } as unknown as CardSpec;
      const values = normalizeValues(spec, null);
      expect(checkField(comp, values[String(comp.id)]) === "").toBe(c.valid);
    });
  }
});

describe("restored chip collections", () => {
  it("are capped at the protocol limit, quickly", () => {
    const many = Array.from({ length: 30000 }, (_, i) => `v${i}`);
    const spec = {
      title: "x",
      components: [
        { type: "multi_select", id: "m", allow_custom: true },
        { type: "include_exclude", id: "ie", allow_custom: true },
      ],
    } as unknown as CardSpec;
    const t0 = performance.now();
    const v = normalizeValues(spec, { m: many, ie: { include: many, exclude: many.slice(0, 15000).map((x) => x + "x") } });
    expect(performance.now() - t0).toBeLessThan(1000);
    expect((v.m as string[]).length).toBe(MAX_CHOICE_ITEMS);
    const ie = v.ie as { include: string[]; exclude: string[] };
    expect(ie.include.length + ie.exclude.length).toBe(MAX_CHOICE_ITEMS);
  });
});
