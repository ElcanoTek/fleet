import { describe, expect, it } from "vitest";
import { evaluate, parseExpr, renderTemplate, ExprError } from "./expr";
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
