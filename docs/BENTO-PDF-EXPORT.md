# Bento PDF export — what shipped, and what it is not

`bento_doc.py pdf DECK.bento.html` renders a Bento deck to a PDF, one page per
visible slide, from inside the sandbox with **no browser**. This page records
what that export reproduces, what it does not, why it was built this way, and
the tests that keep it honest.

Design note for the `bento-slides` skill
([SKILL.md](../internal/clientconfig/builtin_skills/bento-slides/SKILL.md),
[`scripts/bento_pdf.py`](../internal/clientconfig/builtin_skills/bento-slides/scripts/bento_pdf.py)).
The skill's other guarantees — the offline-deck guard, the vendored app shell,
the document-block contract — are unchanged by this and are documented in
[FEATURE-NOTES.md](FEATURE-NOTES.md) and the pack's `templates/NOTICE.md`.

## The problem

A deck is the right deliverable for a reader who wants to *look at* slides: one
self-contained `.bento.html` that is its own viewer and editor. It is the wrong
deliverable for "email this to the board". Before this change the agent could
only tell the user to open the deck and click the printer icon, which broke the
flow whenever the PDF was the actual request — the agent could not attach a file
it could not produce.

## Why not the app's own export

The obvious move is to reuse Bento's `Export PDF (print)`. You cannot: that
function builds a DOM (one `.bp-page` per slide, through the same static
renderer used for thumbnails) and calls `window.print()`. The export *is* the
browser — layout, font metrics and the PDF writer all come from Chromium, and
nothing in the deck file caches a rendered slide.

Reusing it therefore means putting a browser in the sandbox. That was priced and
rejected:

- **~400MB** added to an image built on `fedora-minimal`.
- **The Grype gate.** `scripts/check-grype-policy.sh` fails CI on any fixable
  CRITICAL Fedora RPM in the sandbox image. Chromium is the most CVE-heavy RPM in
  any distro, so this would become a recurring gate that blocks every merge in
  the repository, not just Bento work.
- **A driver.** `--print-to-pdf` prints the page, not the app's print DOM, so
  driving the real export needs CDP. There is no Node in the sandbox, so that
  means a hand-rolled WebSocket/CDP client — more moving parts than the renderer
  it would replace.
- **A browser in the sandbox** is a large new parser and JIT surface in the one
  place where model-authored code runs.

So the export is a second, deliberately small renderer for the **static** form
of a document, and the app's export stays the authority. The skill says so, and
points at the button for the cases below where it matters.

## Why "static" is not the compromise it sounds like

A PDF page cannot show motion, and neither does the app's own PDF: `exportPdf`
renders each slide through the static renderer with `svgAsImage: true` and
placeholders hidden, then prints. Morph, `countUp`, entrances, ken-burns and
motion paths are absent from **both** exports. "Static" is what a Bento PDF is,
not something this renderer invented.

## What it reproduces

Ported from the vendored runtime rather than re-imagined, so the geometry
matches:

- **Page selection** — `!slide.stateOf && !slide.hidden`, the app's own filter,
  so `{{page}}`/`{{pages}}` agree between the two exports.
- **Page size** — 960x540pt (13.333in x 7.5in), the standard 16:9 slide page, so
  a reader sees a normal slide document and printing gives one landscape sheet
  per slide. A deck with a different `size` keeps its aspect ratio.
- **The element box model** — x/y/w/h, rotation about the centre, opacity
  compounded with per-colour alpha.
- **Text** — the CSS line box (half-leading, `lineHeight`, `valign`, `align`),
  greedy wrap with `break-word` for over-long words, inline `<b>/<i>/<code>`,
  `letterSpacing`, HTML entities, and `colorGradient` painted *through* the
  glyphs with a text clip.
- **Shapes** — rect (with radius), ellipse, triangle, arrow, line with the app's
  dash patterns and arrow/dot/bar tips, and `path` with `pathBox` mapped onto
  the element box (points are transformed, so stroke width stays uniform the way
  `vector-effect: non-scaling-stroke` does).
- **Gradients** — the app's `pd(angle)` convention, including per-stop **alpha**
  via a luminosity soft mask. That is what makes the documented hero-image
  recipe (photo + transparent-to-dark scrim) come out as a fade instead of a
  black block.
- **Images** — PNG (grey/RGB/palette/alpha, 8- and 16-bit, `tRNS`) decoded and
  re-deflated with alpha as an `/SMask`; JPEG passed through untouched as
  `/DCTDecode`. `cover`/`contain`/`fill` and corner radius via clipping.
- **Tables** — fractional column weights, header, zebra, per-cell align/colour/
  background/bold, padding, borders, radius, and the row-height distribution a
  browser does for a `height: 100%` table.
- **Charts** — the charts-lite engine ported function for function: default
  palette, grid insets, the tick algorithm, number formatting, bar/line/pie/
  scatter, dual axes, `smooth`, `areaStyle`, symbols, pie labels with leader
  lines and `{b}/{c}/{d}`, and the legend.
- **Dynamic fields** — `{{page}}`, `{{pages}}` (with zero-pad), `{{title}}`,
  `{{date}}`, `{{time}}`, and the `meta` properties.

