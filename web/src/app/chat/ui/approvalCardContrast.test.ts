import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { contrastRatio, over, paint, resolveColor, themesFrom, toHex, type RGB, type Theme } from "../../cssContrast";

/**
 * WCAG AA on the approval cards, in both themes (designer contrast check,
 * 2026-10-20). The cards paint translucent fills over the page, so a token
 * that reads well on the page can fail on a card: fleet's light muted text
 * was ~5:1 on the page and 3.7-4.4:1 on a tinted card, and the base status
 * colors used for status words ("Not applied:", "Check didn't match") were
 * as low as 3.6:1 (light warning) and 4.4:1 (dark danger). Every value here
 * is read from the real globals.css and resolved the way the browser does
 * (var(), nested color-mix with its alpha rules), then composited over the
 * card's fill over the page, so a retuned token or a heavier tint fails this
 * test with the exact pair.
 *
 * Thresholds: 4.5:1 for text (1.4.3), 3:1 for icons and other graphics
 * (1.4.11). The backdrop under a card is the page: --color-bg with the page
 * gradient, which never leaves the range --color-bg .. --color-surface-2, so
 * every pair is checked over bg, surface-1 and surface-2.
 *
 * Scope: fleet's own palette (which the elcano bundle repeats exactly). The
 * primary button (on-primary on --color-primary, both bundle-owned brand
 * colors) is not a card token and is not checked here. Text, accent and
 * surface tokens are bundle-themable; the card tokens derive
 * from them, so a bundle's palette gets the same lift, but a bundle's values
 * are checked in its own repo, not here.
 */

const APP_DIR = join(import.meta.dirname ?? __dirname, "..", "..");
const css = readFileSync(join(APP_DIR, "globals.css"), "utf8");
const themes = themesFrom(css);

const TEXT = 4.5;
const GRAPHIC = 3;

type Pair = { what: string; fg: string; layers: string[]; min: number };

const v = (name: string) => `var(--${name})`;

// The four neutral text roles every card uses.
function textOn(fill: string): Pair[] {
  return [
    { what: "primary text", fg: v("color-text-primary"), layers: [fill], min: TEXT },
    { what: "secondary text (subtitle, note)", fg: v("color-text-secondary"), layers: [fill], min: TEXT },
    { what: "muted text (ids, countdown, footer, Details)", fg: v("color-card-text-muted"), layers: [fill], min: TEXT },
    { what: "link / eyebrow (Open in …, Show N more)", fg: v("color-card-link"), layers: [fill], min: TEXT },
  ];
}

// StatusChip: -soft text on a 15% -strong tint, sitting on the card.
function chip(tone: "success" | "warning" | "danger", fill: string, label: string): Pair {
  return {
    what: `${label} chip text`,
    fg: v(`color-${tone}-soft`),
    layers: [fill, `color-mix(in srgb, var(--color-${tone}-strong) 15%, transparent)`],
    min: TEXT,
  };
}

function statusWords(tone: "success" | "warning" | "danger", fill: string, label: string): Pair {
  return { what: `${label} (status words)`, fg: v(`color-${tone}-text`), layers: [fill], min: TEXT };
}

function statusIcon(tone: "success" | "warning" | "danger", fill: string, label: string): Pair {
  return { what: `${label} icon on its wash`, fg: v(`color-${tone}`), layers: [fill, v(`color-${tone}-tint`)], min: GRAPHIC };
}

const CARD = v("color-card-bg");
const PENDING = v("color-card-pending-bg");
const SUCCESS = v("color-card-success-bg");
const WARNING = v("color-card-warning-bg");
const DANGER = v("color-card-danger-bg");
const RECORD = v("color-card-record-bg");
// The record box / record list inside a card: an opaque surface-1 on the fill.
const BOX = v("color-surface-1");

