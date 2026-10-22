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
| `bg-[var(--color-primary-fill)]`, hover (when enabled) `bg-[var(--color-primary-fill-hover)]` | `text-[var(--color-on-primary-fill)]` | The measured pair. A bundle declares `on_primary` (a dark primary needs light text, a light one — yellow, lime, cyan — needs dark text), but the brand pair itself is not held to 4.5:1 (fleet's own is 4.46:1), so `/theme.css` nudges the primary into a fill that reaches 4.5:1 under the label for every palette. Every filled button, chip, selected option and ON switch (track + knob) uses it, app-wide. The raw `--color-primary` is only for what has nothing painted on it (a nav row's active bar, a checkbox accent, tints, borders), and `--color-primary-hover` is never a button hover: it is LIGHTER than the primary in dark. |
| `bg-[image:var(--gradient-action-fill)]`, hover (when enabled) `bg-[image:var(--gradient-action-fill-hover)]` | `text-[var(--color-on-primary-fill)]` | The action gradient (the composer's Send circle, the empty-state prompt's Run button): primary → secondary between measured stops (`--color-primary-fill` → `--color-secondary-fill`), so the label holds 4.5:1 on every point of it for any palette. Never put a label on the raw `--gradient-action-primary` (its stops are not measured); an inline `linear-gradient()` under the label may use only the `-fill` stops. |
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

- `internal/webui/brand.go` (Go, outside this tree) —
  `defaultBackgroundLight`/`Dark`, the two literals `<meta name="theme-color">`
  and the PWA manifest fall back to when a bundle declares no background. A
  meta tag cannot read a CSS variable, so a literal is unavoidable
  *somewhere*; keeping both in one named place means there is exactly one
  thing to keep in sync with `--color-bg`.
- `chat/ui/ApprovalCards.tsx` — a local light→dark color-inversion map for
  sandboxed HTML previews; those literals describe *content* colors, not UI.

## Token families (globals.css `:root` + light overrides)

- **Connection picker** — `--color-accent-strong`/`--color-on-accent-strong`
  (switch on, count badge), `--color-text-subtle` (row descriptions, group
  headers, counts), `--color-divider`, `--color-selected-bg/-border` (active
  row, selected filter, popover/modal border), `--color-pill-border` /
  `--color-pill-hover-bg`, `--color-switch-track-on/-off`, `--color-switch-knob`
  / `--color-switch-knob-on` (+ `--shadow-switch-knob`), `--color-match-bg`, `--color-state-ok/-warn/-deny`
  (kickers, dots, status icons), `--color-panel-{ok,warn,deny}-{bg,border}`,
  `--color-icon-tile`, `--color-rail-bg`, `--color-scrim`, `--shadow-popover`.
  A copy of flag's "Connection picker components" block (flag
  COMPONENTS.md "Connection Picker"); values from the Tools picker design.
  These defaults are fleet's purples. A bundle's palette repaints the
  brand-tinted ones (accent-strong, on-accent-strong, switch-knob-on, link,
  text-subtle, divider, selected-bg/-border, match-bg, icon-tile, scrim) from
  its own `primary`, `accent`, `text_muted`, `border_subtle`, `border_strong`,
  `rail_active` and `overlay_strong` unless it pins them (docs/BRANDING.md).

- **Type scale** — `--font-size-display/title/subtitle/body-lg/body/caption/
  overline` with matching `--line-height-*`; families `--font-heading/body/
  code/code-ui`. Small UI text steps used in markup: `0.875rem` (controls,
  banners), `0.8125rem` (secondary), `0.75rem` (captions), `0.6875rem`
  (chips/overlines).
