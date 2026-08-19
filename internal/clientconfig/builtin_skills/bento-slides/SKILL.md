---
name: bento-slides
description: Build a presentation the user can actually open — a Bento deck, one self-contained .bento.html file that is its own viewer and editor, authored offline with no Gamma, PowerPoint, or external API. Use it whenever someone asks for a slide deck, presentation, pitch, readout, or "slides" from material you have, and prefer charts, tables, and morph transitions over walls of bullet text.
---

# Bento decks

A Bento deck is ONE self-contained HTML file that contains the slides, the
viewer, and a full editor. The user downloads it and opens it in any browser —
nothing to install, no account, no sign-in, and nothing fetched to render the
deck. This skill bundles the Bento app and a helper that edits the deck for you.

Upstream's app asks `bento.page` for a newer version of itself every time a deck
is opened. `new` disables that, so a deck you create makes no network request at
all — see "A deck you make does not call home" below.

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
`doc.json`. `validate` also prints advisories (missing speaker notes, elements
past the margin) that are worth acting on.

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

**Revising a deck: always deliver the revision under a NEW filename**
(`Q4_Review_v2.bento.html`). Workspace downloads are cached for 24 hours, so
re-using the filename serves the user their old file and makes your fix look
like it did nothing.

## A deck you make does not call home

`new` plants one small `<script id="fleet-no-update-check">` into the shell,
ahead of the app runtime. Upstream checks `bento.page` for a newer app version on
launch; fleet ships the app embedded and pinned, so that answer is unusable here
and the question alone would tell a third party that your reader opened the deck.
The guard refuses it and switches the app's own preference off, so the About panel
shows the true state. Live collaboration (a WebSocket, only for a deck the user
shares) is untouched.

Do not remove it, and do not add active content of your own next to it — that
element is reviewed, tested code, unlike anything you would write into a slide.

**A deck the user gave you keeps upstream behavior.** `set` preserves a shell
byte for byte, so it never plants the guard in someone else's file. `validate`
prints a note when it sees an unguarded shell — pass that on to the user rather
than editing their file. If they want a guarded copy, build a new deck with `new`
and move the document across.

## If the deck is already shared

`get` warns you when a deck carries live-collaboration credentials. That deck is
a live invitation: whoever holds the file can join the session and write to it.
Stop and tell the user before you go further — only they can decide, and they
may not know it is shared. The helper keeps those keys out of your context and
leaves them untouched in the file; deleting them after the fact does not
retract them, so the real remedy is *Share -> Rotate keys* in the app.

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
