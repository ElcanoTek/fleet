#!/usr/bin/env python3
"""Create and edit Bento decks: single-file .bento.html presentations.

A deck is one self-contained HTML file. The fixed app shell (viewer + editor +
runtime) never changes; the slides live in ONE JSON block near the top:

    <script type="application/bento+json" id="bento-doc">{...}</script>

This helper is the only thing that should ever touch that block. It exists so
the deck JSON can be edited as a small ordinary file instead of by splicing a
689KB minified bundle by hand, and so two invariants hold mechanically rather
than by remembering them:

  * Every "<" inside the block is stored escaped as \\u003c, so the JSON can
    never contain a literal </script> that would terminate the block early.
  * docId is the document's identity and collab carries live-session private
    keys. `get` never emits the keys; `set` never regenerates the identity.

Standard library only. Every byte outside the document block is preserved
exactly, and every failure leaves the target file untouched.

Usage:
    python3 bento_doc.py new      DECK.bento.html [--title TITLE]
    python3 bento_doc.py get      DECK.bento.html [-o DOC.json]
    python3 bento_doc.py set      DECK.bento.html DOC.json
    python3 bento_doc.py validate DECK.bento.html | DOC.json
"""

import argparse
import json
import os
import shutil
import sys
import tempfile

# The document block's opening tag. It appears exactly once in a well-formed
# deck; everything before it and everything from the matching </script> onward
# is the shell and is copied through byte for byte.
OPEN_TAG = b'<script type="application/bento+json" id="bento-doc">'
CLOSE_TAG = b"</script>"

# The shell's <title>, which the app itself keeps in sync with doc.title on
# save. Syncing it here means a freshly delivered file has the right browser-tab
# name before it has ever been opened.
TITLE_OPEN = b"<title>"
TITLE_CLOSE = b"</title>"

# ── the offline-deck guard ───────────────────────────────────────────────────
#
# fleet ships Bento as a strictly OFFLINE viewer/editor. A deck is a document the
# user opens on their own machine; it is not a client for anything. Two upstream
# behaviors would make it one, and `new` plants the guard below to stop both:
#
#   * A launch update check (`fetch` to bento.page). fleet embeds and sha256-pins
#     the shell, so it can only report a version the reader cannot install, while
#     telling a third party that they opened the deck.
#   * Live collaboration. `bornWithCollab = !!doc.collab` is enough to make a deck
#     share-eligible, so a deck that merely CARRIES a collab object opens a
#     `wss://sync.bento.page` session on load — no click, and it retries. Hand
#     someone such a file and opening it streams their edits to whoever holds the
#     room key.
#
# The guard is two independent layers, because one of them is only as good as the
# browser's localStorage:
#
#   1. A CSP meta tag. `connect-src 'none'` is enforced by the BROWSER, so no
#      script in the page — upstream's, ours, or anything a model wrote into a
#      slide — can open a socket or a fetch. The other directives turn several of
#      SKILL.md's "never author this" rules into rules the browser keeps:
#      no plugins, no iframes, no form posts, no <base> hijack, no remote images
#      (a remote image is a beacon that reports who opened the deck and when).
#      `script-src 'unsafe-inline'` is unavoidable: the app IS inline script.
#   2. Upstream's own offline switch. With `bento-offline=on` the app refuses
#      network at its own chokepoints (`fi` for fetch, `Ry` for WebSocket), never
#      attaches a collab transport, and strips remote asset URLs at render. That
#      makes it fail COHERENTLY — its own error type, its own UI — instead of
#      retrying into a CSP wall.
#
# Layer 3 is not in this file at all: `set` refuses to write a `collab` block, so
# a deck fleet produces has nothing for a session to attach to in the first place.
GUARD_ID = "fleet-offline-deck"
GUARD = b"""<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'">
    <script id="fleet-offline-deck">
/* Added by fleet's bento-slides skill. NOT part of upstream Bento.
   fleet ships Bento as an offline viewer/editor: a deck is a document, not a
   network client. The CSP above is the boundary the browser enforces; this turns
   on upstream's own offline mode so the app refuses network at its own
   chokepoints and never joins a live session, rather than retrying into the CSP.
   To restore upstream behavior, delete this element AND the meta tag above it. */
(function () {
  try {
    localStorage.setItem("bento-offline", "on");
    localStorage.setItem("bento-auto-check", "off");
  } catch (e) {
    /* No localStorage (some browsers refuse it for file:// URLs). The CSP above
       still holds - it does not depend on storage, or on this script running. */
  }
})();
</script>
    """


