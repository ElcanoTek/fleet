---
name: bento-slides
description: Build a presentation the user can actually open — a Bento deck, one self-contained .bento.html file that is its own viewer and editor, authored offline with no Gamma, PowerPoint, or external API, and delivered as an offline-only file with no update check and no live collaboration, and exported to a PDF you can attach to an email without the user rendering anything. Use it whenever someone asks for a slide deck, presentation, pitch, readout, or "slides" from material you have — including when they want the slides sent, emailed, printed, or attached as a PDF — and prefer charts, tables, and morph transitions over walls of bullet text.
---

# Bento decks

A Bento deck is ONE self-contained HTML file that contains the slides, the
viewer, and a full editor. The user downloads it and opens it in any browser —
nothing to install, no account, no sign-in, and nothing fetched to render the
deck. This skill bundles the Bento app and a helper that edits the deck for you.

A deck you create is **offline-only**: no update check, no live collaboration, no
network of any kind. That is not upstream's default — see "An offline deck, not a
network client" below for what `new` does and why.

Never edit a `.bento.html` by hand. The file is a 689KB minified app around one
JSON document block; `view_file` on it would burn your context on runtime code,
and the document has escaping rules that are easy to break invisibly. Use the
helper for every read and write.

## Step 1 — start the deck

```bash
python3 skills/bento-slides/scripts/bento_doc.py new Q4_Review.bento.html
```

Create it **in the workspace root, not a subdirectory**, and name it after the
topic using only letters, digits, `.`, `_` and `-` — a space or a `#` in the name
breaks the download link even though the file exists. `new` refuses anything else.

It arrives with one title slide, so it is already a valid deck you can open. The
bundled app is read-only — `new` copies it, so never try to write into
`skills/bento-slides/`.

## Step 2 — read the format reference

Before authoring anything beyond text, read
`skills/bento-slides/references/authoring.md`. It is the full document schema:
every element type's required fields, and copy-paste recipes for charts, morph
transitions, state slides, ken-burns and motion. Skipping it is how you end up
with a deck of paragraphs, or elements that silently do not render because a
required field was missing.

## Step 3 — author the document

```bash
python3 skills/bento-slides/scripts/bento_doc.py get Q4_Review.bento.html -o doc.json
```

Edit `doc.json` with `edit_file` — it is a small ordinary JSON file. Then map
each piece of source material to the feature that fits it, rather than
defaulting to bullets:

- numbers to compare (trend, magnitude, share) → a **chart** element
- a comparison, spec, or pricing grid → a **table** element
- consecutive slides about the same thing changing → **morph**: give the shared
  elements the same `id` on both slides and set `transition: "morph"` on the
  later one. This is Bento's signature move; reach for it often.
- a point to drill into → a **state slide** (`stateOf` + an element `link`)
- a headline number → big text with `fx: {countUp: true}`
- a hero image → full-bleed image plus a scrim rectangle behind the text

Respect the canvas: 1280x720, one accent colour, at most two typefaces, 96px
side margins (so the right-most `x + w` stays at or below 1184). Write real
speaker notes on every slide. Keep every slide's text **plain text** — Bento
allows inline `<b>`, `<i>` and `<br>`, and nothing else belongs there. Never
put a `<script>`, an iframe, or a remote URL in slide content: the user opens
this file locally, and active content you author would run on their machine.

Write it back and check it:

```bash
python3 skills/bento-slides/scripts/bento_doc.py set Q4_Review.bento.html doc.json
python3 skills/bento-slides/scripts/bento_doc.py validate Q4_Review.bento.html
```

`set` refuses to write anything that would not open, and leaves the previous
version in place when it refuses, so a failure is safe — read the error and fix
`doc.json`. `validate` also prints advisories — missing speaker notes, elements
past the margin, and text that looks too tall for its box — and they are worth
acting on. **Take the overflow one seriously:** you cannot see your deck, and a
heading that wraps one line further than you expected sits on top of whatever is
below it. The check is an estimate (there are no font metrics here), so give
headings room rather than tuning them to the pixel — a box 30% taller than the
text needs costs nothing, and a collision ruins the slide.

## Step 4 — hand it to the user

`new`, `set` and `validate` each print the exact markdown link for the deck.
**Copy that line verbatim** — do not rebuild the link yourself:

```
download link (use this EXACT text, do not rebuild it): [Q4_Review.bento.html](Q4_Review.bento.html)
```

so your reply reads:

```
Your deck is ready: [Q4_Review.bento.html](Q4_Review.bento.html)
```

The link must match the deck's real path relative to the workspace. Linking a
name that does not match is the one failure the user sees as a broken download
("file wasn't available on site") on a deck that was written perfectly.

Tell them to **download it and open it in a browser** — it will not preview in
the chat, by design, and there is no server-side render step. Opened locally it
boots straight into the editor with the finished deck, so they can keep editing
it themselves.

Never paste the deck's HTML into your reply. It is 689KB of runtime and it tells
the user nothing.

## Step 5 — export a PDF when it has to be sent

The deck is what a reader opens and edits. A **PDF** is what gets emailed,
printed, filed, or attached to a ticket — and you can produce it yourself, in
the same turn, without the user exporting anything:

```bash
python3 skills/bento-slides/scripts/bento_doc.py pdf Q4_Review.bento.html
```

That writes `Q4_Review.pdf` beside the deck (`-o` overrides the path), one page
per slide at 960x540pt — the standard 16:9 slide page, so it prints one landscape
sheet per slide. Hidden and state slides are left out, exactly as the app's own
export leaves them out, so page numbers agree between the two. The deck itself is
never touched.