## What it does not reproduce

Each of these prints a `note:` line when a deck actually hits it, so the agent
can pass it on rather than hand over a page that differs from the deck:

- **Fonts are the PDF core 14** (Helvetica/Times/Courier, mapped from the CSS
  stack). Text metrics are the real Adobe AFM widths for the face embedded, so
  wrapping is exact *for the PDF* — but a deck that embeds a woff2 face renders
  in the mapped fallback. Embedding the deck's own face would need a brotli
  decoder and a TrueType subsetter, neither of which is in the standard library.
- **Text is WinAnsi.** Common typographic symbols are transliterated (`->` for
  an arrow); anything else becomes `?` and is counted in a warning. CJK, Greek,
  Hebrew, Arabic and emoji need the in-app export.
- **`svg` elements and KaTeX math** are skipped — no SVG renderer, no TeX
  layout.
- **blur, drop shadow, blend modes and backdrop-filter** are skipped; the shapes
  and text are drawn without them.
- **Video and audio** become the neutral poster block the app's own print path
  draws.
- **Remote image URLs** are left out: a PDF has no network, so only data: URIs
  and `doc.assets` entries can be drawn.

## Measured against the app's own export

Verified by driving the real thing: Chromium opens the deck, the app's own
*Export PDF (print)* button is clicked, and the resulting PDF is compared with
the built-in export page by page.

- **Page count and content**: identical on every deck tested — same pages, and
  every word present on both sides (140/140 words across a five-slide deck with
  charts, a table, a hero image, gradients and dynamic fields).
- **Text position**: on a deck naming a font whose metrics both sides share,
  **all 150 words landed in the same place** — mean Δx 0.03%, max 0.44% of page
  width — with every wrap point identical. Baselines agreed to within 3.3px on a
  720px canvas (the residue is Helvetica's AFM ascent versus the substituted
  font's `hhea` ascent in the half-leading calculation).
- **Charts**: visually indistinguishable across bar, dual-axis bar+line, smooth
  line with area fill, scatter with a numeric x axis, negative values with a
  pinned axis and a `{value}` formatter, donut and `label: false` pie, 18
  categories with three series, single-datum, and empty-series charts.
- **Where they differ** it is the substituted typeface, not the geometry:
  headless Chromium resolves `system-ui` to DejaVu Sans (~12% wider than
  Helvetica, taller ascent), so its lines run wider and wrap earlier. On a Mac
  or Windows reader's machine `system-ui` is closer to the metrics used here
  than that Linux fallback is.
- **Structure**: every generated PDF parses and renders clean under MuPDF — an
  independent implementation — with no warnings, and text is selectable and
  searchable (including `—` and `·`).

## Fail-closed behaviour

- The deck is only ever **read**; `pdf` writes a separate file.
- The PDF is written atomically, so a failure leaves no partial file and no temp
  file behind.
- A refusal is an error message, never a traceback: output paths are checked with
  the same rules as a deck path (relative, no `..`, link-safe characters, `.pdf`
  extension), a deck with nothing printable is refused rather than exported
  empty, and a missing output directory says so.
- A single malformed element is a `note:`, not a lost deck — the rest of the
  slide still renders.

## Keeping it honest

The maintenance cost of a second renderer is drift. Four tests in
`internal/clientconfig/builtin_skills_bento_test.go` make it loud instead of
silent:

- `TestBentoPdfAssumptionsStillHoldInTheApp` inflates the vendored runtime and
  asserts the facts the port copied are still true of the app: the
  `!stateOf && !hidden` print filter, the `#bento-print` / `.bp-page` print DOM,
  the default chart palette (in both directions — app and renderer), and the
  chart axis greys. A re-vendor that changes any of them fails CI.
- `TestBentoPdfExportMatchesTheAppsPageSelection` pins the page set, the page
  size, the selectable text, and that the deck is left byte-identical.
- `TestBentoPdfExportFailsClosed` pins every refusal path and that nothing is
  left behind.
- `TestBentoPdfRendererIsStandardLibraryOnly` allowlists the renderer's imports,
  so the "the sandbox image gains nothing" claim cannot rot into a silent new
  dependency.

## Deliberately not done

- **No `.pptx`.** Unchanged: charts, morph, count-up and the motion effects have
  no PowerPoint equivalent, and a deck that is almost right is worse than an
  honest PDF.
- **No speaker-notes page.** The notes are in the document and in the deck; a
  notes layout is a separate feature, not a silent addition to this one.
- **No mail integration.** The PDF is an ordinary workspace file; sending it is
  whatever mail or upload tool a deployment's bundle provides. fleet stays the
  engine.
- **`validate` still estimates text overflow.** The renderer now carries real
  AFM metrics, so `bento_doc.py validate` could measure text exactly instead of
  guessing — but that changes a tested advisory and is a separate change, not a
  side effect of adding an export.
- **Not upstreamed yet.** The natural long-term home for a headless Bento
  exporter is upstream Bento (MIT), where it would sit beside the renderer it
  mirrors. That is a follow-up, not a blocker.