def _inject_guard(raw):
    """Plant the guard ahead of the document block, exactly once.

    The guard goes in the prefix, which every later `set` copies through
    unchanged, so it survives editing without ever being added twice.
    """
    if GUARD_ID.encode() in raw:
        return raw
    at = raw.index(OPEN_TAG)
    return raw[:at] + GUARD + raw[at:]


def has_guard(raw):
    return GUARD_ID.encode() in raw


# The vendored app shell, resolved relative to this script so it works from any
# working directory. The skills tree is bind-mounted read-only, so this is a
# read-only source: it is copied, never edited.
TEMPLATE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    os.pardir,
    "templates",
    "Bento_Slides.bento.html",
)

# Names of the collab fields whose VALUES are credentials (private keys and an
# invite token) rather than metadata. Their presence means the file is a
# live-session invitation, so `get` refuses to put them in front of a model and
# tells the caller to raise it with the user instead.
#
# This tuple, and everything derived from it below, holds field NAMES only. No
# code path in this script reads a value out of collab -- not to print, not to
# log, not to return. Keep it that way: the names are what an operator needs to
# act on, and a value here would be a cleartext credential leak.
COLLAB_CREDENTIAL_FIELDS = ("ownerPriv", "writerPriv", "invite")

# Slide-space defaults from the format spec: a 16:9 canvas in pixels.
DEFAULT_SIZE = {"width": 1280, "height": 720}
DEFAULT_THEME = {
    "background": "#101418",
    "color": "#F2F0EA",
    "accent": "#FF9E8A",
    "fontFamily": "system-ui, sans-serif",
}

TRANSITIONS = ("none", "fade", "slide", "zoom", "morph")


class DeckError(Exception):
    """A refusal. The message is written to stderr and the target is untouched."""


# Characters that are safe in a filename the user will be handed as a markdown
# link. Spaces are tolerated by the chat client's href resolver, but "#", "(",
# ")" and "?" break markdown link parsing or truncate the href at a fragment,
# so a deck named with one is created fine and then 404s on download.
SAFE_NAME = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"


def _relative_parts(path):
    """Split a path into its meaningful segments, "/"-style.

    Drops empty and "." segments so "./decks//Q4.bento.html" and
    "decks/Q4.bento.html" agree. Keeps ".." so a caller can reject it.
    """
    return [seg for seg in path.replace(os.sep, "/").split("/") if seg not in ("", ".")]


def check_deck_path(path):
    """Validate a path `new` is about to create, and return its segments.

    EVERY segment is checked, not just the filename. Two reasons:

      * ".." would walk out of the workspace. The deck is meant to land where
        the user can download it; a path that escapes writes somewhere they
        cannot reach, over something they did not name.
      * a directory containing a space, "#", "(" or "?" breaks the markdown
        download link just as surely as a filename does, and the link is built
        from the whole relative path. Checking only the basename left that hole.
    """
    if os.path.isabs(path):
        raise DeckError(
            "%s is an absolute path; a deck must be created at a path relative "
            "to the workspace, or the user will not be able to download it "
            "(the chat client only rewrites relative links)." % path
        )
    parts = _relative_parts(path)
    if not parts:
        raise DeckError("no deck path given")
    if ".." in parts:
        raise DeckError(
            "%r walks outside the workspace with '..'. Create the deck in the "
            "workspace (ideally its root) — a file written outside it is one "
            "the user cannot download." % path
        )
    for seg in parts:
        bad = sorted({c for c in seg if c not in SAFE_NAME})
        if bad:
            raise DeckError(
                "%r contains %s, which break the markdown download link (the "
                "href gets truncated or misparsed, so the deck 404s even though "
                "the file exists). Use only letters, digits, '.', '_' and '-' in "
                "every part of the path — e.g. %r."
                % (seg, " ".join(repr(c) for c in bad), "Q4_Review.bento.html")
            )
    if not parts[-1].endswith(".bento.html"):
        raise DeckError(
            "%r should end in '.bento.html' so it is recognizable as a Bento "
            "deck." % parts[-1]
        )
    return parts


