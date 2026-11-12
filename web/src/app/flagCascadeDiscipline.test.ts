import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

import tailwindcss from "@tailwindcss/postcss";
import { render, screen } from "@testing-library/react";
import postcss from "postcss";
import { createElement } from "react";
import { describe, expect, it } from "vitest";

import { RevealButton } from "./settings/ui/atoms";

const SRC_DIR = join(import.meta.dirname ?? __dirname, "..");
const GLOBALS_CSS = join(SRC_DIR, "app", "globals.css");

function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === "i18n") continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) sourceFiles(full, out);
    else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) out.push(full);
  }
  return out;
}

// Matches a Tailwind utility token glued directly onto `${` inside a JSX
// `className={`...`}` template literal — `@tailwindcss/oxide` tokenizes the
// `${` boundary as part of the candidate and silently drops the utility.
const GLUED_TAILWIND_INTERPOLATION_RE =
  /className=\{`[^`]*?(?:^|\s)((?:gap|size|w|h|min-w|max-w|min-h|max-h|p|px|py|pt|pr|pb|pl|ps|pe|m|mx|my|mt|mr|mb|ml|text|font|leading|tracking|bg|border|rounded|shadow|flex|grid|items|justify|animate|opacity|z)-\S+|[^\s`$]*(?:\]|\)))\$\{/g;

describe("Flag @flag/* CSS cascade and Tailwind v4 Oxide discipline", () => {
  it("never glues a Tailwind utility directly onto `${` in className template literals", () => {
    const files = sourceFiles(SRC_DIR);
    expect(files.length).toBeGreaterThan(50);

    const offenders: string[] = [];
    for (const file of files) {
      const src = readFileSync(file, "utf8");
      const rel = file.slice(SRC_DIR.length + 1);
      for (const m of src.matchAll(GLUED_TAILWIND_INTERPOLATION_RE)) {
        offenders.push(`${rel}: ${m[1]}\${...}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("keeps globals.css legacy .btn, bare table, and modal select rules from clobbering @flag/* primitives", () => {
    const css = readFileSync(GLOBALS_CSS, "utf8");

    // .btn / .btn-* must be layered and excluded from @flag/button ([data-slot="button"]).
    expect(css).toMatch(/@layer components\s*\{[\s\S]*?\.btn:where\(:not\(\[data-slot="button"\]\)\)/);
    expect(css).not.toMatch(/^\s*\.btn\s*\{/m);

    // Bare table/th/td defaults must sit in @layer base and exclude @flag/table slots.
    expect(css).toMatch(/@layer base\s*\{[\s\S]*?table:where\(:not\(\[data-slot="table"\]\)\)/);
    expect(css).toMatch(/th:where\(:not\(\[data-slot="table-head"\]\)\)/);
    expect(css).toMatch(/td:where\(:not\(\[data-slot="table-cell"\]\)\)/);
    expect(css).not.toMatch(/^th,\s*\ntd\s*\{/m);

    // Analytics `.num` alignment helper lives in @layer components.
    expect(css).toMatch(/@layer components\s*\{[\s\S]*?\.num\s*\{[\s\S]*?text-align:\s*right;/);

    // .modal-lg select excludes @flag/native-select via zero-specificity :where(),
    // and .task-gate-options no longer paints a duplicate SVG chevron on select.
    expect(css).toContain('.modal-lg select:where(:not([data-slot="native-select"]))');
    expect(css).not.toContain(".task-modal .task-gate-options select");
  });

  it("emits the previously dropped Oxide utilities in the compiled Tailwind v4 stylesheet", async () => {
    const rawCss = readFileSync(GLOBALS_CSS, "utf8");
    const result = await postcss([tailwindcss()]).process(rawCss, {
      from: GLOBALS_CSS,
    });

    expect(result.css).toContain(".size-\\[0\\.85rem\\]");
    expect(result.css).toContain(".size-\\[0\\.95rem\\]");
    expect(result.css).toContain(".leading-\\[1\\.5\\]");
    expect(result.css).toContain(".min-\\[901px\\]\\:grid-cols-2");
    expect(result.css).toContain(".gap-2\\.5");
  });

  it("preserves RevealButton resting border class", () => {
    render(
      createElement(RevealButton, {
        open: false,
        closedLabel: "Add provider",
        onClick: () => {},
      }),
    );
    const button = screen.getByRole("button", { name: "Add provider" });
    expect(button).toHaveAttribute("data-slot", "button");
    expect(button).toHaveClass("border-border");
  });
});
