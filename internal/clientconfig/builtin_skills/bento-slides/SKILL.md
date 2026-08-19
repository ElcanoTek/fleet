---
name: bento-slides
description: Build a presentation the user can actually open — a Bento deck, one self-contained .bento.html file that is its own viewer and editor, authored offline with no Gamma, PowerPoint, or external API, and delivered as an offline-only file with no update check and no live collaboration. Use it whenever someone asks for a slide deck, presentation, pitch, readout, or "slides" from material you have, and prefer charts, tables, and morph transitions over walls of bullet text.
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