def download_link(path):
    """The exact markdown link that resolves to this deck.

    The chat client rewrites a RELATIVE href to the conversation's workspace
    file API, so the link text must be the deck's path exactly as passed here
    (relative to the workspace, which is the cwd) — not its basename. Linking a
    bare filename for a deck written into a subdirectory is the one mistake that
    produces a deck the user cannot download, so the helper prints the answer
    rather than leaving it to be reconstructed.
    """
    parts = _relative_parts(path)
    if not parts:
        return "[%s](%s)" % (path, path)
    # lstrip("./") would eat leading dots of a name like ".hidden.bento.html";
    # splitting into segments cannot.
    rel = "/".join(parts)
    return "[%s](%s)" % (parts[-1], rel)


# ── the document block ────────────────────────────────────────────────────────


def _split(raw):
    """Split deck bytes into (prefix, block, suffix).

    prefix ends with the opening tag and suffix starts with </script>, so
    prefix + new_block + suffix rebuilds the file with the shell intact.
    """
    count = raw.count(OPEN_TAG)
    if count == 0:
        raise DeckError(
            "no Bento document block found — this is not a .bento.html deck "
            "(expected one %s)" % OPEN_TAG.decode()
        )
    if count > 1:
        raise DeckError(
            "found %d document blocks; a deck must have exactly one. Refusing "
            "to guess which one is the document." % count
        )
    start = raw.index(OPEN_TAG) + len(OPEN_TAG)
    end = raw.find(CLOSE_TAG, start)
    if end < 0:
        raise DeckError("document block is not closed; the file is truncated")
    return raw[:start], raw[start:end], raw[end:]


def _inject_guard(raw):
    """Plant the guard ahead of the document block, exactly once.

    The guard goes in the prefix, which every later `set` copies through
    unchanged, so it survives editing without ever being added twice.
    """
    if GUARD_ID.encode() in raw:
        return raw
    at = raw.index(OPEN_TAG)
    return raw[:at] + GUARD + raw[at:]


def has_guard(raw):
    return GUARD_ID.encode() in raw


def _decode_block(block):
    """Parse a document block's bytes into a dict.

    json.loads handles the \\u003c escaping transparently, so this is where the
    escaping stops mattering to the caller.
    """
    text = block.decode("utf-8").strip()
    if not text:
        raise DeckError(
            "the document block is empty — this is a bare app shell, not a "
            "deck. Run `bento_doc.py new` to start one."
        )
    try:
        doc = json.loads(text)
    except ValueError as exc:
        raise DeckError("document block is not valid JSON: %s" % exc) from exc
    if not isinstance(doc, dict):
        raise DeckError("document block must be a JSON object")
    if doc.get("format") == "bento/enc":
        raise DeckError(
            "this deck is password-encrypted (bento/enc envelope); it must be "
            "decrypted in the browser before a tool can edit it"
        )
    return doc


