import { describe, expect, it } from "vitest";
import { evaluate, evaluateSafe, parseExpr, renderTemplate, ExprError, MAX_EXPR_STRING, MAX_TEMPLATE_CHARS } from "./expr";
import { loadFixture } from "./fixtures";

type Case = { expr: string; valid: boolean; scope?: Record<string, unknown>; want?: unknown };

describe("expression fixture (shared with internal/genui/expr.go)", () => {
  const { cases } = loadFixture<{ cases: Case[] }>("expressions.json");
  for (const c of cases) {
    it(`${c.valid ? "accepts" : "rejects"} ${JSON.stringify(c.expr)}`, () => {
      if (!c.valid) {
        expect(() => parseExpr(c.expr)).toThrow(ExprError);
        return;
      }
      expect(() => parseExpr(c.expr)).not.toThrow();
      if ("want" in c) {
        expect(evaluate(c.expr, c.scope ?? {})).toEqual(c.want);
      }
    });
  }
});

describe("evaluation is sandboxed to the scope", () => {
  it("cannot reach globals or prototype members", () => {
    expect(evaluate("window", {})).toBeNull();
    expect(evaluate("constructor", {})).toBeNull();
    expect(evaluate("x.constructor", { x: {} })).toBeNull();
    expect(evaluate("x.__proto__", { x: {} })).toBeNull();
    expect(evaluate("len.constructor", {})).toBeNull();
  });
  it("rejects a function name outside the whitelist even if a prototype has it", () => {
    expect(() => parseExpr("toString(1)")).toThrow(ExprError);
    expect(() => parseExpr("constructor(1)")).toThrow(ExprError);
  });
});

describe("renderTemplate", () => {
  it("fills holes and formats values", () => {
    expect(renderTemplate("{{ n }} deals · {{ list }}", { n: 3, list: ["a", "b"] })).toBe("3 deals · a, b");
    expect(renderTemplate("{{ 0.1 + 0.2 }}", {})).toBe("0.3");
    expect(renderTemplate("plain", {})).toBe("plain");
    expect(renderTemplate("{{ missing }}!", {})).toBe("!");
  });
  it("marks a broken hole instead of throwing", () => {
    expect(renderTemplate("a {{ 1 + }} b", {})).toBe("a ⚠ b");
  });
});

describe("unique()", () => {
  // The definition: keep each item no earlier kept item is == to.
  const reference = (xs: unknown[]) => {
    const out: unknown[] = [];
    for (const x of xs) if (!out.some((y) => evaluate("a == b", { a: x, b: y }) === true)) out.push(x);
    return out;
  };
  const pool: unknown[] = [null, 0, 1, 2, 1.5, "0", "1", " 1 ", "1.0", "", " ", "a", "A", true, false, [1], { k: 1 }];

  it("matches the pairwise definition on mixed values", () => {
    let seed = 7;
    const rand = () => {
      seed = (seed * 1103515245 + 12345) % 2147483648;
      return seed / 2147483648;
    };
    for (let round = 0; round < 300; round++) {
      const xs = Array.from({ length: 1 + Math.floor(rand() * 10) }, () => pool[Math.floor(rand() * pool.length)]);
      expect(evaluate("unique(xs)", { xs })).toEqual(reference(xs));
    }
  });

  it("stays fast on a full-size list", () => {
    const xs = Array.from({ length: 20000 }, (_, i) => `domain${i % 15000}.example`);
    const start = performance.now();
    expect(evaluate("len(unique(xs))", { xs })).toBe(15000);
    expect(performance.now() - start).toBeLessThan(1500);
  });
});

describe("aggregates near the float limit", () => {
  it("average without overflowing, and sum to null when it cannot be represented", () => {
    expect(evaluate("avg(xs)", { xs: [1e308, 1e308] })).toBe(1e308);
    expect(evaluate("sum(xs)", { xs: [1e308, 1e308] })).toBeNull();
    expect(evaluate("sum(xs)", { xs: [1, 2] })).toBe(3);
  });

  it("round a huge value without overflowing to Infinity", () => {
    expect(evaluate("round(x, 2)", { x: 1e308 })).toBe(1e308);
    expect(evaluate("round(x, 2)", { x: -1e308 })).toBe(-1e308);
    expect(evaluate("round(x, 2)", { x: 1.005 })).toBe(1.01);
  });
});

describe("every function stays finite", () => {
  it("never yields Infinity or NaN for extreme or junk inputs", () => {
    const scope = { big: 1.7e308, neg: -1.7e308, xs: [1.7e308, 1.7e308, -1e-308], s: "abc", nested: [[1], { a: 1 }] };
    const args = ["big", "neg", "xs", "s", "nested", "0", "-1", "11", "big * 10"];
    const fns = ["len", "count", "sum", "avg", "min", "max", "abs", "floor", "ceil", "round", "fixed", "number"];
    for (const fn of fns) {
      for (const a of args) {
        for (const b of ["", ", 2", ", big", ", -5"]) {
          const v = evaluateSafe(`${fn}(${a}${b})`, scope);
          if (typeof v === "number") expect(Number.isFinite(v)).toBe(true);
          if (typeof v === "string") expect(v).not.toMatch(/Infinity|NaN/);
        }
      }
    }
  });
});

