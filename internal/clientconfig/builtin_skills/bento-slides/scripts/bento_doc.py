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

# ── the no-update-check guard ────────────────────────────────────────────────
#
# Upstream's shell checks bento.page for a newer version of itself on every
# launch (a signed manifest; the check is on by default and its switch lives in
# localStorage, so nothing in the file can preset it). fleet EMBEDS and
# sha256-pins the shell it ships, so that check can only ever report a version
# the reader has no way to install — while telling a third party, from their
# machine, that they opened this deck. `new` therefore plants the guard below
# into the shell, ahead of the runtime, and refuses the call.
#
# What it deliberately does NOT touch: `wss://sync.bento.page`, the
# collaboration relay. That is a connection the user opts into by sharing a
# deck, not a background call, and it uses WebSocket rather than fetch.
#
# Only `new` injects. `set` preserves the shell byte for byte, so a deck the
# USER handed us keeps upstream behavior — we do not silently rewrite someone
# else's file. `validate` reports which kind of deck it is looking at.
GUARD_ID = "fleet-no-update-check"
GUARD = b"""<script id="fleet-no-update-check">
/* Added by fleet's bento-slides skill. NOT part of upstream Bento.
   Upstream asks bento.page for a newer app shell on every launch. fleet embeds
   and sha256-pins the shell it ships, so that answer is unusable here, and the
   question alone tells a third party that this deck was opened. Refused.
   Collaboration (wss://sync.bento.page) is untouched: the user opts into that
   by sharing a deck.
   To restore upstream behavior, delete this entire script element. */
(function () {
  try { localStorage.setItem("bento-auto-check", "off"); } catch (e) {}
  var homeward = /^https?:\/\/(?:[a-z0-9-]+\.)*bento\.page(?:[:\/]|$)/i;
  var real = window.fetch;
  if (typeof real !== "function") { return; }
  window.fetch = function (input) {
    var url = typeof input === "string" ? input : (input && input.url) || "";
    if (homeward.test(url)) {
      return Promise.reject(new Error("fleet: this deck does not call home"));
    }
    return real.apply(this, arguments);
  };
})();
</script>
    """


# The vendored app shell, resolved relative to this script so it works from any
# working directory. The skills tree is bind-mounted read-only, so this is a
# read-only source: it is copied, never edited.
TEMPLATE = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    os.pardir,
    "templates",
    "Bento_Slides.bento.html",
)

# collab keys that are secrets rather than metadata. Their presence means the
# file is a live-session invitation, so `get` refuses to put them in front of a
# model and tells the caller to raise it with the user instead.
COLLAB_SECRET_KEYS = ("ownerPriv", "writerPriv", "invite")

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


def download_link(path):
    """The exact markdown link that resolves to this deck.

    The chat client rewrites a RELATIVE href to the conversation's workspace
    file API, so the link text must be the deck's path exactly as passed here
    (relative to the workspace, which is the cwd) — not its basename. Linking a
    bare filename for a deck written into a subdirectory is the one mistake that
    produces a deck the user cannot download, so the helper prints the answer
    rather than leaving it to be reconstructed.
    """
    rel = path.replace(os.sep, "/").lstrip("./")
    return "[%s](%s)" % (os.path.basename(rel), rel)


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


def collab_secrets(doc):
    """Return the names of live-session secret keys present in doc.collab."""
    collab = doc.get("collab")
    if not isinstance(collab, dict):
        return []
    return [k for k in COLLAB_SECRET_KEYS if collab.get(k)]


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
    if os.path.isabs(path):
        raise DeckError(
            "%s is an absolute path; a deck must be created at a path relative "
            "to the workspace, or the user will not be able to download it "
            "(the chat client only rewrites relative links)." % path
        )
    base = os.path.basename(path)
    bad = sorted({c for c in base if c not in SAFE_NAME})
    if bad:
        raise DeckError(
            "%r contains %s, which break the markdown download link (the href "
            "gets truncated or misparsed, so the deck 404s even though the file "
            "exists). Use only letters, digits, '.', '_' and '-' — e.g. %r."
            % (base, " ".join(repr(c) for c in bad), "Q4_Review.bento.html")
        )
    if not base.endswith(".bento.html"):
        raise DeckError(
            "%r should end in '.bento.html' so it is recognizable as a Bento "
            "deck." % base
        )
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
    print("the launch update-check to bento.page is disabled in this deck")
    print("next: bento_doc.py get %s -o doc.json" % path)
    print("download link (use this EXACT text, do not rebuild it): %s" % download_link(path))
    return 0


def cmd_get(args):
    doc = _decode_block(_split(_read(args.deck))[1])

    secrets = collab_secrets(doc)
    if "collab" in doc:
        # Strip collab whatever it holds, so the shape of what reaches the
        # caller does not depend on whether keys happened to be present. `set`
        # puts the original back, so nothing is lost by redacting here.
        del doc["collab"]
    if secrets:
        sys.stderr.write(
            "WARNING: this deck carries live-collaboration credentials "
            "(collab.%s). They have been withheld from this output and are "
            "unchanged in the file.\n"
            "This deck is a live-session invitation: anyone who receives the "
            "file can join and write to it. Tell the user before you go "
            "further — only they can decide. Removing the keys later does not "
            "retract them; the remedy is Share -> Rotate keys.\n"
            % ", ".join(secrets)
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

    # Carry the target's identity and live-session state across the edit:
    # docId must never be regenerated, and collab must survive `get`'s
    # redaction. An incoming doc that explicitly supplies either one wins.
    # An empty block (a bare app shell) has nothing to carry; a non-empty one
    # that will not parse is a real problem and is reported, not ignored.
    current = {} if not block.strip() else _decode_block(block)
    for key in ("docId", "collab"):
        if key not in incoming and key in current:
            incoming[key] = current[key]

    _splice(args.deck, raw, incoming)
    print(
        "updated %s — %d slide(s), %d bytes of document"
        % (args.deck, len(incoming["slides"]), len(_encode_block(incoming)))
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
            right = el.get("x", 0) + el.get("w", 0)
            if isinstance(right, (int, float)) and right > doc["size"]["width"] - 96:
                print(
                    "  note: slide %d element %d extends past the 96px right "
                    "margin (x+w=%s)" % (i + 1, j + 1, right)
                )
    if collab_secrets(doc):
        print("  note: this deck carries live-collaboration credentials")
    if kind == "deck" and not has_guard(raw):
        # A deck the user brought us, or one made before the guard existed. We
        # do not rewrite someone else's shell, so say so rather than fix it.
        print(
            "  note: no fleet update-check guard in this shell, so opening it "
            "will ask bento.page for a newer app version. Decks created by "
            "`new` have that disabled; this one was not. Tell the user rather "
            "than editing their shell."
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