def _encode_block(doc):
    """Serialize a document for the block, escaping every "<".

    The escape is what guarantees the block cannot contain a literal </script>.
    It is applied to the serialized JSON, so it covers "<" wherever it appears
    — including inside slide text HTML.
    """
    payload = json.dumps(doc, ensure_ascii=False, separators=(",", ":"))
    payload = payload.replace("<", "\\u003c")
    encoded = payload.encode("utf-8")
    if CLOSE_TAG in encoded:
        # Unreachable while the replace above stands; kept as a hard stop so a
        # future edit to this function cannot silently corrupt a file.
        raise DeckError("internal error: escaped payload still contains </script>")
    return encoded



# ── text fit (an estimate, because we have no font metrics) ──────────────────
#
# The app measures text for real and reports `text-overflow` from
# `window.bento.validate()`. An agent composing a deck in a chat turn has no
# browser, so that check is unavailable exactly when it would be most useful:
# a heading that wraps to one more line than expected collides with whatever is
# under it, and nothing in the JSON shows it.
#
# This is a deliberately rough greedy wrap. It cannot be exact without the
# font, so it is an ADVISORY and it errs toward silence: the advance ratio is
# calibrated against real Chromium measurements of the bundled system stack, and
# a warning needs to clear the box by a margin before it prints. The app's own
# validate() stays authoritative.
_AVG_ADVANCE = 0.55  # mean glyph advance as a fraction of font size, sans-serif
_FIT_SLACK = 1.05    # only complain when the estimate clears the box by 5%

_ENTITIES = (
    ("&mdash;", "-"), ("&ndash;", "-"), ("&nbsp;", " "), ("&amp;", "&"),
    ("&lt;", "<"), ("&gt;", ">"), ("&quot;", '"'), ("&#39;", "'"),
)


def _plain_lines(html):
    """Split element html into hard lines, with tags and entities removed."""
    text = html.replace("<br/>", "\n").replace("<br />", "\n").replace("<br>", "\n")
    out = []
    depth = 0
    for ch in text:
        if ch == "<":
            depth += 1
        elif ch == ">":
            depth = max(0, depth - 1)
        elif depth == 0:
            out.append(ch)
    text = "".join(out)
    for entity, plain in _ENTITIES:
        text = text.replace(entity, plain)
    return text.split("\n")


def estimate_text_height(el):
    """Estimated rendered height in px, or None if the element is not sizable."""
    size = el.get("fontSize")
    width = el.get("w")
    if not isinstance(size, (int, float)) or not isinstance(width, (int, float)):
        return None
    if size <= 0 or width <= 0:
        return None
    line_height = el.get("lineHeight")
    if not isinstance(line_height, (int, float)) or line_height <= 0:
        line_height = 1.2
    per_line = max(1, int(width / (size * _AVG_ADVANCE)))

    lines = 0
    for hard in _plain_lines(str(el.get("html", ""))):
        words = hard.split()
        if not words:
            lines += 1
            continue
        used = 0
        count = 1
        for word in words:
            need = len(word) if used == 0 else len(word) + 1
            if used + need <= per_line:
                used += need
            else:
                count += 1
                used = len(word)
        lines += count
    return lines * size * line_height


# ── validation ───────────────────────────────────────────────────────────────


def _require(doc, key, kind, where):
    if key not in doc:
        raise DeckError("%s: missing required field %r" % (where, key))
    if not isinstance(doc[key], kind):
        raise DeckError(
            "%s: field %r has the wrong type (%s)" % (where, key, type(doc[key]).__name__)
        )
    return doc[key]