// Every card state, and what is painted on it.
const STATES: Record<string, Pair[]> = {
  pending: [
    ...textOn(CARD),
    statusWords("danger", CARD, "Couldn't submit your decision"),
  ],
  "pending, plain words": [
    ...textOn(PENDING),
    chip("warning", PENDING, '"Deal is Active"'),
    chip("danger", PENDING, "danger flag"),
    ...textOn(BOX),
    chip("warning", BOX, '"Deal is Active" in the record box'),
  ],
  "pending email send": [...textOn(PENDING)],
  plan: [...textOn(CARD), chip("warning", CARD, "flag on a plan row")],
  running: [
    ...textOn(CARD),
    { what: "progress bar fill on its track", fg: v("color-primary"), layers: [CARD, v("color-overlay-soft")], min: GRAPHIC },
  ],
  completed: [
    ...textOn(CARD),
    statusWords("success", CARD, "Email sent / Command executed title"),
    ...textOn(SUCCESS),
    statusIcon("success", SUCCESS, "outcome"),
    chip("success", SUCCESS, "Done"),
    ...textOn(BOX),
    statusIcon("success", BOX, "row"),
    chip("success", BOX, "Done in the record list"),
  ],
  failed: [
    ...textOn(CARD),
    statusWords("danger", CARD, "Send failed title"),
    ...textOn(DANGER),
    statusWords("danger", DANGER, '"Not applied:" / "1 failed"'),
    statusIcon("danger", DANGER, "outcome"),
    chip("danger", DANGER, "Not applied"),
    statusWords("danger", BOX, '"Not applied:" in the record list'),
    statusIcon("danger", BOX, "row"),
    chip("danger", BOX, "Not applied in the record list"),
  ],
  "check didn't match": [
    ...textOn(WARNING),
    statusWords("warning", WARNING, '"Applied, but the read-back didn\'t match:"'),
    statusIcon("warning", WARNING, "outcome"),
    chip("warning", WARNING, "Check didn't match"),
    statusWords("warning", BOX, "read-back note in the record list"),
    statusIcon("warning", BOX, "row"),
  ],
  "outcome unclear": [...textOn(WARNING), statusIcon("warning", WARNING, "outcome"), chip("warning", WARNING, "Outcome unclear")],
  "partly done": [...textOn(PENDING)],
  declined: [...textOn(CARD)],
  expired: [
    statusWords("danger", PENDING, '"Timed out" countdown'),
    statusWords("danger", CARD, '"Timed out — the action was not taken."'),
    ...textOn(PENDING),
  ],
  "notify record": [
    ...textOn(RECORD),
    { what: "record icon on its wash", fg: v("color-text-secondary"), layers: [RECORD, RECORD], min: GRAPHIC },
  ],
  "no connectors (schedule card)": [statusWords("warning", CARD, '"No connectors." note')],
};

const BACKDROPS = ["color-bg", "color-surface-1", "color-surface-2"];

function measure(theme: Theme, pair: Pair, backdrop: string): { ratio: number; fg: RGB; bg: RGB } {
  const base = over(resolveColor(v(backdrop), theme), [0, 0, 0]);
  const bg = paint(base, ...pair.layers.map((l) => resolveColor(l, theme)));
  const fg = over(resolveColor(pair.fg, theme), bg);
  return { ratio: contrastRatio(fg, bg), fg, bg };
}

describe("approval card contrast (WCAG AA)", () => {
  for (const mode of ["light", "dark"] as const) {
    it.each(Object.keys(STATES))(`%s card (${mode})`, (state) => {
      const failures: string[] = [];
      for (const pair of STATES[state]) {
        for (const backdrop of BACKDROPS) {
          const { ratio, fg, bg } = measure(themes[mode], pair, backdrop);
          if (ratio < pair.min) {
            failures.push(
              `${pair.what}: ${toHex(fg)} on ${toHex(bg)} (over --${backdrop}) = ${ratio.toFixed(2)}:1, needs ${pair.min}:1`,
            );
          }
        }
      }
      expect(failures).toEqual([]);
    });
  }

  it("resolves every card token in both themes (guards a typo'd var())", () => {
    for (const mode of ["light", "dark"] as const) {
      for (const pairs of Object.values(STATES)) {
        for (const pair of pairs) {
          expect(() => resolveColor(pair.fg, themes[mode])).not.toThrow();
          for (const l of pair.layers) expect(() => resolveColor(l, themes[mode])).not.toThrow();
        }
      }
    }
  });
});

/**
 * The pairs above are only true if the cards paint these tokens. The approval
 * card files must not mix their own fills or paint a base status color as
 * text: a hand-mixed `color-mix(…)` fill or `color: var(--color-danger)` is
 * exactly the drift this test cannot see.
 */
describe("approval cards use the measured tokens", () => {
  const FILES = ["ApprovalCards.tsx", "ApprovalReadableCard.tsx", "ApprovalResultCard.tsx", "ApprovalGroupCard.tsx"];
  const BANNED: [RegExp, string][] = [
    [/\bbg-\[color-mix\(/, "a hand-mixed fill: use a --color-card-*-bg token"],
    [/\bbackground(Color)?\s*:\s*["'`][^"'`]*color-mix\(/, "a hand-mixed fill: use a --color-card-*-bg token"],
    [/\bcolor\s*:\s*["'`]var\(--color-(success|warning|danger)\)/, "status words need the -text token"],
    [/\btext-\[var\(--color-(success|warning|danger)\)\]/, "status words need the -text token"],
    [/\b(?:text|color)\S*var\(--color-text-muted\)/, "muted text on a card is --color-card-text-muted"],
    [/\btext-\[var\(--color-accent\)\]/, "link text on a card is --color-card-link"],
  ];

  it.each(FILES)("%s", (file) => {
    const offenders: string[] = [];
    readFileSync(join(import.meta.dirname ?? __dirname, file), "utf8")
      .split("\n")
      .forEach((line, i) => {
        for (const [re, why] of BANNED) if (re.test(line)) offenders.push(`${file}:${i + 1} — ${why}`);
      });
    expect(offenders).toEqual([]);
  });
});
