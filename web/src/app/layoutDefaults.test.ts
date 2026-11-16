import { readFileSync } from "node:fs";
import { join } from "node:path";

import postcss from "postcss";
import { describe, expect, it } from "vitest";

// One long unbreakable value (a 40-character name, an e-mail address) must not
// be able to widen the screen it is shown on. The two layout defaults that make
// that true are easy to lose, because nothing looks wrong until someone saves a
// long name: globals.css explains them beside the `:where(.grid)` rule. These
// tests keep the defaults in place and stop a new column-less grid from being
// added to the product stylesheet; web/e2e/mocked/layout-long-values.spec.ts
// checks the result in a browser.

const GLOBALS_CSS = join(import.meta.dirname ?? __dirname, "globals.css");

describe("layout defaults for long values", () => {
  const css = readFileSync(GLOBALS_CSS, "utf8");
  const root = postcss.parse(css);

  it("gives every grid in globals.css a column it can shrink", () => {
    const offenders: string[] = [];
    root.walkRules((rule) => {
      const decls = new Map<string, string>();
      rule.walkDecls((d) => {
        if (d.parent === rule) decls.set(d.prop, d.value);
      });
      const display = decls.get("display");
      if (display !== "grid" && display !== "inline-grid") return;
      // A grid that names its columns, or that flows in columns, has chosen.
      // Everything else gets one implicit column as wide as its widest child.
      const chosen =
        decls.has("grid-template-columns") ||
        decls.has("grid-template") ||
        decls.has("grid-template-areas") ||
        (decls.get("grid-auto-flow") ?? "").includes("column");
      if (!chosen) offenders.push(`${rule.selector.replace(/\s+/g, " ")} (line ${rule.source?.start?.line ?? "?"})`);
    });
    // Fix: add `grid-template-columns: minmax(0, 1fr);` to the rule, which is
    // the same single column, allowed to be narrower than its content.
    expect(offenders).toEqual([]);
  });

  it("keeps the defaults for Tailwind's `grid` class, badges and long words, in @layer base", () => {
    const inBase: string[] = [];
    root.walkAtRules("layer", (layer) => {
      if (layer.params.trim() !== "base") return;
      layer.walkRules((rule) => {
        rule.walkDecls((d) => {
          inBase.push(`${rule.selector.replace(/\s+/g, " ")} { ${d.prop}: ${d.value} }`);
        });
      });
    });
    // Tailwind's bare `grid`, except grids that flow in columns or position
    // their own column (see the comment beside the rule).
    const gridDefault = inBase.find((r) => r.startsWith(":where(.grid):where("));
    expect(gridDefault).toBeDefined();
    expect(gridDefault).toContain("{ grid-template-columns: minmax(0, 1fr) }");
    for (const left of ["grid-flow-col", "auto-cols-", "place-content-", "justify-center"]) {
      expect(gridDefault).toContain(`[class*="${left}"]`);
    }
    expect(inBase).toContain(':where([data-slot="badge"]) { max-width: 100% }');
    // A word longer than its line may break; text that fits is unchanged.
    expect(inBase).toContain(":where(html) { overflow-wrap: break-word }");
  });
});