def validate_doc(doc):
    """Check the format invariants the app needs to boot. Raises DeckError."""
    fmt = _require(doc, "format", str, "document")
    if fmt != "bento/slides":
        raise DeckError("document format is %r, expected 'bento/slides'" % fmt)
    _require(doc, "version", int, "document")
    _require(doc, "title", str, "document")

    # size and theme are required — the app will not boot without them, and
    # theme.fontFamily in particular is easy to leave out.
    size = _require(doc, "size", dict, "document")
    for axis in ("width", "height"):
        if not isinstance(size.get(axis), (int, float)):
            raise DeckError("document: size.%s must be a number" % axis)
    theme = _require(doc, "theme", dict, "document")
    for field in ("background", "color", "accent", "fontFamily"):
        if not isinstance(theme.get(field), str) or not theme[field].strip():
            raise DeckError("document: theme.%s is required" % field)

    slides = _require(doc, "slides", list, "document")
    if not slides:
        raise DeckError("document: slides must not be empty")

    seen = set()
    for i, slide in enumerate(slides):
        where = "slide %d" % (i + 1)
        if not isinstance(slide, dict):
            raise DeckError("%s: must be an object" % where)
        sid = _require(slide, "id", str, where)
        if sid in seen:
            raise DeckError(
                "%s: duplicate slide id %r — ids are identity (morph, states "
                "and links all key off them)" % (where, sid)
            )
        seen.add(sid)
        _require(slide, "background", str, where)
        _require(slide, "notes", str, where)
        transition = _require(slide, "transition", str, where)
        if transition not in TRANSITIONS:
            raise DeckError(
                "%s: transition %r is not one of %s"
                % (where, transition, ", ".join(TRANSITIONS))
            )
        elements = _require(slide, "elements", list, where)
        for j, el in enumerate(elements):
            if not isinstance(el, dict):
                raise DeckError("%s element %d: must be an object" % (where, j + 1))
            for field in ("id", "type"):
                if not isinstance(el.get(field), str) or not el[field].strip():
                    raise DeckError(
                        "%s element %d: %r is required" % (where, j + 1, field)
                    )
    return doc


def collab_field_label(names):
    """Render collab field names for an operator warning.

    Takes the output of collab_credential_fields() -- names -- and prefixes each
    with "collab." so the reader knows where to look in the file. Nothing here
    touches the values those names point at.
    """
    return ", ".join("collab." + name for name in names)


def collab_credential_fields(doc):
    """Return the NAMES of the credential-bearing collab fields present in doc.

    Names only, and only ones drawn from the COLLAB_CREDENTIAL_FIELDS constant:
    the value behind each is a private key or an invite token, and every caller
    of this prints its result for the operator to read.
    """
    collab = doc.get("collab")
    if not isinstance(collab, dict):
        return []
    return [name for name in COLLAB_CREDENTIAL_FIELDS if collab.get(name)]


# ── file I/O ─────────────────────────────────────────────────────────────────


def _read(path):
    try:
        with open(path, "rb") as fh:
            return fh.read()
    except FileNotFoundError:
        raise DeckError("no such file: %s" % path) from None
    except IsADirectoryError:
        raise DeckError("%s is a directory" % path) from None


def _write_atomic(path, data):
    """Replace path's contents atomically.

    The temp file MUST live in the target's own directory: the workspace is a
    bind mount and /tmp is a separate tmpfs, so os.replace() across them fails
    with EXDEV. Writing beside the target also means a failure anywhere above
    leaves the original file exactly as it was.
    """
    directory = os.path.dirname(os.path.abspath(path))
    fh = tempfile.NamedTemporaryFile(
        dir=directory, prefix=".bento-", suffix=".tmp", delete=False
    )
    tmp = fh.name
    try:
        with fh:
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def _sync_title(prefix, title):
    """Point the shell's <title> at the deck title, as the app does on save."""
    start = prefix.find(TITLE_OPEN)
    if start < 0:
        return prefix
    end = prefix.find(TITLE_CLOSE, start)
    if end < 0:
        return prefix
    safe = title.replace("<", "").replace(">", "").strip() or "bento/slides"
    return prefix[: start + len(TITLE_OPEN)] + safe.encode("utf-8") + prefix[end:]


def _splice(path, raw, doc):
    """Write doc into path's document block, preserving the shell byte for byte."""
    prefix, _, suffix = _split(raw)
    prefix = _sync_title(prefix, doc.get("title", ""))
    _write_atomic(path, prefix + _encode_block(doc) + suffix)

    # Read back and re-parse. The write is atomic, so if this fails the file on
    # disk is still the previous good version — fail loudly rather than report
    # success on a deck nobody can open.
    verify = _decode_block(_split(_read(path))[1])
    validate_doc(verify)


