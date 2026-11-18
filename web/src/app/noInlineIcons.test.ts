import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

import { describe, expect, it } from "vitest";

/**
 * No hand-drawn icons.
 *
 * Every icon in fleet is a symbol from Flag's sprite, rendered by `<Icon>`
 * (web/AGENTS.md). A glyph Flag lacks is added to Flag, not drawn in a
 * component: fleet once drew its own share marks, lock, hourglass, unarchive
 * and branch glyphs, each at a slightly different weight from the sprite icon
 * beside it, and they all moved to Flag.
 *
 * This guard keeps it that way. An `<svg>` written in a component must be one
 * of the things listed below — the `<Icon>` component itself (the one `<use>`
 * that renders a sprite symbol), or a
 * drawing of data or motion (a chart, the loading logo, a spinner). Adding a
 * file here is a statement that its `<svg>` is not an icon; an icon goes to
 * Flag instead.
 */

const APP_DIR = join(import.meta.dirname ?? __dirname);

// path (relative to src/app) → how many `<svg` it may contain, and why.
const ALLOWED: Record<string, { count: number; why: string }> = {
  "shared/ui/Icon.tsx": { count: 1, why: "renders a sprite symbol with <use>" },
  "chat/ui/Icon.tsx": { count: 1, why: "renders a sprite symbol with <use>" },
  "chat/ui/LoadingLogo.tsx": { count: 2, why: "the animated loading logo" },
  "chat/ui/genui/GenerativeCard.tsx": { count: 1, why: "a chart drawn from card data" },
  "orchestrator/AdoptionPanel.tsx": { count: 2, why: "charts" },
  "orchestrator/SLAReportPanel.tsx": { count: 1, why: "a chart" },
  "orchestrator/UsagePanel.tsx": { count: 2, why: "charts" },
  "orchestrator/TaskCreateModal.tsx": { count: 1, why: "the submit spinner (motion, not an icon)" },
};

function sources(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      // Vendored highlighter and fonts hold no components.
      if (name === "highlight" || name === "fonts") continue;
      sources(path, out);
    } else if (/\.tsx$/.test(name) && !/\.test\.tsx$/.test(name)) {
      out.push(path);
    }
  }
  return out;
}

describe("no hand-drawn icons", () => {
  it("an <svg> in a component is a sprite <use>, a chart, the logo or a spinner", () => {
    const found: Record<string, number> = {};
    for (const file of sources(APP_DIR)) {
      // Count opening tags in code, not mentions in comments.
      const code = readFileSync(file, "utf8").replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
      const n = (code.match(/<svg[\s>]/g) ?? []).length;
      if (n > 0) found[relative(APP_DIR, file).split("\\").join("/")] = n;
    }
    const expected = Object.fromEntries(Object.entries(ALLOWED).map(([file, v]) => [file, v.count]));
    expect(found).toEqual(expected);
  });
});
