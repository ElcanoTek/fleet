import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

/**
 * Sprite-coverage guard.
 *
 * Every icon in this app is an `<svg><use href="/icons/core-icons.svg#NAME">`.
 * An `href` pointing at a symbol id the sprite does not define is not an error
 * anywhere — not at build time, not in the console, not in a Playwright run:
 * `<use>` simply renders nothing. The card keeps its 36px bordered icon box and
 * the box comes out empty, which is exactly how four shipped cards looked:
 *
 *   file-text ... config/default + example-config "Summarize a document",
 *                 and protocolPills' DEFAULT_PILLS — the hardcoded fallback
 *                 rendered whenever the /config fetch fails, i.e. the bare
 *                 fleet experience and every degraded one
 *   book-open ... example-config's third card
 *   globe ....... a client bundle's "Pull data from a site" card
 *   mail ........ the same bundle's "Work the mailbox" card
 *
 * The names were all reasonable; the sprite just didn't have the glyphs. So the
 * fix direction is fixed too: add the glyph to the sprite, never edit a bundle
 * to dodge the gap. Bundles are data and fleet is the engine — the engine owes
 * the common glyph.
 *
 * This test closes the silent-failure hole for everything that lives in THIS
 * repo: literal icon names in the web source, the `*_ICONS` lookup maps, and
 * the empty-state cards of the built-in config/default bundle. Two things it
 * structurally cannot cover, and where the checks for them live instead:
 *
 *   - Out-of-repo bundles. TestRealBundleSanity (internal/clientconfig) checks
 *     a real bundle's card icons against this same sprite when it's run with
 *     FLEET_SANITY_BUNDLE_DIR pointed at one.
 *   - Names computed at runtime (`<Icon name={someVar} />`). Only literals are
 *     visible to a static scan.
 */

const APP_DIR = join(import.meta.dirname ?? __dirname);
const SPRITE = join(APP_DIR, "..", "..", "public", "icons", "core-icons.svg");
const DEFAULT_MANIFEST = join(APP_DIR, "..", "..", "..", "config", "default", "manifest.yaml");

/** Symbol ids the sprite actually defines. */
function spriteIds(): Set<string> {
  const svg = readFileSync(SPRITE, "utf8");
  return new Set(Array.from(svg.matchAll(/<symbol\s+id="([^"]+)"/g), (m) => m[1]));
}

/**
 * Shipping source only. Tests are excluded because they render to no one and a
 * fixture is free to use a made-up icon name — including this file, whose prose
 * quotes the very patterns scanned for below.
 */
function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === ".next") continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) sourceFiles(full, out);
    else if (/\.tsx?$/.test(entry) && !/\.test\.tsx?$/.test(entry)) out.push(full);
  }
  return out;
}

/**
 * An icon name is a lowercase kebab slug. The pattern doubles as the filter
 * that keeps emoji out of the results: `task_templates[].icon` is an emoji
 * ("🔎"), rendered as text and not through the sprite at all.
 */
const NAME = "[a-z][a-z0-9-]*";

type Ref = { name: string; where: string };

/** Icon names referenced as literals by the web source. */
function sourceRefs(files: string[]): Ref[] {
  const refs: Ref[] = [];
  const patterns = [
    // <Icon name="x" />, <PillIcon name="x" /> — [^>]* keeps the match inside
    // the one element, so an unrelated later `name=` can't be attributed to it.
    new RegExp(`<(?:Icon|PillIcon)\\b[^>]*?\\bname="(${NAME})"`, "g"),
    // icon="x" (JSX prop, e.g. Menu's MenuItem) and icon: "x" (object literal,
    // e.g. protocolPills' DEFAULT_PILLS).
    new RegExp(`\\bicon=\\{?"(${NAME})"`, "g"),
    new RegExp(`\\bicon:\\s*"(${NAME})"`, "g"),
  ];

  for (const file of files) {
    const src = readFileSync(file, "utf8");
    const rel = file.replace(APP_DIR, "");
    for (const re of patterns) {
      for (const m of src.matchAll(re)) {
        refs.push({ name: m[1], where: `${rel} (${m[1]})` });
      }
    }
    // Slug → glyph lookup maps (catalog.ts's CATEGORY_ICONS). Their values are
    // icon names but their keys are arbitrary, so the `icon:`-keyed patterns
    // above miss them entirely; match the map by name and read its values.
    for (const block of src.matchAll(/const\s+\w*ICONS\w*[^=]*=\s*\{([\s\S]*?)\n\};/g)) {
      for (const v of block[1].matchAll(new RegExp(`:\\s*"(${NAME})"`, "g"))) {
        refs.push({ name: v[1], where: `${rel} (${v[1]})` });
      }
    }
  }
  return refs;
}

/**
 * Icon names in the built-in bundle's empty-state cards. Scanned with a regex
 * rather than parsed: the web has no YAML dependency, and the shape needed here
 * is one flat `icon: "name"` per card. Scoped to the `empty_state:` block so
 * the emoji `task_templates` icons below it stay out of scope regardless.
 */
function defaultBundleRefs(): Ref[] {
  const yaml = readFileSync(DEFAULT_MANIFEST, "utf8");
  const start = yaml.indexOf("\nempty_state:");
  expect(start, "config/default/manifest.yaml has no empty_state block").toBeGreaterThan(-1);
  // Ends at the next top-level key (a non-indented, non-comment line).
  const rest = yaml.slice(start + 1);
  const end = rest.search(/\n(?![\s#])[A-Za-z_]/);
  const block = end === -1 ? rest : rest.slice(0, end);

  return Array.from(block.matchAll(new RegExp(`\\bicon:\\s*"(${NAME})"`, "g")), (m) => ({
    name: m[1],
    where: `config/default/manifest.yaml (${m[1]})`,
  }));
}

describe("core-icons sprite coverage", () => {
  const ids = spriteIds();
  const files = sourceFiles(APP_DIR);
  const refs = [...sourceRefs(files), ...defaultBundleRefs()];

  it("parses the sprite and finds references (guards against a vacuous pass)", () => {
    // Every assertion below is trivially satisfiable if either side comes back
    // empty — a moved sprite, a renamed manifest key, or a broken walk.
    expect(ids.size).toBeGreaterThan(40);
    expect(files.length).toBeGreaterThan(50);
    expect(refs.length).toBeGreaterThan(30);
  });

  it("the built-in bundle's cards resolve", () => {
    // Called out separately from the sweep below because these are the ones a
    // bare install renders: a gap here is the first screen of a new deployment.
    const missing = defaultBundleRefs()
      .filter((r) => !ids.has(r.name))
      .map((r) => r.where);
    expect(missing, `config/default cards name glyphs the sprite lacks: ${missing.join(", ")}`)
      .toEqual([]);
  });

  it("every referenced icon name is a symbol in the sprite", () => {
    const missing = [...new Set(refs.filter((r) => !ids.has(r.name)).map((r) => r.where))].sort();
    expect(
      missing,
      `these render an empty icon box — add the glyph to ` +
        `web/public/icons/core-icons.svg: ${missing.join(", ")}`,
    ).toEqual([]);
  });
});