describe("bounded display", () => {
  it("clips the text a template's holes add, not the template's own text", () => {
    const lines = Array.from({ length: 20000 }, () => "x");
    const out = renderTemplate("Lines: {{ join(lines) }} and {{ join(lines) }}", { lines });
    expect(out.length).toBeLessThanOrEqual("Lines:  and ".length + MAX_TEMPLATE_CHARS + 2);
    expect(out.startsWith("Lines: x, x")).toBe(true);
    const long = "a".repeat(5000);
    expect(renderTemplate(`${long} {{ n }}`, { n: 1 })).toBe(`${long} 1`);
  });

  it("joins one list once however many templates show it", () => {
    const lines = Array.from({ length: 1000 }, (_, i) => `l${i}`);
    const a = evaluate("join(lines)", { lines });
    expect(evaluate("join(lines)", { lines })).toBe(a);
    expect(evaluate('join(lines, "|")', { lines })).toBe(lines.join("|"));
  });

  it("bounds every string an expression builds, the same in every context", () => {
    const lines = Array.from({ length: 20000 }, () => "x");
    const scope = { lines, sep: "y".repeat(20000) };
    const shown = renderTemplate("{{ join(lines, sep) }}", scope);
    expect(shown.length).toBeLessThanOrEqual(MAX_TEMPLATE_CHARS + 2);
    expect(evaluate("len(join(lines, sep))", scope)).toBe(MAX_EXPR_STRING);
    expect((evaluate("lines + sep", scope) as string).length).toBeLessThanOrEqual(MAX_EXPR_STRING);
    // A short join is exact.
    expect(evaluate("len(join(few, sep))", { few: ["a", "b"], sep: "y".repeat(20000) })).toBe(20002);
  });

  it("aggregates a list once, however many templates read it", () => {
    const lines = ["1", "2", "x", "3"];
    expect(evaluate("sum(lines)", { lines })).toBe(6);
    expect(evaluate("sum(lines)", { lines })).toBe(6);
    // A changed list is a new array, so it is aggregated afresh.
    expect(evaluate("sum(lines)", { lines: [...lines, "4"] })).toBe(10);
    expect(evaluate("avg(lines)", { lines })).toBe(2);
    expect(evaluate("max(lines)", { lines })).toBe(3);
    expect(evaluate("min(lines)", { lines })).toBe(1);
    expect(evaluate("sum(5)", {})).toBe(5);
  });

  it("projects a list's member once, so later functions reuse their results", () => {
    const rows = [{ note: "a" }, { note: "b" }];
    const first = evaluate("rows.note", { rows });
    expect(first).toEqual(["a", "b"]);
    expect(evaluate("rows.note", { rows })).toBe(first);
    expect(evaluate("unique(rows.note)", { rows })).toBe(evaluate("unique(rows.note)", { rows }));
  });

  it("deduplicates a list once, however many templates read it", () => {
    const lines = ["a", "b", "a"];
    const u = evaluate("unique(lines)", { lines });
    expect(u).toEqual(["a", "b"]);
    expect(evaluate("unique(lines)", { lines })).toBe(u);
  });

  it("converts case once per text and keeps the result bounded", () => {
    // "ß" upper-cases to "SS": the conversion can outgrow its input.
    const lines = Array.from({ length: 20000 }, () => "ß".repeat(30));
    const scope = { lines };
    const up = evaluate("upper(join(lines))", scope) as string;
    expect(up.length).toBe(MAX_EXPR_STRING);
    expect(evaluate("upper(join(lines))", scope)).toBe(up);
    expect(evaluate('lower("AbC")', {})).toBe("abc");
    expect(evaluate('upper("aBc")', {})).toBe("ABC");
  });

  it("shows small values instead of rounding them to zero", () => {
    expect(renderTemplate("{{ n }}", { n: 1e-11 })).toBe("1e-11");
    expect(renderTemplate("{{ a + b }}", { a: 0.1, b: 0.2 })).toBe("0.3");
    expect(renderTemplate("{{ n }}", { n: 1234567890123.5 })).toBe("1234567890123.5");
    expect(renderTemplate("{{ n }}", { n: 999999999999.5 })).toBe("999999999999.5");
    expect(renderTemplate("{{ n }}", { n: 1.234567890123456 })).toBe("1.234567890123456");
    expect(renderTemplate("{{ n }}", { n: 1000000000000000.1 })).toBe("1000000000000000.1");
    expect(renderTemplate("{{ n }}", { n: 12345678901234.5 })).toBe("12345678901234.5");
  });

  it("averages huge values without overflowing", () => {
    expect(evaluate("avg(xs)", { xs: [Number.MAX_VALUE, Number.MAX_VALUE, Number.MAX_VALUE] })).toBe(Number.MAX_VALUE);
    expect(evaluate("avg(xs)", { xs: [1, 2, 3, 4] })).toBe(2.5);
    expect(evaluate("avg(xs)", { xs: [Number.MAX_VALUE, -Number.MAX_VALUE] })).toBe(0);
  });
});