- **Typefaces** — exactly two, everywhere: **Nebula Sans**
  (`--font-heading`/`--font-body`) and **Hack** (`--font-code`/`--font-code-ui`).
  Both are self-hosted from `src/app/fonts/`, declared in the single vendored
  sheet `src/app/fonts/fonts.css` (a copy of flag's `design-system/fonts/fonts.css`)
  which `src/main.tsx` imports (not `globals.css` — Tailwind would inline it
  without rebasing its `url()`s) and which is the ONLY place a family name is
  written — the token families above read `--font-brand` /
  `--font-code-brand` / `--font-code-ui-brand` from it. There is no font CDN
  request and no font-loader wrapper: the `url()`s resolve through the bundler
  (Vite), so a moved or renamed file fails the build instead of 404-ing at
  runtime. No `<link rel=preload>` is emitted per face; `font-display: swap`
  covers the gap and no hand-written preload is added, because a hand-written
  one would point at a hashed asset path and rot silently.
  - Licences: Nebula Sans is SIL OFL 1.1, Hack is MIT (plus Bitstream Vera for
    its Vera-derived glyphs). Both permit redistribution under this repo's MIT
    licence — that is why they were chosen — and both REQUIRE the licence text
    to travel with the binaries, so `fonts/nebula-sans/OFL.txt` and
    `fonts/hack/LICENSE.md` must stay beside the `.woff2` files.
  - The vendored Flag files (`fonts/fonts.css`, `public/icons/core-icons.svg`,
    `public/logos/fleet-mark.svg`, listed in the repo-root `flag.lock`) are the
    Flag design system — Copyright (c) 2026 ElcanoTek, Inc. — MIT; its licence
    text travels with them as `src/app/FLAG-LICENSE`. The fonts keep their own
    licences above.
  - Icons: some glyphs in Flag's core-icons.svg are drawn after Lucide
    (https://lucide.dev), ISC License — the full notice is retained at the top
    of the vendored sprite (`public/icons/core-icons.svg`).
  - These are the COMPLETE builds, not the narrower subsets both upstreams also
    publish. Do not substitute a subset: a missing glyph does not error, it
    silently falls back to a system font for that one character, so a name
    renders in two typefaces mid-word.
  - **Nebula Sans has PROPORTIONAL figures** (digit advances 407–625 per 1000
    em units; `1` is a third narrower than `8`). IBM Plex Sans, the face this
    replaced, had every digit at 600, so numeric columns in the sans face used
    to align for free and now do not. `globals.css` therefore makes
    `font-variant-numeric: tabular-nums` the `@layer base` default for every
    `table` — including assistant-authored markdown tables — and applies it
    explicitly to the numeric readouts that sit outside a table. **Any new
    columnar number in the sans face needs tabular figures**; put it in a
    `table` and it inherits them, otherwise say so on the element. Hack is
    monospaced at a uniform 0.602 em, so `--font-code` needs none of this.
  - No other face may be added, self-hosted, or CDN-loaded. The tail of each
    stack is generic fallback keywords only (`system-ui`, `"Segoe UI"`,
    `sans-serif`, `ui-monospace`, `monospace`, …). The proprietary Dubai face in
    particular must never appear here.
- **Surfaces & text** — `--color-bg`, `--color-surface-1/2`, the
  `--gradient-*` surface set, `--color-text-primary/secondary/muted/disabled`,
  `--color-border`/`-strong`/`-subtle`, `--color-overlay-soft/strong`.
  `--color-overlay-strong` is the modal **scrim** only; a recessed in-flow
  surface (code/output well, preview toolbar, tool-result panel) uses
  `--color-surface-inset`. The two match in dark mode, but a light scrim must
  stay a visible slate film while a well inside a card stays near-white.
- **Brand accents** — `--color-primary(-hover)`, `--color-secondary`,
  `--color-accent`.
- **Status** — two layers, both theme-aware:
  - `--color-success` / `--color-danger` / `--color-warning` (+ `-border`):
    inline text and icons on plain surfaces — stderr lines, diff gutters,
    toast borders, validation errors.
  - `--color-{success,warning,danger}-strong` / `-soft`: the chip/badge/banner
    palette. `-strong` anchors borders and tinted fills (via
    `color-mix(in srgb, var(--color-*-strong) 15%, transparent)`); `-soft` is
    the readable foreground on those tints — a text color, never a fill.
    Every `-soft` value clears WCAG AA (4.5:1) on its own 15–20% tint over
    every surface fleet and its bundles ship, in both themes; status colors
    are not bundle-themable, so `designTokens.test.ts` pins this.
  - `--color-{success,warning,danger}-text`: status **words** on a card
    ("Not applied:", "Timed out", a failed card's title). The base status
    color with 25–30% of `--color-text-primary` mixed in, so it is darker in
    light and lighter in dark; the base tokens fail 4.5:1 on tinted cards.
    `--color-{success,warning,danger}-tint` is the 15% wash behind a status
    icon; success and danger icons keep the base color and need 3:1 on it.
    The warning icon is `--color-warning-icon` (15% of the strongest text
    mixed in): the base amber fell to 2.77:1 on its wash over a mid-gray
    light `surface_2`.
- **Approval cards** (`chat/ui/Approval*.tsx`) — one fill token per state:
  `--color-card-bg` (neutral: pending without a plain-words layout, plan,
  running, declined, expired), `--color-card-pending-bg` (a card asking for a
  decision), `--color-card-{success,warning,danger}-bg` (outcomes; the danger
  wash is 4% in dark, 7% in light) and `--color-card-record-bg` (a notify
  record). Muted text and links on those fills use `--color-card-text-muted`
  and `--color-card-link`: the themable muted/accent with 20% of the strongest
  text mixed in, because a tinted card costs them ~0.5:1. The running card's
  progress bar is `--color-progress` (the card link color), never
  `--color-primary`: a bundle's primary can be a light brand fill (a yellow
  with dark button text) that cannot reach 3:1 on the track. For the same
  reason the help guides paint links in `--color-link`. Filled buttons
  (Approve, Check result, Retry failed, Approve 2 of 3, and every other
  filled button in the app) paint
  `--color-on-primary-fill` on `--color-primary-fill` and hover (when
  enabled) to `--color-primary-fill-hover`, never `--color-primary` or a faded
  `hover:opacity-*`: the brand pair can miss 4.5:1 (fleet's own is 4.46:1),
  so the fill is the primary nudged until it passes, measured per bundle by
  `/theme.css` (see `docs/BRANDING.md`). Don't mix a card
  fill or paint a base status color as text inline in those files:
  `approvalCardContrast.test.ts` measures every state's pairs (4.5:1 text,
  3:1 icons, both themes, over `--color-bg`..`--color-surface-2`) from these
  tokens and bans the inline forms.
- **Syntax** — `--color-syntax-*` for code highlighting.
- **Geometry & motion** — `--radius-md` (0.625rem), `--radius-lg` (0.875rem),
  `--radius-xl` (1.125rem), `--radius-pill`; `--space-*`; `--shadow-sm/md/lg`;
  `--transition-fast/base`; `--focus-ring`.

## Shared status primitives (`shared/ui/`)

- **`StatusChip`** — every small status pill, one size everywhere:
  connection state, "Bundled" / "Workspace" / "Built-in" badges (settings
  `ConnBadge` is a thin wrapper), admin health pills, tool-output error and
  exit chips, the model picker's tags, AND the Operations Center's task, SLA,
  log, dataset and learned-instruction states. General tones: `success`,
  `warning`, `danger`, `neutral`, `accent` (a deliberate non-default such as
  an admin override). Run tones (`run-pending` … `run-cancelled`) keep each
  lifecycle state's own `--color-status-<state>-*` colors, because a run has
  ~10 states four tones cannot tell apart; `runStateTone(state)` maps a status
  value to its tone, and `RunStatusChip` renders one. **Case rule:** `caps`
  (uppercase, tracked, semibold) is for run/lifecycle states, which are system
  values scanned down a table column; every other badge keeps normal case.
  The old `.status-badge` / `.status-<state>` / `.sla-badge*` / `.logs-badge`
  classes are unstyled test hooks now. Don't hand-roll
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

## Button and pill shapes

Shape tells a reader what can be pressed. Every **text button** (Send,
Cancel, Done, Share, Create, Retry, Add memory, the orchestrator `.btn`
family, the settings `btnClass`) uses `--radius-md`. Full pills
(`rounded-full` / `--radius-pill`) are reserved for things that are not
buttons or are chips: status and info badges, removable tag/label/filter
chips, choice chips that add a value, segmented tracks, switch tracks, avatars,
count bubbles, round icon-only buttons and progress bars. A helper that builds
both (GenerativeCard's `chipButton` / `actionButton`) has to be split, not
shared.

## Segmented controls

A choice between 2–4 mutually exclusive options (theme, schedule mode,
list/week, simple/cron, memory views, the email preview's inbox and size) is
ONE look: `.seg` / `.seg-option` in `globals.css`, a pill track with the active
option on `--color-primary-fill` / `--color-on-primary-fill`. React button groups use
`shared/ui/Segmented` (aria-pressed); a radiogroup or tablist with its own
keyboard handling keeps its markup and semantics and takes the classes
(`.seg--md` is the form-field size). Underlined page tabs, filter chips with
counts and agent-generated choice cards are different patterns.

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
