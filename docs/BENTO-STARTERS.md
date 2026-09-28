# Bundle-provided Bento starters

The `bento-slides` helper's `new` command loads
`skills/bento-theme/document.json` by default when present in the merged skills
tree. A bundle supplies that companion skill (including `SKILL.md`) alongside
fleet's built-in skill. This works with both sandbox backends and normal skill
refresh; no customer content or alternate app shell belongs in fleet.

The file is a complete `bento/slides` document, including embedded font and image
assets and any attribution notices. Theme data and layouts belong there, not in
generation code. The helper validates it before writing an output, refuses any
`collab` block, drops `docId`, and resets `modified`. The requested title (or a
filename-derived title) replaces `document.title` and the first slide's text
element whose id is `title`. Other content survives unchanged.

`new --starter PATH.json` explicitly selects a different document.
`new --blank` explicitly selects the generic one-title-slide default. They are
mutually exclusive. Missing optional bundle data retains the generic default;
present but malformed data fails rather than silently producing the wrong deck.
All generated content remains freely editable with `get`/`set` or Bento's editor.

The existing pinned Bento app, notices, escaping, offline CSP and upstream
offline switch are retained. Starter documents should embed every rendering
asset as data URLs: the offline protections block network resources. This is
not a new theme engine or a schema change; no new export format is included.

Scope: the Elcano ticket's branding lives in `elcano-config`, while this change
adds only the generic starter seam. The shell's own in-browser new-document
button is upstream behavior; fleet's normal authoring flow is `bento_doc.py new`.