# ── subcommands ──────────────────────────────────────────────────────────────


def cmd_new(args):
    path = args.deck
    check_deck_path(path)
    if os.path.exists(path):
        raise DeckError(
            "%s already exists; refusing to overwrite it. Pick another name "
            "(and note that re-delivering a revision under a NEW filename is "
            "required anyway — workspace downloads are cached for 24h)." % path
        )
    parent = os.path.dirname(os.path.abspath(path))
    if parent and not os.path.isdir(parent):
        os.makedirs(parent, exist_ok=True)

    if not os.path.isfile(TEMPLATE):
        raise DeckError(
            "the bundled Bento template is missing (looked for %s). The skills "
            "tree may not be mounted in this run." % TEMPLATE
        )
    shutil.copyfile(TEMPLATE, path)

    # Verify the copy before building on it: the source is a 689KB file on a
    # read-only bind mount that is re-materialized on the host, so a truncated
    # or missing copy is worth catching here rather than three steps later.
    raw = _read(path)
    if raw.count(OPEN_TAG) != 1:
        os.unlink(path)
        raise DeckError(
            "the copied template does not contain exactly one document block; "
            "the copy or the bundled template is corrupt"
        )

    # Plant the no-update-check guard before the document is spliced in, so the
    # deck never has a state in which it would call home.
    raw = _inject_guard(raw)

    title = args.title or os.path.basename(path).split(".")[0].replace("_", " ")
    doc = {
        "format": "bento/slides",
        "version": 1,
        "title": title,
        "size": dict(DEFAULT_SIZE),
        "theme": dict(DEFAULT_THEME),
        "slides": [
            {
                "id": "s1",
                "background": DEFAULT_THEME["background"],
                "transition": "none",
                "notes": "Speaker notes for the title slide.",
                "elements": [
                    {
                        "id": "title",
                        "type": "text",
                        "x": 96,
                        "y": 260,
                        "w": 1088,
                        "h": 160,
                        "rotation": 0,
                        "opacity": 1,
                        "html": title,
                        "fontSize": 88,
                        "fontFamily": DEFAULT_THEME["fontFamily"],
                        "fontWeight": 800,
                        "color": DEFAULT_THEME["color"],
                        "align": "left",
                        "valign": "top",
                        "lineHeight": 1.1,
                    }
                ],
            }
        ],
        # docId is deliberately absent: the app mints a fresh identity on first
        # open. A tool must never invent one.
        "modified": "1970-01-01T00:00:00.000Z",
    }
    validate_doc(doc)
    _splice(path, raw, doc)
    print("created %s — one title slide, ready to author" % path)
    print("offline-only deck: no update check, no live collaboration, no network")
    print("next: bento_doc.py get %s -o doc.json" % path)
    print("download link (use this EXACT text, do not rebuild it): %s" % download_link(path))
    return 0


