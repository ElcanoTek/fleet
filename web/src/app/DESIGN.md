# Web UI design system (#540)

One token-driven system for the whole app (`/chat`, `/orchestrator`, `/admin`,
`/settings`, `/login`). Everything visual keys off the CSS custom properties in
[`globals.css`](globals.css); components — Tailwind arbitrary values and plain
CSS alike — reference tokens, never raw values.

**The rule: no raw colors in `.tsx` — hex literals *or* Tailwind color
utilities.** `text-white` and `text-black` are banned outright and pinned by
`designTokens.test.ts`; the earlier wording said "no raw *hex* colors", and that
loophole is exactly how #890 shipped 16 elements painting `text-white` on a
token-driven fill. If a color you need has no token,
add a *semantic* token to `globals.css` (defined for **both** the dark `:root`
block and the `:root[data-theme="light"]` block), then use
`var(--your-token)`. Never rename or repurpose an existing token — add.

### Foreground on a saturated fill

An element filled with a brand or status color needs a foreground chosen for
*that fill*, in both themes. There are exactly two right answers:

| Fill | Foreground | Why |
|---|---|---|
| `bg-[var(--color-primary)]` | `text-[var(--color-on-primary)]` | The purpose-built token. It cannot be derived — a dark primary needs light text, a light one (yellow, lime, cyan) needs dark text — so a bundle declares it. |
| `bg-[var(--color-accent)]`, `bg-[var(--color-danger)]` | `text-[var(--color-surface-1)]` | Inverts with the theme (near-black in dark, white in light), which is the polarity a saturated fill needs. Themable, so a bundle keeps control. |

Reaching for `--color-text-primary` on a saturated fill is the common mistake:
it is the foreground for *ordinary surfaces*, and on a filled button it is
near-invisible. `designTokens.test.ts` fails the build on that too.

Measured, for the record — white failed on three of the four fills, and on two
of them it failed for **fleet's own** stock palette, not just a white-labeled
one:

```
white on --color-primary (Reklaim #FFDF03) ....  1.33:1  ✗
white on --color-accent  (fleet dark #9da7ef) .  2.28:1  ✗   stock fleet
white on --color-danger  (fleet dark #e08080) .  2.77:1  ✗   stock fleet
surface-1 on those same fills ................  5.9–7.2:1  ✓
```

A *variant-prefixed* fill (`before:bg-…`, `hover:bg-…`) paints something other
than the element's own background, so it is outside this rule — NavRail's active
indicator is a `before:` bar on an element whose real fill is an 18% tint.

Known, deliberate exceptions:

- `lib/serverBranding.ts` — `DEFAULT_BACKGROUND_LIGHT`/`_DARK`, the two
  literals `<meta name="theme-color">` falls back to when a bundle declares
  no background. A meta tag cannot read a CSS variable, so a literal is
  unavoidable *somewhere*; keeping both in one named place (instead of inline
  in `layout.tsx`, where they used to be un-overridable by any bundle) means
  there is exactly one thing to keep in sync with `--color-bg`.
- `chat/ui/ApprovalCards.tsx` — a local light→dark color-inversion map for
  sandboxed HTML previews; those literals describe *content* colors, not UI.

## Token families (globals.css `:root` + light overrides)

- **Type scale** — `--font-size-display/title/subtitle/body-lg/body/caption/
  overline` with matching `--line-height-*`; families `--font-heading/body/
  code/code-ui`. Small UI text steps used in markup: `0.875rem` (controls,
  banners), `0.8125rem` (secondary), `0.75rem` (captions), `0.6875rem`
  (chips/overlines).
- **Surfaces & text** — `--color-bg`, `--color-surface-1/2`, the
  `--gradient-*` surface set, `--color-text-primary/secondary/muted/disabled`,
  `--color-border`/`-strong`/`-subtle`, `--color-overlay-soft/strong`.
- **Brand accents** — `--color-primary(-hover)`, `--color-secondary`,
  `--color-accent`.
- **Status** — two layers, both theme-aware:
  - `--color-success` / `--color-danger` / `--color-warning` (+ `-border`):
    inline text and icons on plain surfaces — stderr lines, diff gutters,
    toast borders, validation errors.
  - `--color-{success,warning,danger}-strong` / `-soft`: the chip/badge/banner
    palette. `-strong` anchors borders and tinted fills (via
    `color-mix(in srgb, var(--color-*-strong) 15%, transparent)`); `-soft` is
    the readable foreground on those tints. Light-theme values are darker,
    AA-contrast counterparts, mirroring the base status tokens.
- **Syntax** — `--color-syntax-*` for code highlighting.
- **Geometry & motion** — `--radius-md` (0.625rem), `--radius-lg` (0.875rem),
  `--radius-xl` (1.125rem), `--radius-pill`; `--space-*`; `--shadow-sm/md/lg`;
  `--transition-fast/base`; `--focus-ring`.

## Shared status primitives (`shared/ui/`)

- **`StatusChip`** — every small status pill: connection state, "Bundled" /
  "Third-party" trust badges, admin health pills. Tones: `success`,
  `warning`, `danger`, `neutral`. Don't hand-roll
  `rounded-full border px-2 py-0.5` + a color trio again; add a tone (token
  pair first) if none fits. `statusToneClass(tone)` exposes just the colors
  for non-pill markup.
- **`NoticeBanner`** — page-level success/error/warning notice: `tone` +
  children; pass margins/width and `role`/`data-testid` through `className` /
  rest props. Compact inline errors inside chat surfaces may stay hand-rolled
  but must use the status tokens.

## Radii

Use the `--radius-*` tokens (`rounded-[var(--radius-md)]` etc.) for new work.
Card/banner shells sit on `--radius-lg`; small controls on `--radius-md`;
pills on `rounded-full`/`--radius-pill`. Legacy steps still in the tree
(`0.6rem` controls, `0.75rem` compact cards, `1rem`/`1.25rem` page panels and
hero cards) are internally consistent families — match the surrounding file;
don't invent new in-between values.

## Focus states

Interactive elements use the shared ring:
`focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]`
(CSS: `box-shadow: var(--focus-ring)`). Text inputs additionally use
`outline-none focus:border-[var(--color-accent)]` — the accent border is the
one input focus treatment; don't substitute other colors.

## Theming

Themes switch via `data-theme` on `<html>` (see `--theme-selector-attribute`).
Anything you add must define both dark and light values, or derive from tokens
that do (e.g. via `color-mix`). Check contrast against the light surfaces —
light-theme status colors are intentionally darker/saturated for WCAG AA.