**Do it whenever the PDF is the point**: "email this to the team", "attach it to
the ticket", "put it in the folder", "I need something to print". The PDF is an
ordinary workspace file, so anything that takes a file path can take it — a mail
tool's attachment, a file upload, a datastore write. Hand over both links when
the user may want to keep editing:

```
Deck: [Q4_Review.bento.html](Q4_Review.bento.html) · PDF: [Q4_Review.pdf](Q4_Review.pdf)
```

If you have no way to send mail, say so and give them the PDF link rather than
claiming it was sent.

**Read the notes it prints and pass on the ones that matter.** The export is a
static renderer, not a browser, and it prints a `note:` line for anything in the
deck it could not reproduce. What it does faithfully: the element box model,
text wrapping and alignment, tables, shapes, gradients (including transparent
scrims), PNG/JPEG images, and charts. What it cannot:

- **Fonts are the PDF core 14** (Helvetica/Times/Courier, chosen from the CSS
  stack). A deck naming a system stack looks right; a deck that *embeds* a woff2
  face renders in the mapped fallback, so the shape is right and the typeface is
  not.
- **Text is Western European (WinAnsi).** Common symbols are transliterated;
  CJK, Greek, Hebrew, Arabic and emoji come out as `?` and are counted in a
  warning. A deck in those scripts needs the in-app export.
- `svg` elements, KaTeX math, blur, drop shadow and blend modes are skipped, and
  video/audio become the same neutral poster block the app's own print path
  draws. Animation is not a limitation: morph, count-up, entrances and
  ken-burns have no meaning on a static page, and the app's export drops them
  too.

**The deck's own export stays the authority.** When a page has to be
pixel-exact — an embedded typeface, a slide with `svg` artwork, a non-Latin
script — point at the button instead, in one line:

> For a pixel-exact PDF: open the deck and click the printer icon in the toolbar
> (*Export PDF (print)*), then choose "Save as PDF" — one page per slide.

Never describe the built-in export as the deck's own renderer, and never claim a
PDF you did not write. If `pdf` fails it says why and writes nothing — read the
error, fix the document, and run it again.

A PDF is a **revision** like the deck is: give the new one a new filename
(`Q4_Review_v2.pdf`) for the same 24-hour cache reason below.

There is **no PowerPoint export**, and there is no way for you to make one from a
deck. If the user needs a `.pptx` specifically, say so plainly and offer the PDF
instead of implying a conversion exists. Do not try to build a `.pptx` by hand
from the document: charts, morph, count-up and the motion effects have no
PowerPoint equivalent, and a deck that is almost right is worse than an honest
PDF.

**Revising a deck: always deliver the revision under a NEW filename**
(`Q4_Review_v2.bento.html`). Workspace downloads are cached for 24 hours, so
re-using the filename serves the user their old file and makes your fix look
like it did nothing.

## An offline deck, not a network client

fleet ships Bento as a presentation tool, not a client for anything. Upstream has
two behaviors that would make a delivered deck reach the network, and `new`
disables both:

- an **update check** on every launch (`fetch` to `bento.page`) — fleet embeds and
  pins the app, so it can only report a version your reader cannot install, while
  telling a third party that they opened the deck;
- **live collaboration.** This is the one that matters. Merely *carrying* a
  `collab` block makes a deck share-eligible, so such a deck opens a
  `wss://sync.bento.page` session the moment it is opened — no click, and it keeps
  retrying. A file like that is a live door into whoever opens it.

`new` plants two layers ahead of the app runtime, and you should understand why
there are two:

1. A **CSP `<meta>` tag** with `connect-src 'none'`. The *browser* enforces this,
   so no script in the page can open a socket or a fetch — not upstream's, not
   one you wrote into a slide. It also blocks iframes, plugins, form posts and
   remote images, which turns several rules below into rules the browser keeps
   rather than rules you must remember.
2. **Upstream's own offline mode**, so the app refuses network at its own
   chokepoints and never attaches a session, failing cleanly instead of retrying
   into the CSP. This one needs `localStorage`; layer 1 does not, which is exactly
   why layer 1 exists.

Never remove either, and never add active content of your own beside them: those
elements are reviewed, tested code, unlike anything you would write into a slide.

**A deck the user gave you keeps upstream's shell.** `set` preserves a shell byte
for byte, so it never plants the guard in someone else's file. `validate` says so
when it sees an unguarded shell — pass that on rather than editing their file. If
they want it locked down, make a fresh deck with `new` and move the document
across.

## If the deck arrives shared

`get` warns you when a deck carries a live-collaboration block, and keeps any
private keys out of your context. Understand what that deck is: opening it joins
a live session with no click, and anyone holding a copy can join and write.

**`set` removes the session block.** A deck this skill writes is offline-only, so
the revision you hand back edits locally and nothing else. Tell the user you did
it, and tell them the part that is easy to miss: **removing the keys does not
retract an invitation already shared.** Anyone who already has an earlier copy can
still join that room. The only remedy for that is *Share -> Rotate keys* in the
app, and only they can decide to do it.

Two other things the helper protects, so do not work around them: never invent
or change a deck's `docId` (it is the document's identity), and never
hand-escape the document block yourself.

## Editing a deck the user gave you

Same loop, starting at `get`. Read the existing document before changing it,
keep the element `id`s you find (morph, links and states all key off them), and
change only what was asked.

---

The bundled `templates/Bento_Slides.bento.html` is [Bento](https://bento.page)
(<https://github.com/nyblnet/bento>), MIT-licensed, © 2026 The Bento authors,
redistributed unmodified. See `templates/NOTICE.md` for the full provenance and
the notices for the components it bundles.