def cmd_get(args):
    doc = _decode_block(_split(_read(args.deck))[1])

    credential_fields = collab_credential_fields(doc)
    had_collab = "collab" in doc
    if "collab" in doc:
        # Strip collab whatever it holds, so the shape of what reaches the
        # caller does not depend on whether keys happened to be present. `set`
        # puts the original back, so nothing is lost by redacting here.
        del doc["collab"]
    # Warn on ANY collab block, not just one holding private keys: room + key is
    # already a joinable session, and a reader-role or v1 block has no priv keys
    # at all. Keying the warning on the credential fields alone would stay
    # silent on a deck that still joins a room the moment it is opened.
    if had_collab:
        sys.stderr.write(
            "WARNING: this deck carries live-collaboration credentials "
            "(%s). Any private keys are withheld from this output and are "
            "unchanged in this file for now.\n"
            "This deck is a live-session invitation: anyone who receives the "
            "file can join and write to it, and opening it joins that session "
            "with no click. `set` will REMOVE the session block, because fleet "
            "ships Bento as an offline editor. Tell the user before you go "
            "further - removing the keys does not retract an invitation already "
            "shared; the remedy for that is Share -> Rotate keys.\n"
            % (
                collab_field_label(credential_fields)
                if credential_fields
                else "collab.room + collab.key"
            )
        )

    payload = json.dumps(doc, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        _write_atomic(args.output, payload.encode("utf-8"))
        print("wrote %s (%d slides)" % (args.output, len(doc.get("slides", []))))
    else:
        sys.stdout.write(payload)
    return 0


def cmd_set(args):
    raw = _read(args.deck)
    prefix, block, suffix = _split(raw)

    # In a well-formed deck every "<" in the block is stored as <, so the
    # raw block contains no "<" at all. One that does was written by something
    # that did not escape — and if the unescaped markup was a </script>, _split
    # already cut the block short, so what we think we are about to replace is
    # not the real document. Either way the boundary cannot be trusted.
    if b"<" in block:
        raise DeckError(
            "the existing document block contains unescaped markup ('<'); this "
            "deck was written by something that did not escape it. Refusing to "
            "splice, because the block boundary cannot be trusted."
        )

    try:
        incoming = json.loads(_read(args.doc).decode("utf-8"))
    except ValueError as exc:
        raise DeckError("%s is not valid JSON: %s" % (args.doc, exc)) from exc
    if not isinstance(incoming, dict):
        raise DeckError("%s must contain a JSON object" % args.doc)
    validate_doc(incoming)

    # Carry the target's identity across the edit: docId must never be
    # regenerated. An incoming doc that supplies its own wins. An empty block (a
    # bare app shell) has nothing to carry; a non-empty one that will not parse
    # is a real problem and is reported, not ignored.
    current = {} if not block.strip() else _decode_block(block)
    if "docId" not in incoming and "docId" in current:
        incoming["docId"] = current["docId"]

    # collab is deliberately NOT carried, and is dropped if the incoming doc
    # supplies one. fleet ships Bento as an offline editor, and a collab block is
    # not inert data: `bornWithCollab = !!doc.collab` makes a deck share-eligible,
    # so merely carrying one opens a live session on load, with no click, and
    # retries. A deck written here has nothing for a session to attach to.
    #
    # This is reported rather than done silently, because it is the user's
    # session and only they can weigh it — and because removing the keys does not
    # retract an invitation already handed out (the remedy is Share -> Rotate
    # keys in the app).
    dropped_fields = sorted(
        set(collab_credential_fields(current)) | set(collab_credential_fields(incoming))
    )
    had_collab = "collab" in current or "collab" in incoming
    incoming.pop("collab", None)

    _splice(args.deck, raw, incoming)
    print(
        "updated %s — %d slide(s), %d bytes of document"
        % (args.deck, len(incoming["slides"]), len(_encode_block(incoming)))
    )
    if had_collab:
        sys.stderr.write(
            "NOTE: removed this deck's live-collaboration block%s. fleet ships "
            "Bento as an offline editor, and a deck that merely carries one "
            "joins a live session the moment it is opened. The deck now edits "
            "locally only.\n"
            "TELL THE USER: this does not retract an invitation already shared "
            "- anyone holding an earlier copy can still join that room. The "
            "remedy for that is Share -> Rotate keys in the app.\n"
            % (
                " (including credential fields: %s)" % collab_field_label(dropped_fields)
                if dropped_fields
                else ""
            )
        )
    print("download link (use this EXACT text, do not rebuild it): %s" % download_link(args.deck))
    return 0


def cmd_validate(args):
    raw = _read(args.path)
    if OPEN_TAG in raw:
        doc = _decode_block(_split(raw)[1])
        kind = "deck"
    else:
        try:
            doc = json.loads(raw.decode("utf-8"))
        except ValueError as exc:
            raise DeckError("%s is neither a deck nor valid JSON: %s" % (args.path, exc)) from exc
        if not isinstance(doc, dict):
            raise DeckError("%s must contain a JSON object" % args.path)
        kind = "document"

    validate_doc(doc)
    slides = doc["slides"]
    elements = sum(len(s["elements"]) for s in slides)
    print(
        "%s OK: %s, %d slide(s), %d element(s), %dx%d"
        % (
            args.path,
            kind,
            len(slides),
            elements,
            doc["size"]["width"],
            doc["size"]["height"],
        )
    )

    # Advisories, not failures: a deck that trips these still opens.
    for i, slide in enumerate(slides):
        if not slide["notes"].strip():
            print("  note: slide %d has no speaker notes" % (i + 1))
        for j, el in enumerate(slide["elements"]):
            if el.get("type") == "text":
                need = estimate_text_height(el)
                box = el.get("h")
                if (
                    need is not None
                    and isinstance(box, (int, float))
                    and need > box * _FIT_SLACK
                ):
                    print(
                        "  note: slide %d element %r may overflow its box - "
                        "about %dpx of text in %dpx. This is an estimate (no "
                        "font metrics here); give it room, or check the exact "
                        "number with window.bento.validate() in the browser."
                        % (i + 1, el.get("id"), round(need), box)
                    )
            right = el.get("x", 0) + el.get("w", 0)
            if isinstance(right, (int, float)) and right > doc["size"]["width"] - 96:
                print(
                    "  note: slide %d element %d extends past the 96px right "
                    "margin (x+w=%s)" % (i + 1, j + 1, right)
                )
    if "collab" in doc:
        credential_fields = collab_credential_fields(doc)
        print(
            "  WARNING: this deck carries a live-collaboration block%s. Opening "
            "it joins that session with no click. Re-write it with `set` to "
            "remove the block and make the deck offline-only."
            % (
                " including credential fields (%s)" % collab_field_label(credential_fields)
                if credential_fields
                else ""
            )
        )
    if kind == "deck" and not has_guard(raw):
        # A deck the user brought us, or one made before the guard existed. We
        # do not rewrite someone else's shell, so say so rather than fix it.
        print(
            "  note: this shell has no fleet offline guard, so opening it can "
            "reach the network (an update check, and a live session if the "
            "document carries one). Decks created by `new` cannot. We do not "
            "rewrite someone else's shell - tell the user, and offer to move "
            "the document into a fresh `new` deck if they want it locked down."
        )
    if kind == "deck":
        print(
            "download link (use this EXACT text, do not rebuild it): %s"
            % download_link(args.path)
        )
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(
        prog="bento_doc.py", description="Create and edit single-file Bento decks."
    )
    sub = parser.add_subparsers(dest="command", required=True)

    p_new = sub.add_parser("new", help="start a deck from the bundled Bento app")
    p_new.add_argument("deck", help="path to create, e.g. decks/Q4_Review.bento.html")
    p_new.add_argument("--title", help="deck title (default: derived from the filename)")
    p_new.set_defaults(func=cmd_new)

    p_get = sub.add_parser("get", help="extract a deck's document JSON")
    p_get.add_argument("deck")
    p_get.add_argument("-o", "--output", help="write to this file instead of stdout")
    p_get.set_defaults(func=cmd_get)

    p_set = sub.add_parser("set", help="write document JSON back into a deck")
    p_set.add_argument("deck")
    p_set.add_argument("doc", help="the document JSON to splice in")
    p_set.set_defaults(func=cmd_set)

    p_val = sub.add_parser("validate", help="check a deck or document for format errors")
    p_val.add_argument("path")
    p_val.set_defaults(func=cmd_validate)

    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except DeckError as exc:
        sys.stderr.write("error: %s\n" % exc)
        return 1


if __name__ == "__main__":
    sys.exit(main())
