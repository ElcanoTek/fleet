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
