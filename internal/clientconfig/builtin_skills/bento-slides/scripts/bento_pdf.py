#!/usr/bin/env python3
"""Render a Bento document to a static PDF — one page per visible slide.

`bento_doc.py pdf` is the entry point; this module is the renderer behind it.
It exists so the agent can finish the job it was asked to do: build a deck AND
hand back something attachable. Producing a PDF used to need the reader to open
the deck and click the printer icon, which is fine for "here is my deck" and
useless for "email this to the board".

WHAT THIS IS. A second, deliberately small renderer for the STATIC form of a
document: the same document JSON, the same layout arithmetic, drawn with PDF
operators instead of DOM nodes. It is not a browser and does not pretend to be
one. The app's own *Export PDF (print)* stays the authority, and the skill says
so — this is the export you attach to mail, that one is the export you use when
a page has to be pixel-exact.

WHY IT CAN BE FAITHFUL AT ALL. A PDF page is static, so the parts of Bento that
are hard to reproduce are also the parts a page cannot show. The app's own PDF
path (`exportPdf` in the runtime) renders each slide through the SAME static
renderer used for thumbnails, with `svgAsImage: true` and placeholders hidden,
then prints it: no entrance tweens, no morph, no count-up, no motion paths, no
ken-burns. So "static" is not a compromise this module invented; it is what a
Bento PDF is. What we do reproduce is the geometry and typography that decide
whether a slide reads correctly: the element box model, text wrapping and
alignment, tables, gradients, images, and the charts-lite engine, ported from
the vendored runtime rather than re-imagined.

WHERE IT IS HONESTLY WEAKER than the in-app export, all reported on stderr as
warnings when a deck actually hits one:

  * Fonts are the PDF core 14 (Helvetica/Times/Courier, mapped from the CSS
    stack). A deck that embeds a woff2 face renders in the mapped fallback,
    because turning woff2 into an embeddable PDF font needs a brotli decoder
    and a TrueType subsetter, neither of which is in the standard library. Text
    metrics are the real Adobe AFM widths for the face we actually embed, so
    wrapping is exact for THIS PDF — it just isn't the deck's typeface.
  * Text is WinAnsi (Western European). Common typographic symbols outside it
    are transliterated (-> for an arrow, x for a multiplication sign); other
    non-Latin text becomes '?' and is counted in a warning. CJK, Greek beyond
    the common symbols, Hebrew, Arabic and emoji need the in-app export.
  * `svg` elements, KaTeX math, blur, shadow and blend modes are skipped.
    Video/audio render as the same neutral poster block the app's print path
    draws.

Standard library only, like `bento_doc.py`: no reportlab, no browser, nothing
added to the sandbox image. Every failure raises PdfError before a byte of the
output file is written.
"""

import base64
import binascii
import html
import math
import re
import struct
import time
import zlib


class PdfError(Exception):
    """Rendering failed; the caller has written nothing."""


# ── page geometry ────────────────────────────────────────────────────────────
#
# The deck canvas is in CSS px; a PDF is in points. 960pt (13.333in) wide is the
# standard 16:9 slide page — what PowerPoint and Keynote export — so the file
# looks native in a PDF reader and prints on one landscape sheet per slide. On a
# 1280px canvas that is exactly 0.75 pt/px; a deck with a different `size` keeps
# its aspect ratio.
PAGE_WIDTH_PT = 960.0


# ── colors ───────────────────────────────────────────────────────────────────

_NAMED = {
    "transparent": (0, 0, 0, 0.0),
    "none": (0, 0, 0, 0.0),
    "black": (0, 0, 0, 1.0),
    "white": (1, 1, 1, 1.0),
    "red": (1, 0, 0, 1.0),
    "green": (0, 0.502, 0, 1.0),
    "blue": (0, 0, 1, 1.0),
    "gray": (0.502, 0.502, 0.502, 1.0),
    "grey": (0.502, 0.502, 0.502, 1.0),
    "silver": (0.753, 0.753, 0.753, 1.0),
    "navy": (0, 0, 0.502, 1.0),
    "teal": (0, 0.502, 0.502, 1.0),
    "orange": (1, 0.647, 0, 1.0),
    "yellow": (1, 1, 0, 1.0),
    "purple": (0.502, 0, 0.502, 1.0),
    "inherit": None,
    "currentcolor": None,
}

_RGB_FN = re.compile(
    r"^rgba?\(\s*([0-9.]+%?)[\s,]+([0-9.]+%?)[\s,]+([0-9.]+%?)"
    r"(?:[\s,/]+([0-9.]+%?))?\s*\)$",
    re.I,
)


def _chan(tok):
    tok = tok.strip()
    if tok.endswith("%"):
        return max(0.0, min(1.0, float(tok[:-1]) / 100.0))
    return max(0.0, min(1.0, float(tok) / 255.0))


def parse_color(value, default=(0, 0, 0, 1.0)):
    """CSS color -> (r, g, b, alpha) in 0..1, or `default`.

    Covers what a Bento document can actually contain: hex (3/4/6/8 digits),
    rgb()/rgba() including the space-slash form, `transparent`, and the handful
    of names that turn up in hand-written JSON. Anything else falls back rather
    than failing the export — a wrong swatch is recoverable, a refused deck is
    not.
    """
    if not isinstance(value, str):
        return default
    v = value.strip().lower()
    if not v:
        return default
    if v in _NAMED:
        got = _NAMED[v]
        return default if got is None else got
    if v.startswith("#"):
        h = v[1:]
        try:
            if len(h) in (3, 4):
                vals = [int(c * 2, 16) / 255.0 for c in h]
            elif len(h) in (6, 8):
                vals = [int(h[i : i + 2], 16) / 255.0 for i in range(0, len(h), 2)]
            else:
                return default
        except ValueError:
            return default
        if len(vals) == 3:
            return (vals[0], vals[1], vals[2], 1.0)
        return (vals[0], vals[1], vals[2], vals[3])
    m = _RGB_FN.match(v)
    if m:
        r, g, b, a = m.groups()
        alpha = 1.0
        if a is not None:
            alpha = float(a[:-1]) / 100.0 if a.endswith("%") else float(a)
        return (_chan(r), _chan(g), _chan(b), max(0.0, min(1.0, alpha)))
    return default


def is_visible(rgba):
    return rgba is not None and rgba[3] > 0.001


# ── fonts ────────────────────────────────────────────────────────────────────
#
# Adobe AFM advance widths for the PDF core fonts, in 1/1000 em, listed in
# WinAnsiEncoding order from code 32 to 255. These are the metrics the PDF
# viewer itself will use to set the text we emit, so a line that fits here fits
# in the rendered page exactly — unlike bento_doc.py's `validate` heuristic,
# which has to guess at a font it will never see. Zeros are WinAnsi's unused
# slots and never reached: text is transliterated into this encoding first.
_WINANSI_WIDTHS_SRC = {
    "Helvetica": "278 278 355 556 556 889 667 191 333 333 389 584 278 333 278 278 556 "
    "556 556 556 556 556 556 556 556 556 278 278 584 584 584 556 1015 667 "
    "667 722 722 667 611 778 722 278 500 667 556 833 722 778 667 778 722 "
    "667 611 722 667 944 667 667 611 278 278 278 469 556 333 556 556 500 "
    "556 556 278 556 556 222 222 500 222 833 556 556 556 556 333 500 278 "
    "556 500 722 500 500 500 334 260 334 584 350 556 350 222 556 333 1000 "
    "556 556 333 1000 667 333 1000 350 611 350 350 222 222 333 333 350 "
    "556 1000 333 1000 500 333 944 350 500 667 278 333 556 556 556 556 260 "
    "556 333 737 370 556 584 333 737 552 400 549 333 333 333 576 537 278 "
    "333 333 365 556 834 834 834 611 667 667 667 667 667 667 1000 722 667 "
    "667 667 667 278 278 278 278 722 722 778 778 778 778 778 584 778 722 "
    "722 722 722 667 667 611 556 556 556 556 556 556 889 500 556 556 556 "
    "556 278 278 278 278 556 556 556 556 556 556 556 549 611 556 556 556 "
    "556 500 556 500",
    "Helvetica-Bold": "278 333 474 556 556 889 722 238 333 333 389 584 278 333 278 278 556 "
    "556 556 556 556 556 556 556 556 556 333 333 584 584 584 611 975 722 "
    "722 722 722 667 611 778 722 278 556 722 611 833 722 778 667 778 722 "
    "667 611 722 667 944 667 667 611 333 278 333 584 556 333 556 611 556 "
    "611 556 333 611 611 278 278 556 278 889 611 611 611 611 389 556 333 "
    "611 556 778 556 556 500 389 280 389 584 350 556 350 278 556 500 1000 "
    "556 556 333 1000 667 333 1000 350 611 350 350 278 278 500 500 350 556 "
    "1000 333 1000 556 333 944 350 500 667 278 333 556 556 556 556 280 556 "
    "333 737 370 556 584 333 737 552 400 549 333 333 333 576 556 278 333 "
    "333 365 556 834 834 834 611 722 722 722 722 722 722 1000 722 667 667 "
    "667 667 278 278 278 278 722 722 778 778 778 778 778 584 778 722 722 "
    "722 722 667 667 611 556 556 556 556 556 556 889 556 556 556 556 556 "
    "278 278 278 278 611 611 611 611 611 611 611 549 611 611 611 611 611 "
    "556 611 556",
    "Helvetica-Oblique": "278 278 355 556 556 889 667 191 333 333 389 584 278 333 278 278 556 "
    "556 556 556 556 556 556 556 556 556 278 278 584 584 584 556 1015 667 "
    "667 722 722 667 611 778 722 278 500 667 556 833 722 778 667 778 722 "
    "667 611 722 667 944 667 667 611 278 278 278 469 556 333 556 556 500 "
    "556 556 278 556 556 222 222 500 222 833 556 556 556 556 333 500 278 "
    "556 500 722 500 500 500 334 260 334 584 350 556 350 222 556 333 1000 "
    "556 556 333 1000 667 333 1000 350 611 350 350 222 222 333 333 350 "
    "556 1000 333 1000 500 333 944 350 500 667 278 333 556 556 556 556 260 "
    "556 333 737 370 556 584 333 737 552 400 549 333 333 333 576 537 278 "
    "333 333 365 556 834 834 834 611 667 667 667 667 667 667 1000 722 667 "
    "667 667 667 278 278 278 278 722 722 778 778 778 778 778 584 778 722 "
    "722 722 722 667 667 611 556 556 556 556 556 556 889 500 556 556 556 "
    "556 278 278 278 278 556 556 556 556 556 556 556 549 611 556 556 556 "
    "556 500 556 500",
    "Helvetica-BoldOblique": "278 333 474 556 556 889 722 238 333 333 389 584 278 333 278 278 556 "
    "556 556 556 556 556 556 556 556 556 333 333 584 584 584 611 975 722 "
    "722 722 722 667 611 778 722 278 556 722 611 833 722 778 667 778 722 "
    "667 611 722 667 944 667 667 611 333 278 333 584 556 333 556 611 556 "
    "611 556 333 611 611 278 278 556 278 889 611 611 611 611 389 556 333 "
    "611 556 778 556 556 500 389 280 389 584 350 556 350 278 556 500 1000 "
    "556 556 333 1000 667 333 1000 350 611 350 350 278 278 500 500 350 556 "
    "1000 333 1000 556 333 944 350 500 667 278 333 556 556 556 556 280 556 "
    "333 737 370 556 584 333 737 552 400 549 333 333 333 576 556 278 333 "
    "333 365 556 834 834 834 611 722 722 722 722 722 722 1000 722 667 667 "
    "667 667 278 278 278 278 722 722 778 778 778 778 778 584 778 722 722 "
    "722 722 667 667 611 556 556 556 556 556 556 889 556 556 556 556 556 "
    "278 278 278 278 611 611 611 611 611 611 611 549 611 611 611 611 611 "
    "556 611 556",
    "Times-Roman": "250 333 408 500 500 833 778 180 333 333 500 564 250 333 250 278 500 "
    "500 500 500 500 500 500 500 500 500 278 278 564 564 564 444 921 722 "
    "667 667 722 611 556 722 722 333 389 722 611 889 722 722 556 722 667 "
    "556 611 722 722 944 722 722 611 333 278 333 469 500 333 444 500 444 "
    "500 444 333 500 500 278 278 500 278 778 500 500 500 500 333 389 278 "
    "500 500 722 500 500 444 480 200 480 541 350 500 350 333 500 444 1000 "
    "500 500 333 1000 556 333 889 350 611 350 350 333 333 444 444 350 500 "
    "1000 333 980 389 333 722 350 444 722 250 333 500 500 500 500 200 500 "
    "333 760 276 500 564 333 760 500 400 549 300 300 333 576 453 250 333 "
    "300 310 500 750 750 750 444 722 722 722 722 722 722 889 667 611 611 "
    "611 611 333 333 333 333 722 722 722 722 722 722 722 564 722 722 722 "
    "722 722 722 556 500 444 444 444 444 444 444 667 444 444 444 444 444 "
    "278 278 278 278 500 500 500 500 500 500 500 549 500 500 500 500 500 "
    "500 500 500",
    "Times-Bold": "250 333 555 500 500 1000 833 278 333 333 500 570 250 333 250 278 500 "
    "500 500 500 500 500 500 500 500 500 333 333 570 570 570 500 930 722 "
    "667 722 722 667 611 778 778 389 500 778 667 944 722 778 611 778 722 "
    "556 667 722 722 1000 722 722 667 333 278 333 581 500 333 500 556 444 "
    "556 444 333 500 556 278 333 556 278 833 556 500 556 556 444 389 333 "
    "556 500 722 500 500 444 394 220 394 520 350 500 350 333 500 500 1000 "
    "500 500 333 1000 556 333 1000 350 667 350 350 333 333 500 500 350 500 "
    "1000 333 1000 389 333 722 350 444 722 250 333 500 500 500 500 220 500 "
    "333 747 300 500 570 333 747 500 400 549 300 300 333 576 500 250 333 "
    "300 330 500 750 750 750 500 722 722 722 722 722 722 1000 722 667 667 "
    "667 667 389 389 389 389 722 722 778 778 778 778 778 570 778 722 722 "
    "722 722 722 611 556 500 500 500 500 500 500 722 444 444 444 444 444 "
    "278 278 278 278 500 556 500 500 500 500 500 549 500 556 556 556 556 "
    "500 556 500",
    "Times-Italic": "250 333 420 500 500 833 778 214 333 333 500 675 250 333 250 278 500 "
    "500 500 500 500 500 500 500 500 500 333 333 675 675 675 500 920 611 "
    "611 667 722 611 611 722 722 333 444 667 556 833 667 722 611 722 611 "
    "500 556 722 611 833 611 556 556 389 278 389 422 500 333 500 500 444 "
    "500 444 278 500 500 278 278 444 278 722 500 500 500 500 389 389 278 "
    "500 444 667 444 444 389 400 275 400 541 350 500 350 333 500 556 889 "
    "500 500 333 1000 500 333 944 350 556 350 350 333 333 556 556 350 500 "
    "889 333 980 389 333 667 350 389 556 250 389 500 500 500 500 275 500 "
    "333 760 276 500 675 333 760 500 400 549 300 300 333 576 523 250 333 "
    "300 310 500 750 750 750 500 611 611 611 611 611 611 889 667 611 611 "
    "611 611 333 333 333 333 722 667 722 722 722 722 722 675 722 722 722 "
    "722 722 556 611 500 500 500 500 500 500 500 667 444 444 444 444 444 "
    "278 278 278 278 500 500 500 500 500 500 500 549 500 500 500 500 500 "
    "444 500 444",
    "Times-BoldItalic": "250 389 555 500 500 833 778 278 333 333 500 570 250 333 250 278 500 "
    "500 500 500 500 500 500 500 500 500 333 333 570 570 570 500 832 667 "
    "667 667 722 667 667 722 778 389 500 667 611 889 722 722 611 722 667 "
    "556 611 722 667 889 667 611 611 333 278 333 570 500 333 500 500 444 "
    "500 444 333 444 500 278 278 444 278 722 500 500 500 500 389 389 278 "
    "500 444 667 500 444 389 348 220 348 570 350 500 350 333 500 500 1000 "
    "500 500 333 1000 556 333 944 350 611 350 350 333 333 500 500 350 500 "
    "1000 333 1000 389 333 722 350 389 611 250 389 500 500 500 500 220 500 "
    "333 747 266 500 606 333 747 500 400 549 300 300 333 576 500 250 333 "
    "300 300 500 750 750 750 500 667 667 667 667 667 667 944 667 667 667 "
    "667 667 389 389 389 389 722 722 722 722 722 722 722 570 722 722 722 "
    "722 722 611 611 500 500 500 500 500 500 500 722 444 444 444 444 444 "
    "278 278 278 278 500 500 500 500 500 500 500 549 500 500 500 500 500 "
    "444 500 444",
}

WIDTHS = {
    name: [int(n) for n in src.split()] for name, src in _WINANSI_WIDTHS_SRC.items()
}
WIDTHS["Courier"] = [600] * 224
WIDTHS["Courier-Bold"] = [600] * 224
WIDTHS["Courier-Oblique"] = [600] * 224
WIDTHS["Courier-BoldOblique"] = [600] * 224

# Vertical metrics (fraction of em) for the three families we can emit. Used to
# place the first baseline inside a CSS line box the same way a browser does:
# half the leading above the ascent.
VMETRICS = {
    "Helvetica": (0.718, 0.207),
    "Times": (0.683, 0.217),
    "Courier": (0.629, 0.157),
}

_SERIF_HINTS = (
    "serif",
    "georgia",
    "times",
    "garamond",
    "fraunces",
    "playfair",
    "merriweather",
    "cambria",
    "book",
    "charter",
    "spectral",
    "lora",
    "source serif",
    "pt serif",
    "noto serif",
    "ibm plex serif",
    "instrument serif",
    "newsreader",
    "literata",
    "bitter",
)
_MONO_HINTS = (
    "mono",
    "courier",
    "consolas",
    "menlo",
    "sf mono",
    "jetbrains",
    "fira code",
    "source code",
    "ibm plex mono",
    "roboto mono",
)


def family_of(stack):
    """CSS font stack -> "Helvetica" | "Times" | "Courier".

    A deck names a stack (or a face it embeds); we can only emit the core 14, so
    the job is to keep the SHAPE right — a serif deck must not come back sans.
    The first family in the stack decides, and generic keywords anywhere in it
    are the fallback signal, exactly as the browser would resolve them.
    """
    if not isinstance(stack, str) or not stack.strip():
        return "Helvetica"
    low = stack.lower()
    first = low.split(",")[0].strip().strip("'\"")
    for hint in _MONO_HINTS:
        if hint in first:
            return "Courier"
    for hint in _SERIF_HINTS:
        if hint in first:
            return "Times"
    # A named face we do not recognise (very often one the deck embeds): let the
    # rest of the stack decide, which is what the browser does when the named
    # face is missing there too.
    for hint in _MONO_HINTS:
        if hint in low:
            return "Courier"
    if "sans-serif" in low:
        return "Helvetica"
    for hint in _SERIF_HINTS:
        if hint in low:
            return "Times"
    return "Helvetica"


def face_name(family, bold, italic):
    """(family, bold, italic) -> a PDF core-14 BaseFont name."""
    if family == "Times":
        if bold and italic:
            return "Times-BoldItalic"
        if bold:
            return "Times-Bold"
        if italic:
            return "Times-Italic"
        return "Times-Roman"
    suffix = ""
    if bold and italic:
        suffix = "-BoldOblique"
    elif bold:
        suffix = "-Bold"
    elif italic:
        suffix = "-Oblique"
    return family + suffix


def is_bold(weight):
    if isinstance(weight, (int, float)):
        return weight >= 600
    if isinstance(weight, str):
        w = weight.strip().lower()
        if w in ("bold", "bolder", "black", "heavy"):
            return True
        try:
            return float(w) >= 600
        except ValueError:
            return False
    return False


# ── text: WinAnsi encoding, inline markup, measurement, wrapping ─────────────
#
# The core-14 fonts are WinAnsi-encoded, so a character outside Latin-1 has no
# glyph to point at. Rather than dropping it silently (a slide that reads
# "Revenue  Q4" tells the reader nothing went wrong), transliterate the symbols
# a deck actually uses and count the rest into a warning the agent can pass on.
# Only characters WinAnsi genuinely cannot carry. Anything cp1252 encodes on its
# own (bullet, ellipsis, en/em dash, curly quotes, guillemets, multiplication
# sign, plus-minus) is left alone — transliterating those would degrade text the
# PDF can render perfectly.
_TRANSLIT = {
    "→": "->",
    "←": "<-",
    "↔": "<->",
    "⇒": "=>",
    "⇐": "<=",
    "↑": "^",
    "↓": "v",
    "−": "-",
    "≤": "<=",
    "≥": ">=",
    "≈": "~",
    "≠": "!=",
    "′": "'",
    "″": '"',
    "✓": "*",
    "✔": "*",
    "✗": "x",
    "✘": "x",
    "▶": ">",
    "◀": "<",
    "▪": "\u2022",
    "●": "\u2022",
    # Exotic spaces a model can paste in: render as a normal space rather than
    # as a missing glyph in the middle of a headline.
    "\u00a0": " ",
    "\u2007": " ",
    "\u2009": " ",
    "\u202f": " ",
}


class TextEncoder:
    """Turns document text into WinAnsi bytes, counting what it had to give up.

    One instance per export, so the warning is per deck rather than per string.
    """

    def __init__(self):
        self.dropped = 0
        self.samples = []

    def encode(self, text):
        out = bytearray()
        for ch in text:
            sub = _TRANSLIT.get(ch, ch)
            for c in sub:
                try:
                    out += c.encode("cp1252")
                except UnicodeEncodeError:
                    out += b"?"
                    self.dropped += 1
                    if ch not in self.samples and len(self.samples) < 6:
                        self.samples.append(ch)
        return bytes(out)

    def codes(self, text):
        """The WinAnsi code points a string will occupy, for measurement."""
        return self.encode(text)

    def warning(self):
        if not self.dropped:
            return None
        shown = " ".join("%r" % c for c in self.samples)
        return (
            "%d character(s) have no glyph in the PDF core fonts and were "
            "written as '?' (%s). Text outside Western European scripts "
            "needs the deck's own Export PDF (print) button." % (self.dropped, shown)
        )


def text_width(encoder, text, face, size, letter_spacing=0.0):
    """Rendered advance width, in the same px units as the document."""
    table = WIDTHS[face]
    total = 0
    for byte in encoder.codes(text):
        idx = byte - 32
        total += table[idx] if 0 <= idx < len(table) else 500
    return total / 1000.0 * size + letter_spacing * len(text)


_TAG = re.compile(r"<(/?)([a-zA-Z][a-zA-Z0-9]*)[^>]*>")

_BOLD_TAGS = {"b", "strong"}
_ITALIC_TAGS = {"i", "em", "cite", "var"}
_MONO_TAGS = {"code", "kbd", "samp", "tt"}


def unescape(text):
    """Resolve HTML entities.

    `html.unescape` knows the whole HTML5 named table plus numeric references,
    which is what the browser resolves — a hand-kept subset silently ships
    "&middot;" onto a slide the day someone uses an entity that is not in it.
    """
    return html.unescape(text)


class Run:
    """A stretch of text sharing one face. `break_before` marks a hard <br>."""

    __slots__ = ("text", "bold", "italic", "mono", "break_before")

    def __init__(self, text, bold, italic, mono, break_before=False):
        self.text = text
        self.bold = bold
        self.italic = italic
        self.mono = mono
        self.break_before = break_before


def parse_inline(html):
    """Bento inline markup -> runs. Everything not inline styling is dropped.

    Bento allows `<b>`, `<i>` and `<br>` in slide text, and the app's own
    sanitizer keeps a slightly wider inline set (`code`, `u`, `em`, `strong`).
    Unknown tags are removed and their text kept, which is what a browser shows
    once the sanitizer has stripped them.
    """
    runs = []
    bold_depth = italic_depth = mono_depth = 0
    pending_break = False
    pos = 0
    text = str(html or "")

    def push(chunk):
        nonlocal pending_break
        if not chunk:
            return
        runs.append(
            Run(
                unescape(chunk),
                bold_depth > 0,
                italic_depth > 0,
                mono_depth > 0,
                pending_break,
            )
        )
        pending_break = False

    for match in _TAG.finditer(text):
        push(text[pos : match.start()])
        pos = match.end()
        closing = match.group(1) == "/"
        name = match.group(2).lower()
        if name == "br":
            if runs or pending_break:
                pending_break = True
            else:
                runs.append(Run("", False, False, False, False))
                pending_break = True
            continue
        step = -1 if closing else 1
        if name in _BOLD_TAGS:
            bold_depth = max(0, bold_depth + step)
        elif name in _ITALIC_TAGS:
            italic_depth = max(0, italic_depth + step)
        elif name in _MONO_TAGS:
            mono_depth = max(0, mono_depth + step)
    push(text[pos:])
    if pending_break:
        runs.append(Run("", bold_depth > 0, italic_depth > 0, mono_depth > 0, True))
    return runs


class Piece:
    """One measured, positioned span on a laid-out line."""

    __slots__ = ("text", "face", "size", "width", "x")

    def __init__(self, text, face, size, width):
        self.text = text
        self.face = face
        self.size = size
        self.width = width
        self.x = 0.0


class Line:
    __slots__ = ("pieces", "width")

    def __init__(self):
        self.pieces = []
        self.width = 0.0

    def add(self, piece):
        piece.x = self.width
        self.pieces.append(piece)
        self.width += piece.width


_SPLIT = re.compile(r"(\s+)")


def layout_text(
    encoder, runs, box_width, family, size, weight, letter_spacing=0.0, wrap=True
):
    """Greedy word wrap into `box_width`, honouring hard breaks and runs.

    Mirrors the browser closely enough to matter: `overflow-wrap: break-word` is
    on in the app's print CSS, so a single word longer than the box is broken
    rather than allowed to run off the slide.
    """
    base_bold = is_bold(weight)
    lines = [Line()]
    for run in runs:
        if run.break_before:
            lines.append(Line())
        if not run.text:
            continue
        run_family = "Courier" if run.mono else family
        face = face_name(run_family, base_bold or run.bold, run.italic)
        run_size = size * 0.9 if run.mono else size
        tokens = [t for t in _SPLIT.split(run.text) if t]
        for token in tokens:
            width = text_width(encoder, token, face, run_size, letter_spacing)
            line = lines[-1]
            blank = not line.pieces
            if token.strip() == "":
                if blank:
                    continue  # a browser collapses leading whitespace away
                line.add(Piece(token, face, run_size, width))
                continue
            if not wrap or blank or line.width + width <= box_width + 0.01:
                if wrap and blank and width > box_width + 0.01:
                    for part in _break_word(
                        encoder, token, face, run_size, box_width, letter_spacing
                    ):
                        if lines[-1].pieces:
                            lines.append(Line())
                        lines[-1].add(part)
                    continue
                line.add(Piece(token, face, run_size, width))
            else:
                while line.pieces and line.pieces[-1].text.strip() == "":
                    line.width -= line.pieces.pop().width
                lines.append(Line())
                if width > box_width + 0.01:
                    for part in _break_word(
                        encoder, token, face, run_size, box_width, letter_spacing
                    ):
                        if lines[-1].pieces:
                            lines.append(Line())
                        lines[-1].add(part)
                else:
                    lines[-1].add(Piece(token, face, run_size, width))
    for line in lines:
        while line.pieces and line.pieces[-1].text.strip() == "":
            line.width -= line.pieces.pop().width
    return lines


def _break_word(encoder, token, face, size, box_width, letter_spacing):
    """Split an over-long word into box-width chunks (break-word behaviour)."""
    out = []
    current = ""
    current_width = 0.0
    for ch in token:
        advance = text_width(encoder, ch, face, size, letter_spacing)
        if current and current_width + advance > box_width + 0.01:
            out.append(Piece(current, face, size, current_width))
            current, current_width = ch, advance
        else:
            current += ch
            current_width += advance
    if current:
        out.append(Piece(current, face, size, current_width))
    return out


# ── the PDF file ─────────────────────────────────────────────────────────────
#
# A slide deck needs a small corner of PDF: pages, one content stream each, core
# fonts, image XObjects, axial shadings and an ExtGState per alpha. Writing that
# directly keeps this module standard-library-only, which is the whole point —
# the sandbox image gains nothing to make a deck exportable.


def pdf_string(raw):
    """Escape bytes for a PDF literal string."""
    out = bytearray(b"(")
    for byte in raw:
        if byte in (0x28, 0x29, 0x5C):
            out += b"\\" + bytes([byte])
        elif byte == 0x0D:
            out += b"\\r"
        elif byte == 0x0A:
            out += b"\\n"
        else:
            out.append(byte)
    out += b")"
    return bytes(out)


def num(value):
    """Compact fixed-point number: PDF has no exponent notation."""
    if value is None or not isinstance(value, (int, float)):
        return "0"
    if isinstance(value, float) and (
        value != value or value in (float("inf"), float("-inf"))
    ):
        return "0"
    text = "%.4f" % value
    text = text.rstrip("0").rstrip(".")
    return text if text not in ("", "-", "-0") else "0"


class PdfWriter:
    def __init__(self):
        self._objects = [None]  # 1-based object numbers
        self.compress = True

    def reserve(self):
        self._objects.append(b"")
        return len(self._objects) - 1

    def put(self, number, body):
        self._objects[number] = (
            body if isinstance(body, bytes) else body.encode("latin-1")
        )

    def add(self, body):
        number = self.reserve()
        self.put(number, body)
        return number

    def add_stream(self, entries, data, compress=None):
        if compress is None:
            compress = self.compress
        if compress:
            data = zlib.compress(data, 9)
            entries = entries + ["/Filter /FlateDecode"]
        head = "<< %s /Length %d >>\nstream\n" % (" ".join(entries), len(data))
        return self.add(head.encode("latin-1") + data + b"\nendstream")

    def serialize(self, root, info):
        out = bytearray(b"%PDF-1.5\n%\xe2\xe3\xcf\xd3\n")
        offsets = [0] * len(self._objects)
        for number in range(1, len(self._objects)):
            offsets[number] = len(out)
            out += ("%d 0 obj\n" % number).encode("latin-1")
            out += self._objects[number]
            out += b"\nendobj\n"
        start = len(out)
        out += ("xref\n0 %d\n" % len(self._objects)).encode("latin-1")
        out += b"0000000000 65535 f \n"
        for number in range(1, len(self._objects)):
            out += ("%010d 00000 n \n" % offsets[number]).encode("latin-1")
        out += (
            "trailer\n<< /Size %d /Root %d 0 R /Info %d 0 R >>\n"
            "startxref\n%d\n%%%%EOF\n" % (len(self._objects), root, info, start)
        ).encode("latin-1")
        return bytes(out)


# ── images ───────────────────────────────────────────────────────────────────

_DATA_URI = re.compile(r"^data:([^;,]*)(;[^,]*)?,", re.I)


def decode_data_uri(src):
    """data: URI -> (mime, bytes), or None for anything else."""
    if not isinstance(src, str):
        return None
    match = _DATA_URI.match(src.strip())
    if not match:
        return None
    mime = (match.group(1) or "").lower()
    params = (match.group(2) or "").lower()
    payload = src[match.end() :]
    try:
        if "base64" in params:
            data = base64.b64decode(payload + "=" * (-len(payload) % 4))
        else:
            from urllib.parse import unquote_to_bytes

            data = unquote_to_bytes(payload)
    except (binascii.Error, ValueError):
        return None
    return mime, data


class Image:
    """A decoded raster ready to become an XObject."""

    __slots__ = (
        "width",
        "height",
        "data",
        "filter",
        "colorspace",
        "bpc",
        "smask",
        "palette",
    )

    def __init__(
        self, width, height, data, filt, colorspace, bpc=8, smask=None, palette=None
    ):
        self.width = width
        self.height = height
        self.data = data
        self.filter = filt
        self.colorspace = colorspace
        self.bpc = bpc
        self.smask = smask
        self.palette = palette


def _jpeg_size(data):
    """Parse a JPEG's SOF marker for dimensions and component count."""
    i = 2
    while i + 9 < len(data):
        if data[i] != 0xFF:
            i += 1
            continue
        marker = data[i + 1]
        if marker in (0xD8, 0xD9) or 0xD0 <= marker <= 0xD7 or marker == 0x01:
            i += 2
            continue
        length = struct.unpack(">H", data[i + 2 : i + 4])[0]
        if 0xC0 <= marker <= 0xCF and marker not in (0xC4, 0xC8, 0xCC):
            height, width = struct.unpack(">HH", data[i + 5 : i + 9])
            components = data[i + 9]
            return width, height, components
        i += 2 + length
    return None


def _png_decode(data):
    """Decode a non-interlaced 8-bit PNG into raw samples (+ alpha mask).

    PNG is the format a model actually embeds — matplotlib writes it, and it is
    what an `asset:` data URI usually carries. The IDAT stream is zlib, so
    stdlib gets us the bytes; undoing the per-scanline filters is the only real
    work, and it is what lets an image become a plain PDF /FlateDecode XObject
    instead of a rewrite.
    """
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        return "not a PNG"
    pos = 8
    header = None
    idat = bytearray()
    palette = None
    trns = None
    while pos + 8 <= len(data):
        length, kind = struct.unpack(">I4s", data[pos : pos + 8])
        body = data[pos + 8 : pos + 8 + length]
        pos += 12 + length
        if kind == b"IHDR":
            width, height, depth, color, _comp, _filt, interlace = struct.unpack(
                ">IIBBBBB", body[:13]
            )
            header = (width, height, depth, color, interlace)
        elif kind == b"PLTE":
            palette = bytes(body)
        elif kind == b"tRNS":
            trns = bytes(body)
        elif kind == b"IDAT":
            idat += body
        elif kind == b"IEND":
            break
    if header is None:
        return "the PNG has no IHDR chunk"
    width, height, depth, color, interlace = header
    if interlace:
        return "the PNG is interlaced (Adam7)"
    if depth not in (8, 16):
        return "the PNG has %d-bit samples" % depth
    if color not in (0, 2, 3, 4, 6):
        return "the PNG uses colour type %d" % color
    channels = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[color]
    if color == 3 and palette is None:
        return "the PNG is palette-indexed but carries no palette"
    sample_bytes = depth // 8
    stride = width * channels * sample_bytes
    try:
        raw = zlib.decompress(bytes(idat))
    except zlib.error:
        return "the PNG image data is corrupt"
    if len(raw) < (stride + 1) * height:
        return "the PNG image data is truncated"

    # Undo the five PNG scanline filters in place.
    unit = channels * sample_bytes
    out = bytearray(stride * height)
    prev = bytearray(stride)
    at = 0
    for row in range(height):
        filt = raw[at]
        at += 1
        line = bytearray(raw[at : at + stride])
        at += stride
        if filt == 1:
            for i in range(unit, stride):
                line[i] = (line[i] + line[i - unit]) & 0xFF
        elif filt == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 0xFF
        elif filt == 3:
            for i in range(stride):
                left = line[i - unit] if i >= unit else 0
                line[i] = (line[i] + ((left + prev[i]) >> 1)) & 0xFF
        elif filt == 4:
            for i in range(stride):
                left = line[i - unit] if i >= unit else 0
                up = prev[i]
                upper_left = prev[i - unit] if i >= unit else 0
                peak = left + up - upper_left
                da, db, dc = (abs(peak - left), abs(peak - up), abs(peak - upper_left))
                nearest = (
                    left
                    if (da <= db and da <= dc)
                    else (up if db <= dc else upper_left)
                )
                line[i] = (line[i] + nearest) & 0xFF
        elif filt != 0:
            return "the PNG uses an unknown scanline filter (%d)" % filt
        out[row * stride : (row + 1) * stride] = line
        prev = line

    if depth == 16:  # keep the high byte; PDF viewers do the same visually
        out = bytearray(out[i] for i in range(0, len(out), 2))
        sample_bytes = 1
        stride = width * channels

    # Split colour from alpha; PDF carries alpha as a separate soft mask.
    alpha = None
    if color == 4:  # gray + alpha
        colour = bytearray(out[i] for i in range(0, len(out), 2))
        alpha = bytearray(out[i] for i in range(1, len(out), 2))
        space, ncomp = "/DeviceGray", 1
    elif color == 6:  # RGBA
        colour = bytearray()
        alpha = bytearray()
        for i in range(0, len(out), 4):
            colour += out[i : i + 3]
            alpha.append(out[i + 3])
        space, ncomp = "/DeviceRGB", 3
    elif color == 2:
        colour, space, ncomp = out, "/DeviceRGB", 3
    elif color == 0:
        colour, space, ncomp = out, "/DeviceGray", 1
    else:  # palette
        colour, space, ncomp = out, None, 1
        if trns:
            table = list(trns) + [255] * (256 - len(trns))
            alpha = bytearray(table[b] for b in out)

    smask = None
    if alpha is not None and min(alpha) < 255:
        smask = Image(
            width, height, zlib.compress(bytes(alpha), 9), "/FlateDecode", "/DeviceGray"
        )
    del ncomp
    return Image(
        width,
        height,
        zlib.compress(bytes(colour), 9),
        "/FlateDecode",
        space,
        8,
        smask,
        palette,
    )


def decode_image(src):
    """A document image source -> (Image, None) or (None, reason).

    JPEG rides into the PDF untouched (DCTDecode is a native PDF filter, so the
    bytes are simply re-wrapped); PNG is decoded and re-deflated. The reason
    comes back so the agent is told WHAT it needs to change about the asset,
    not merely that something was dropped.
    """
    got = decode_data_uri(src)
    if not got:
        return None, "the data: URI could not be decoded"
    mime, data = got
    if data[:3] == b"\xff\xd8\xff" or "jpeg" in mime or "jpg" in mime:
        size = _jpeg_size(data)
        if not size:
            return None, "the JPEG has no readable frame header"
        width, height, components = size
        space = {1: "/DeviceGray", 3: "/DeviceRGB", 4: "/DeviceCMYK"}.get(components)
        if space is None:
            return None, "the JPEG has %d components" % components
        return Image(width, height, data, "/DCTDecode", space), None
    if data[:8] == b"\x89PNG\r\n\x1a\n":
        result = _png_decode(data)
        if isinstance(result, str):
            return None, result
        return result, None
    kind = mime or "an unrecognised format"
    return None, "%s is not PNG or JPEG" % kind


# ── the drawing surface ──────────────────────────────────────────────────────
#
# One Canvas per page. It emits PDF operators in DOCUMENT coordinates: the page
# CTM is flipped once (`scale 0 0 -scale 0 pageHeight`) so x/y/w/h out of the
# JSON go straight through with y running down, exactly as the element boxes are
# authored. Text un-flips itself with its own matrix, which is the standard way
# to draw upright glyphs under a mirrored CTM.

_ARC_K = 0.5522847498307936  # circle -> cubic bezier control-point ratio


class Resources:
    """The resource dictionaries shared by every page of one export."""

    def __init__(self, writer):
        self.writer = writer
        self.fonts = {}
        self.xobjects = {}
        self.gstates = {}
        self.shadings = {}
        self._image_cache = {}

    def font(self, face):
        if face not in self.fonts:
            number = self.writer.add(
                "<< /Type /Font /Subtype /Type1 /BaseFont /%s "
                "/Encoding /WinAnsiEncoding >>" % face
            )
            self.fonts[face] = ("/F%d" % len(self.fonts), number)
        return self.fonts[face][0]

    def alpha(self, fill_alpha, stroke_alpha):
        key = (round(fill_alpha, 3), round(stroke_alpha, 3))
        if key not in self.gstates:
            number = self.writer.add(
                "<< /Type /ExtGState /ca %s /CA %s >>" % (num(key[0]), num(key[1]))
            )
            self.gstates[key] = ("/GS%d" % len(self.gstates), number)
        return self.gstates[key][0]

    def image(self, image, cache_key=None):
        if cache_key is not None and cache_key in self._image_cache:
            return self._image_cache[cache_key]
        entries = [
            "/Type /XObject",
            "/Subtype /Image",
            "/Width %d" % image.width,
            "/Height %d" % image.height,
            "/BitsPerComponent %d" % image.bpc,
            "/Filter %s" % image.filter,
        ]
        if image.palette is not None:
            palette = self.writer.add_stream([], image.palette, compress=False)
            entries.append(
                "/ColorSpace [/Indexed /DeviceRGB %d %d 0 R]"
                % (len(image.palette) // 3 - 1, palette)
            )
        else:
            entries.append("/ColorSpace %s" % image.colorspace)
        if image.smask is not None:
            # The soft mask is referenced by the image, not by the page, so it
            # is written as a plain object and stays out of /XObject.
            mask = image.smask
            head = (
                "<< /Type /XObject /Subtype /Image /Width %d /Height %d "
                "/BitsPerComponent 8 /ColorSpace /DeviceGray /Filter %s "
                "/Length %d >>\nstream\n"
                % (mask.width, mask.height, mask.filter, len(mask.data))
            )
            number = self.writer.add(
                head.encode("latin-1") + mask.data + b"\nendstream"
            )
            entries.append("/SMask %d 0 R" % number)
        head = "<< %s /Length %d >>\nstream\n" % (" ".join(entries), len(image.data))
        number = self.writer.add(head.encode("latin-1") + image.data + b"\nendstream")
        name = "/Im%d" % len(self.xobjects)
        self.xobjects[name] = (name, number)
        if cache_key is not None:
            self._image_cache[cache_key] = name
        return name

    def shading(self, coords, stops):
        """An axial (type 2) shading stitched from the gradient's stops."""
        stops = sorted(
            ((max(0.0, min(1.0, at)), rgb) for at, rgb in stops),
            key=lambda pair: pair[0],
        )
        if len(stops) == 1:
            stops = [(0.0, stops[0][1]), (1.0, stops[0][1])]
        functions, bounds, encode = [], [], []
        for index in range(len(stops) - 1):
            start, end = stops[index], stops[index + 1]
            functions.append(
                self.writer.add(
                    "<< /FunctionType 2 /Domain [0 1] /C0 [%s] /C1 [%s] /N 1 >>"
                    % (
                        " ".join(num(c) for c in start[1][:3]),
                        " ".join(num(c) for c in end[1][:3]),
                    )
                )
            )
            if index:
                bounds.append(stops[index][0])
            encode.append("0 1")
        if len(functions) == 1:
            combined = functions[0]
        else:
            combined = self.writer.add(
                "<< /FunctionType 3 /Domain [0 1] /Functions [%s] "
                "/Bounds [%s] /Encode [%s] >>"
                % (
                    " ".join("%d 0 R" % f for f in functions),
                    " ".join(num(b) for b in bounds),
                    " ".join(encode),
                )
            )
        number = self.writer.add(
            "<< /ShadingType 2 /ColorSpace /DeviceRGB /Coords [%s] "
            "/Function %d 0 R /Extend [true true] >>"
            % (" ".join(num(c) for c in coords), combined)
        )
        name = "/Sh%d" % len(self.shadings)
        self.shadings[name] = (name, number)
        return name

    def gradient_mask(self, coords, stops, bbox):
        """An ExtGState whose soft mask fades with the gradient's OWN alpha.

        The scrim recipe in the skill — a full-bleed photo under a
        transparent-to-dark rectangle — depends on per-stop alpha, and a PDF
        axial shading carries colour only. So the alpha channel is painted a
        second time as a greyscale shading inside a transparency group, and
        that group becomes a /Luminosity soft mask: white lets the colour
        through, black hides it. Without this the scrim renders as a solid
        block and swallows the photograph.
        """
        greys = [(at, (rgba[3], rgba[3], rgba[3])) for at, rgba in stops]
        shading = self.shading(coords, greys)
        canvas_ops = "q %s sh Q" % shading
        form = self.writer.add_stream(
            [
                "/Type /XObject",
                "/Subtype /Form",
                "/BBox [%s]" % " ".join(num(v) for v in bbox),
                "/Group << /Type /Group /S /Transparency /CS /DeviceGray >>",
                "/Resources << /Shading << %s %d 0 R >> >>"
                % (shading, self.shadings[shading][1]),
            ],
            canvas_ops.encode("latin-1"),
        )
        number = self.writer.add(
            "<< /Type /ExtGState /SMask << /S /Luminosity /G %d 0 R "
            "/BC [0] >> >>" % form
        )
        name = "/GM%d" % len(self.gstates)
        self.gstates[name] = (name, number)
        return name

    def dictionary(self):
        def group(items):
            return " ".join(
                "%s %d 0 R" % (name, number)
                for name, number in sorted(items, key=lambda pair: pair[0])
            )

        parts = ["/ProcSet [/PDF /Text /ImageB /ImageC /ImageI]"]
        parts.append("/Font << %s >>" % group(self.fonts.values()))
        if self.xobjects:
            parts.append("/XObject << %s >>" % group(self.xobjects.values()))
        if self.gstates:
            parts.append("/ExtGState << %s >>" % group(self.gstates.values()))
        if self.shadings:
            parts.append("/Shading << %s >>" % group(self.shadings.values()))
        return self.writer.add("<< %s >>" % " ".join(parts))


class Canvas:
    def __init__(self, resources, encoder):
        self.res = resources
        self.encoder = encoder
        self.ops = []
        self.base_alpha = 1.0
        self._alpha_dirty = False
        self._stack = []
        self._in_text = False
        self._text_font = None
        self._text_spacing = None

    # -- state -------------------------------------------------------------
    def op(self, text):
        self.ops.append(text)

    def save(self):
        self.op("q")
        self._stack.append((self.base_alpha, self._alpha_dirty))

    def restore(self):
        self.op("Q")
        if self._stack:
            self.base_alpha, self._alpha_dirty = self._stack.pop()

    def alpha(self, value):
        """Element opacity. Every later paint multiplies its own colour alpha
        into this, because a PDF ExtGState REPLACES /ca rather than compounding
        it — so `rgba(0,0,0,.5)` inside an element at opacity .8 has to be
        resolved to one number here, not set twice."""
        self.base_alpha = max(0.0, min(1.0, value))

    def apply_alpha(self, fill_alpha=1.0, stroke_alpha=1.0):
        fill_alpha *= self.base_alpha
        stroke_alpha *= self.base_alpha
        if fill_alpha < 0.999 or stroke_alpha < 0.999:
            self.op("%s gs" % self.res.alpha(fill_alpha, stroke_alpha))
        elif self._alpha_dirty:
            self.op("%s gs" % self.res.alpha(1.0, 1.0))
        self._alpha_dirty = fill_alpha < 0.999 or stroke_alpha < 0.999

    def fill_color(self, rgba):
        self.op("%s %s %s rg" % tuple(num(c) for c in rgba[:3]))

    def stroke_color(self, rgba):
        self.op("%s %s %s RG" % tuple(num(c) for c in rgba[:3]))

    def line_width(self, width):
        self.op("%s w" % num(width))

    def dash(self, pattern, cap=None):
        if pattern:
            self.op("[%s] 0 d" % " ".join(num(v) for v in pattern))
        else:
            self.op("[] 0 d")
        if cap is not None:
            self.op("%d J" % cap)

    def translate(self, dx, dy):
        self.op("1 0 0 1 %s %s cm" % (num(dx), num(dy)))

    def rotate(self, degrees, cx, cy):
        radians = math.radians(degrees)
        cos, sin = math.cos(radians), math.sin(radians)
        self.translate(cx, cy)
        self.op("%s %s %s %s 0 0 cm" % (num(cos), num(sin), num(-sin), num(cos)))
        self.translate(-cx, -cy)

    # -- paths -------------------------------------------------------------
    def rect_path(self, x, y, w, h, radius=0):
        radius = max(0.0, min(radius or 0.0, min(abs(w), abs(h)) / 2.0))
        if radius <= 0.01:
            self.op("%s %s %s %s re" % (num(x), num(y), num(w), num(h)))
            return
        k = radius * _ARC_K
        right, bottom = x + w, y + h
        self.op("%s %s m" % (num(x + radius), num(y)))
        self.op("%s %s l" % (num(right - radius), num(y)))
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(right - radius + k),
                num(y),
                num(right),
                num(y + radius - k),
                num(right),
                num(y + radius),
            )
        )
        self.op("%s %s l" % (num(right), num(bottom - radius)))
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(right),
                num(bottom - radius + k),
                num(right - radius + k),
                num(bottom),
                num(right - radius),
                num(bottom),
            )
        )
        self.op("%s %s l" % (num(x + radius), num(bottom)))
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(x + radius - k),
                num(bottom),
                num(x),
                num(bottom - radius + k),
                num(x),
                num(bottom - radius),
            )
        )
        self.op("%s %s l" % (num(x), num(y + radius)))
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(x),
                num(y + radius - k),
                num(x + radius - k),
                num(y),
                num(x + radius),
                num(y),
            )
        )
        self.op("h")

    def ellipse_path(self, cx, cy, rx, ry):
        kx, ky = rx * _ARC_K, ry * _ARC_K
        self.op("%s %s m" % (num(cx + rx), num(cy)))
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(cx + rx),
                num(cy + ky),
                num(cx + kx),
                num(cy + ry),
                num(cx),
                num(cy + ry),
            )
        )
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(cx - kx),
                num(cy + ry),
                num(cx - rx),
                num(cy + ky),
                num(cx - rx),
                num(cy),
            )
        )
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(cx - rx),
                num(cy - ky),
                num(cx - kx),
                num(cy - ry),
                num(cx),
                num(cy - ry),
            )
        )
        self.op(
            "%s %s %s %s %s %s c"
            % (
                num(cx + kx),
                num(cy - ry),
                num(cx + rx),
                num(cy - ky),
                num(cx + rx),
                num(cy),
            )
        )
        self.op("h")

    def polygon_path(self, points):
        for index, (px, py) in enumerate(points):
            self.op("%s %s %s" % (num(px), num(py), "m" if not index else "l"))
        self.op("h")

    def line_path(self, x1, y1, x2, y2):
        self.op("%s %s m %s %s l" % (num(x1), num(y1), num(x2), num(y2)))

    def paint(
        self, fill=None, stroke=None, width=1.0, dash=None, cap=None, even_odd=False
    ):
        """Close out the current path with the right painting operator."""
        self.apply_alpha(
            fill[3] if is_visible(fill) else 1.0,
            stroke[3] if is_visible(stroke) else 1.0,
        )
        if is_visible(fill):
            self.fill_color(fill)
        if is_visible(stroke) and width > 0:
            self.stroke_color(stroke)
            self.line_width(width)
            self.dash(dash, cap)
        if is_visible(fill) and is_visible(stroke) and width > 0:
            self.op("B*" if even_odd else "B")
        elif is_visible(fill):
            self.op("f*" if even_odd else "f")
        elif is_visible(stroke) and width > 0:
            self.op("S")
        else:
            self.op("n")

    def clip(self, even_odd=False):
        self.op("W* n" if even_odd else "W n")

    # -- images ------------------------------------------------------------
    def draw_image(self, name, x, y, w, h):
        # The page CTM is y-down, so an image (drawn in a y-up unit square)
        # needs its own flip to land upright.
        self.save()
        self.apply_alpha()
        self.op("%s 0 0 %s %s %s cm" % (num(w), num(-h), num(x), num(y + h)))
        self.op("%s Do" % name)
        self.restore()

    def shade(self, name):
        self.op("%s sh" % name)

    # -- text --------------------------------------------------------------
    #
    # One BT/ET per element, not per span. That is a correctness requirement,
    # not a tidiness one: with text render mode 7 (add to clip) the clip is
    # applied at ET, and a second ET INTERSECTS it — so a gradient headline
    # drawn as one BT/ET per word clips itself away to nothing.
    def begin_text(self, render_mode=0):
        self.op("BT")
        if render_mode:
            self.op("%d Tr" % render_mode)
        self._in_text = True
        self._text_font = None
        self._text_spacing = None

    def end_text(self):
        self.op("ET")
        self._in_text = False

    def show_text(self, x, y, text, face, size, letter_spacing=0.0):
        raw = self.encoder.encode(text)
        if not raw:
            return
        standalone = not self._in_text
        if standalone:
            self.begin_text()
        name = self.res.font(face)
        if self._text_spacing != letter_spacing:
            self.op("%s Tc" % num(letter_spacing))
            self._text_spacing = letter_spacing
        if self._text_font != (name, size):
            self.op("%s %s Tf" % (name, num(size)))
            self._text_font = (name, size)
        self.op("1 0 0 -1 %s %s Tm" % (num(x), num(y)))
        self.op("%s Tj" % pdf_string(raw).decode("latin-1"))
        if standalone:
            self.end_text()

    def content(self):
        return ("\n".join(self.ops) + "\n").encode("latin-1")


# ── dynamic fields ───────────────────────────────────────────────────────────

_TOKEN = re.compile(
    r"\{\{\s*(page|pages|title|date|time|author|company|subject|event)"
    r"(?::([^}]*))?\s*\}\}",
    re.I,
)


def visible_slides(doc):
    """The slides a PDF contains, in order.

    The app's own export filters with `!slide.stateOf && !slide.hidden`: a state
    is a click-only variant of another slide and a hidden slide is appendix
    material. Matching it exactly is why page numbering agrees between the two
    exports.
    """
    slides = doc.get("slides")
    if not isinstance(slides, list):
        return []
    return [
        s
        for s in slides
        if isinstance(s, dict) and not s.get("stateOf") and not s.get("hidden")
    ]


def build_fields(doc, index, total, now):
    meta = doc.get("meta") if isinstance(doc.get("meta"), dict) else {}
    return {
        "page": index,
        "pages": total,
        "title": str(doc.get("title") or ""),
        # The app resolves {{date}}/{{time}} through the READER's locale at open
        # time; a PDF is a snapshot, so they are resolved here, at export time,
        # in the format Chromium's default locale produces — which is what the
        # author saw in the deck.
        "date": "%d/%d/%d" % (now.tm_mon, now.tm_mday, now.tm_year),
        "time": time.strftime("%I:%M %p", now),
        "author": str(meta.get("author") or ""),
        "company": str(meta.get("company") or ""),
        "subject": str(meta.get("subject") or ""),
        "event": str(meta.get("event") or ""),
    }


def resolve_fields(html, fields):
    if not fields or "{{" not in html:
        return html

    def one(match):
        key = match.group(1).lower()
        pad = match.group(2)
        value = fields.get(key, "")
        if key in ("page", "pages"):
            try:
                width = int(pad or "")
            except ValueError:
                width = 0
            return str(value).zfill(width) if width > 0 else str(value)
        return str(value)

    return _TOKEN.sub(one, html)


# ── the renderer ─────────────────────────────────────────────────────────────

_VALIGN = {"top": 0.0, "middle": 0.5, "bottom": 1.0}
_ALIGN = {"left": 0.0, "center": 0.5, "right": 1.0}


def number(value, default=0.0):
    return (
        float(value) if isinstance(value, (int, float)) and value == value else default
    )


class Renderer:
    def __init__(self, doc):
        if not isinstance(doc, dict):
            raise PdfError("document is not an object")
        size = doc.get("size")
        if not isinstance(size, dict):
            raise PdfError("document has no size; it is not a Bento document")
        self.doc = doc
        self.width = number(size.get("width"), 1280.0) or 1280.0
        self.height = number(size.get("height"), 720.0) or 720.0
        theme = doc.get("theme") if isinstance(doc.get("theme"), dict) else {}
        self.theme = theme
        self.theme_font = theme.get("fontFamily") or "sans-serif"
        self.theme_color = parse_color(theme.get("color"), (0.1, 0.1, 0.1, 1.0))
        self.theme_bg = parse_color(theme.get("background"), (1, 1, 1, 1.0))
        self.assets = doc.get("assets") if isinstance(doc.get("assets"), dict) else {}
        self.writer = PdfWriter()
        self.res = Resources(self.writer)
        self.encoder = TextEncoder()
        self.warnings = []
        self._warned = set()
        self.scale = PAGE_WIDTH_PT / self.width
        self.page_width = PAGE_WIDTH_PT
        self.page_height = self.height * self.scale

    def warn(self, message, once_key=None):
        key = once_key or message
        if key in self._warned:
            return
        self._warned.add(key)
        self.warnings.append(message)

    # -- assets ------------------------------------------------------------
    def source(self, src):
        """Resolve `asset:<key>` against doc.assets, like the app does."""
        if isinstance(src, str) and src.startswith("asset:"):
            return self.assets.get(src[6:], "")
        return src or ""

    def image_for(self, src):
        resolved = self.source(src)
        if not isinstance(resolved, str) or not resolved:
            return None
        if not resolved.startswith("data:"):
            self.warn(
                "image sources that are not embedded (%s...) were left "
                "out: a PDF has no network, so only data: URIs and "
                "doc.assets entries can be drawn." % resolved[:32],
                "remote-image",
            )
            return None
        image, reason = decode_image(resolved)
        if image is None:
            self.warn(
                "an embedded image was skipped because %s. The built-in "
                "export draws PNG (non-interlaced) and JPEG; re-embed it "
                "in one of those, or use the deck's own PDF export." % reason,
                "image-%s" % reason,
            )
            return None
        key = resolved if len(resolved) < 4096 else None
        name = self.res.image(image, cache_key=key)
        return name, image.width, image.height

    # -- document ----------------------------------------------------------
    def render(self):
        slides = visible_slides(self.doc)
        if not slides:
            raise PdfError(
                "the document has no printable slides (every slide "
                "is hidden or a state variant)"
            )
        now = time.localtime()
        pages = []
        contents = []
        for index, slide in enumerate(slides, start=1):
            fields = build_fields(self.doc, index, len(slides), now)
            canvas = Canvas(self.res, self.encoder)
            self.render_slide(canvas, slide, fields)
            contents.append(self.writer.add_stream([], canvas.content()))
            pages.append(self.writer.reserve())
        resources = self.res.dictionary()
        tree = self.writer.reserve()
        for number_, content in zip(pages, contents, strict=True):
            self.writer.put(
                number_,
                "<< /Type /Page /Parent %d 0 R /MediaBox "
                "[0 0 %s %s] /Resources %d 0 R /Contents %d 0 R >>"
                % (
                    tree,
                    num(self.page_width),
                    num(self.page_height),
                    resources,
                    content,
                ),
            )
        self.writer.put(
            tree,
            "<< /Type /Pages /Count %d /Kids [%s] >>"
            % (len(pages), " ".join("%d 0 R" % p for p in pages)),
        )
        root = self.writer.add("<< /Type /Catalog /Pages %d 0 R >>" % tree)
        title = self.encoder.encode(str(self.doc.get("title") or "Bento deck"))
        info = self.writer.add(
            "<< /Title %s /Producer (fleet bento-slides skill) "
            "/CreationDate (D:%s) >>"
            % (pdf_string(title).decode("latin-1"), time.strftime("%Y%m%d%H%M%S", now))
        )
        note = self.encoder.warning()
        if note:
            self.warn(note, "encoding")
        return self.writer.serialize(root, info)

    def render_slide(self, canvas, slide, fields):
        canvas.save()
        canvas.op(
            "%s 0 0 %s 0 %s cm"
            % (num(self.scale), num(-self.scale), num(self.page_height))
        )
        background = parse_color(slide.get("background"), self.theme_bg)
        if is_visible(background):
            canvas.fill_color(background)
            canvas.op("0 0 %s %s re f" % (num(self.width), num(self.height)))
        elements = slide.get("elements")
        for element in elements if isinstance(elements, list) else []:
            if not isinstance(element, dict):
                continue
            try:
                self.render_element(canvas, element, fields)
            except PdfError:
                raise
            except Exception as exc:  # a bad element must not lose the deck
                self.warn(
                    "element %r (%s) could not be drawn: %s"
                    % (element.get("id"), element.get("type"), exc),
                    "element-%s" % element.get("id"),
                )
        canvas.restore()

    def render_element(self, canvas, element, fields):
        kind = element.get("type")
        x = number(element.get("x"))
        y = number(element.get("y"))
        w = number(element.get("w"))
        h = number(element.get("h"))
        opacity = element.get("opacity")
        opacity = (
            1.0
            if not isinstance(opacity, (int, float))
            else max(0.0, min(1.0, float(opacity)))
        )
        if opacity <= 0.001:
            return
        rotation = number(element.get("rotation"))
        canvas.save()
        if rotation:
            canvas.rotate(rotation, x + w / 2.0, y + h / 2.0)
        canvas.alpha(opacity)
        if (
            element.get("blur")
            or element.get("shadow")
            or element.get("blend")
            or element.get("backdropFilter")
        ):
            self.warn(
                "blur, drop shadow, blend and backdrop-filter effects "
                "are not reproduced by the built-in export; the shapes "
                "and text are drawn without them.",
                "filters",
            )
        box = (x, y, w, h)
        if kind == "text":
            self.render_text(canvas, element, box, fields)
        elif kind == "shape":
            self.render_shape(canvas, element, box)
        elif kind == "image":
            self.render_image(canvas, element, box)
        elif kind == "table":
            self.render_table(canvas, element, box)
        elif kind == "chart":
            self.render_chart(canvas, element, box)
        elif kind == "media":
            self.render_media(canvas, element, box)
        elif kind == "svg":
            self.warn(
                "an `svg` element was skipped: the built-in export has "
                "no SVG renderer. Compose the artwork from shape "
                "elements, or use the deck's own PDF export.",
                "svg",
            )
        else:
            self.warn("unknown element type %r was skipped." % kind, "type-%s" % kind)
        canvas.restore()

    # -- text --------------------------------------------------------------
    def render_text(self, canvas, element, box, fields):
        x, y, w, h = box
        html = resolve_fields(str(element.get("html") or ""), fields)
        runs = parse_inline(html)
        if not "".join(run.text for run in runs).strip():
            return  # print hides placeholders, and empty text draws nothing
        size = number(element.get("fontSize"), 24.0) or 24.0
        line_height = element.get("lineHeight")
        line_height = (
            float(line_height)
            if isinstance(line_height, (int, float)) and line_height > 0
            else 1.2
        )
        family = family_of(element.get("fontFamily") or self.theme_font)
        letter_spacing = number(element.get("letterSpacing"))
        lines = layout_text(
            self.encoder,
            runs,
            w,
            family,
            size,
            element.get("fontWeight"),
            letter_spacing,
        )
        ascent, descent = VMETRICS[family]
        step = size * line_height
        block = step * len(lines)
        offset = _VALIGN.get(element.get("valign"), 0.0)
        top = y + (h - block) * offset
        align = _ALIGN.get(element.get("align"), 0.0)
        half_leading = (step - size * (ascent + descent)) / 2.0

        gradient = element.get("colorGradient")
        stops = self.gradient_stops(gradient)
        stroke = element.get("textStroke")
        color = parse_color(element.get("color"), self.theme_color)
        mode = 0
        if stops:
            # Paint the gradient THROUGH the glyphs: add them to the clip path
            # and then flood the box. The browser does the same thing with
            # background-clip:text.
            canvas.save()
            mode = 7
        elif (
            isinstance(stroke, dict)
            and number(stroke.get("width")) > 0
            and stroke.get("fill") == "none"
        ):
            color = parse_color(stroke.get("color"), color)
        if not is_visible(color) and not stops:
            return
        if not stops:
            canvas.apply_alpha(color[3])
            canvas.fill_color(color)
        canvas.begin_text(mode)
        for index, line in enumerate(lines):
            baseline = top + index * step + half_leading + ascent * size
            start = x + (w - line.width) * align
            for piece in line.pieces:
                if not piece.text.strip():
                    continue
                canvas.show_text(
                    start + piece.x,
                    baseline,
                    piece.text,
                    piece.face,
                    piece.size,
                    letter_spacing,
                )
        canvas.end_text()
        if stops:
            # `ET` turns the accumulated glyph outlines (text render mode 7)
            # into the clip path, so the shading below paints THROUGH the
            # letters — the PDF equivalent of background-clip:text.
            self.paint_gradient(
                canvas,
                gradient,
                stops,
                self.gradient_coords(gradient, box),
                (x, y, x + w, y + h),
            )
            canvas.restore()

    def paint_gradient(self, canvas, gradient, stops, coords, bbox):
        """Flood the current clip with a gradient, alpha included."""
        if any(rgba[3] < 0.999 for _, rgba in stops):
            canvas.op("%s gs" % self.res.gradient_mask(coords, stops, bbox))
        else:
            canvas.apply_alpha()
        canvas.shade(self.res.shading(coords, stops))

    # -- gradients ---------------------------------------------------------
    def gradient_stops(self, gradient):
        if not isinstance(gradient, dict):
            return None
        stops = gradient.get("stops")
        if not isinstance(stops, list) or not stops:
            return None
        out = []
        for stop in stops:
            if not isinstance(stop, dict):
                continue
            rgba = parse_color(stop.get("color"))
            out.append((number(stop.get("at")), rgba))
        return out or None

    def gradient_coords(self, gradient, box):
        """The gradient line, using the app's own angle convention.

        `pd(angle)` in the runtime: 0deg points up, angles run clockwise, and
        the line is expressed in fractions of the element box — so a wide box
        skews the gradient exactly as the SVG one does.
        """
        x, y, w, h = box
        angle = gradient.get("angle")
        angle = 180.0 if not isinstance(angle, (int, float)) else float(angle)
        radians = math.radians(angle)
        dx = math.sin(radians) / 2.0
        dy = -math.cos(radians) / 2.0
        return (
            x + (0.5 - dx) * w,
            y + (0.5 - dy) * h,
            x + (0.5 + dx) * w,
            y + (0.5 + dy) * h,
        )


# ── SVG path data ────────────────────────────────────────────────────────────
#
# A `shape: "path"` element carries SVG path data plus an authoring `pathBox`
# viewBox that is stretched into the element box. Parsing the data and mapping
# the POINTS (rather than scaling the coordinate system) is what keeps the
# stroke width uniform, which is what the app's `vector-effect:
# non-scaling-stroke` does in the browser.

_PATH_TOKEN = re.compile(
    r"([MmLlHhVvCcSsQqTtAaZz])|(-?[0-9]*\.?[0-9]+(?:[eE][-+]?[0-9]+)?)"
)


def parse_path(data):
    """SVG path data -> [("m"|"l"|"c", points...)] in absolute coordinates."""
    tokens = []
    for match in _PATH_TOKEN.finditer(data or ""):
        tokens.append(match.group(1) or float(match.group(2)))
    out = []
    index = 0
    command = None
    cx = cy = 0.0
    start_x = start_y = 0.0
    last_control = None

    def take(count):
        nonlocal index
        values = []
        while len(values) < count:
            if index >= len(tokens) or isinstance(tokens[index], str):
                raise ValueError("truncated path data")
            values.append(tokens[index])
            index += 1
        return values

    while index < len(tokens):
        if isinstance(tokens[index], str):
            command = tokens[index]
            index += 1
            if command in "Zz":
                out.append(("z",))
                cx, cy = start_x, start_y
                continue
        if command is None:
            break
        lower = command.lower()
        relative = command.islower()
        try:
            if lower == "m":
                px, py = take(2)
                if relative:
                    px, py = cx + px, cy + py
                out.append(("m", (px, py)))
                cx, cy = px, py
                start_x, start_y = px, py
                command = "l" if command == "m" else "L"
            elif lower == "l":
                px, py = take(2)
                if relative:
                    px, py = cx + px, cy + py
                out.append(("l", (px, py)))
                cx, cy = px, py
            elif lower == "h":
                (px,) = take(1)
                px = cx + px if relative else px
                out.append(("l", (px, cy)))
                cx = px
            elif lower == "v":
                (py,) = take(1)
                py = cy + py if relative else py
                out.append(("l", (cx, py)))
                cy = py
            elif lower == "c":
                x1, y1, x2, y2, px, py = take(6)
                if relative:
                    x1, y1, x2, y2, px, py = (
                        cx + x1,
                        cy + y1,
                        cx + x2,
                        cy + y2,
                        cx + px,
                        cy + py,
                    )
                out.append(("c", (x1, y1), (x2, y2), (px, py)))
                last_control = (x2, y2)
                cx, cy = px, py
            elif lower == "s":
                x2, y2, px, py = take(4)
                if relative:
                    x2, y2, px, py = cx + x2, cy + y2, cx + px, cy + py
                if last_control is None:
                    x1, y1 = cx, cy
                else:
                    x1, y1 = 2 * cx - last_control[0], 2 * cy - last_control[1]
                out.append(("c", (x1, y1), (x2, y2), (px, py)))
                last_control = (x2, y2)
                cx, cy = px, py
            elif lower in ("q", "t"):
                if lower == "q":
                    qx, qy, px, py = take(4)
                    if relative:
                        qx, qy, px, py = cx + qx, cy + qy, cx + px, cy + py
                else:
                    px, py = take(2)
                    if relative:
                        px, py = cx + px, cy + py
                    if last_control is None:
                        qx, qy = cx, cy
                    else:
                        qx, qy = 2 * cx - last_control[0], 2 * cy - last_control[1]
                out.append(
                    (
                        "c",
                        (cx + 2.0 / 3 * (qx - cx), cy + 2.0 / 3 * (qy - cy)),
                        (px + 2.0 / 3 * (qx - px), py + 2.0 / 3 * (qy - py)),
                        (px, py),
                    )
                )
                last_control = (qx, qy)
                cx, cy = px, py
            elif lower == "a":
                rx, ry, rot, large, sweep, px, py = take(7)
                if relative:
                    px, py = cx + px, cy + py
                out.extend(_arc_to_beziers(cx, cy, rx, ry, rot, large, sweep, px, py))
                cx, cy = px, py
            else:
                break
        except ValueError:
            break
        if lower not in ("c", "s", "q", "t"):
            last_control = None
    return out


def _arc_to_beziers(x1, y1, rx, ry, rotation, large, sweep, x2, y2):
    """SVG elliptical arc -> cubic segments (endpoint parameterisation)."""
    if rx == 0 or ry == 0 or (x1 == x2 and y1 == y2):
        return [("l", (x2, y2))]
    rx, ry = abs(rx), abs(ry)
    phi = math.radians(rotation)
    cos_phi, sin_phi = math.cos(phi), math.sin(phi)
    dx2, dy2 = (x1 - x2) / 2.0, (y1 - y2) / 2.0
    x1p = cos_phi * dx2 + sin_phi * dy2
    y1p = -sin_phi * dx2 + cos_phi * dy2
    lam = x1p * x1p / (rx * rx) + y1p * y1p / (ry * ry)
    if lam > 1:
        scale = math.sqrt(lam)
        rx, ry = rx * scale, ry * scale
    denom = rx * rx * y1p * y1p + ry * ry * x1p * x1p
    factor = 0.0 if denom == 0 else max(0.0, (rx * rx * ry * ry - denom) / denom)
    coef = math.sqrt(factor) * (-1 if bool(large) == bool(sweep) else 1)
    cxp = coef * rx * y1p / ry
    cyp = -coef * ry * x1p / rx
    cx = cos_phi * cxp - sin_phi * cyp + (x1 + x2) / 2.0
    cy = sin_phi * cxp + cos_phi * cyp + (y1 + y2) / 2.0

    def angle_of(ux, uy):
        return math.atan2(uy, ux)

    theta1 = angle_of((x1p - cxp) / rx, (y1p - cyp) / ry)
    theta2 = angle_of((-x1p - cxp) / rx, (-y1p - cyp) / ry)
    delta = theta2 - theta1
    if not sweep and delta > 0:
        delta -= 2 * math.pi
    elif sweep and delta < 0:
        delta += 2 * math.pi
    segments = max(1, int(math.ceil(abs(delta) / (math.pi / 2))))
    out = []
    step = delta / segments
    k = 4.0 / 3.0 * math.tan(step / 4.0)
    theta = theta1
    for _ in range(segments):
        cos1, sin1 = math.cos(theta), math.sin(theta)
        cos2, sin2 = math.cos(theta + step), math.sin(theta + step)

        def point(cos_t, sin_t):
            return (
                cx + rx * cos_t * cos_phi - ry * sin_t * sin_phi,
                cy + rx * cos_t * sin_phi + ry * sin_t * cos_phi,
            )

        px1, py1 = point(cos1, sin1)
        px2, py2 = point(cos2, sin2)
        dx1 = -rx * sin1 * cos_phi - ry * cos1 * sin_phi
        dy1 = -rx * sin1 * sin_phi + ry * cos1 * cos_phi
        dx2 = -rx * sin2 * cos_phi - ry * cos2 * sin_phi
        dy2 = -rx * sin2 * sin_phi + ry * cos2 * cos_phi
        out.append(
            (
                "c",
                (px1 + k * dx1, py1 + k * dy1),
                (px2 - k * dx2, py2 - k * dy2),
                (px2, py2),
            )
        )
        theta += step
    return out


# ── shape, image, table and media elements ───────────────────────────────────


def _dash_pattern(element, width):
    """The app's stroke-dasharray, in the same units."""
    style = element.get("strokeStyle")
    if style == "dashed":
        return [max(width * 2.4, 7), max(width * 1.8, 5)], 0
    if style == "dotted":
        return [0.1, max(width * 2.2, 5)], 1
    dash = element.get("strokeDash")
    if style not in (None, "solid") and isinstance(dash, (int, float)) and dash > 0:
        return [dash, dash], None
    return None, None


class ShapeMixin:
    def render_shape(self, canvas, element, box):
        x, y, w, h = box
        shape = element.get("shape") or "rect"
        stroke_width = number(element.get("strokeWidth"))
        inset = stroke_width / 2.0
        fill = parse_color(element.get("fill"))
        stops = self.gradient_stops(element.get("fillGradient"))
        stroke = parse_color(element.get("stroke"))
        dash, cap = _dash_pattern(element, stroke_width)

        if shape == "line":
            self.render_line(canvas, element, box)
            return

        canvas.save()
        canvas.translate(x, y)
        if shape == "rect":
            canvas.rect_path(
                inset,
                inset,
                max(w - stroke_width, 0),
                max(h - stroke_width, 0),
                number(element.get("radius")),
            )
        elif shape == "ellipse":
            canvas.ellipse_path(
                w / 2.0, h / 2.0, max(w / 2.0 - inset, 0), max(h / 2.0 - inset, 0)
            )
        elif shape == "triangle":
            canvas.polygon_path(
                [(w / 2.0, inset), (w - inset, h - inset), (inset, h - inset)]
            )
        elif shape == "arrow":
            shaft = h * 0.44
            head = min(w * 0.38, h)
            top = (h - shaft) / 2.0
            canvas.polygon_path(
                [
                    (0, top),
                    (w - head, top),
                    (w - head, 0),
                    (w, h / 2.0),
                    (w - head, h),
                    (w - head, top + shaft),
                    (0, top + shaft),
                ]
            )
        elif shape == "path":
            self.emit_path(canvas, element, w, h)
        else:
            self.warn("unknown shape %r was skipped." % shape, "shape-%s" % shape)
            canvas.restore()
            return

        if stops:
            canvas.save()
            canvas.clip()
            self.paint_gradient(
                canvas,
                element.get("fillGradient"),
                stops,
                self.gradient_coords(element.get("fillGradient"), (0, 0, w, h)),
                (0, 0, w, h),
            )
            canvas.restore()
            if is_visible(stroke) and stroke_width > 0:
                # The fill is painted by the shading, so re-lay the outline for
                # the stroke pass rather than trying to keep the clipped path.
                self.render_shape_outline(
                    canvas, element, box, stroke, stroke_width, dash, cap
                )
        else:
            canvas.paint(
                fill=fill, stroke=stroke, width=stroke_width, dash=dash, cap=cap
            )
        canvas.restore()

    def render_shape_outline(self, canvas, element, box, stroke, width, dash, cap):
        _, _, w, h = box
        inset = width / 2.0
        shape = element.get("shape") or "rect"
        if shape == "rect":
            canvas.rect_path(
                inset,
                inset,
                max(w - width, 0),
                max(h - width, 0),
                number(element.get("radius")),
            )
        elif shape == "ellipse":
            canvas.ellipse_path(
                w / 2.0, h / 2.0, max(w / 2.0 - inset, 0), max(h / 2.0 - inset, 0)
            )
        elif shape == "triangle":
            canvas.polygon_path(
                [(w / 2.0, inset), (w - inset, h - inset), (inset, h - inset)]
            )
        elif shape == "path":
            self.emit_path(canvas, element, w, h)
        else:
            return
        canvas.paint(stroke=stroke, width=width, dash=dash, cap=cap)

    def emit_path(self, canvas, element, w, h):
        """Draw `d`, mapping the authoring `pathBox` onto the element box."""
        segments = parse_path(element.get("d"))
        if not segments:
            return
        pbox = element.get("pathBox")
        if (
            isinstance(pbox, list)
            and len(pbox) == 4
            and all(isinstance(v, (int, float)) for v in pbox)
        ):
            vx, vy, vw, vh = (float(v) for v in pbox)
        else:
            vx, vy, vw, vh = 0.0, 0.0, w or 1.0, h or 1.0
        sx = w / vw if vw else 1.0
        sy = h / vh if vh else 1.0

        def point(pair):
            return ((pair[0] - vx) * sx, (pair[1] - vy) * sy)

        for segment in segments:
            if segment[0] == "m":
                px, py = point(segment[1])
                canvas.op("%s %s m" % (num(px), num(py)))
            elif segment[0] == "l":
                px, py = point(segment[1])
                canvas.op("%s %s l" % (num(px), num(py)))
            elif segment[0] == "c":
                (a, b), (c, d), (e, f) = (point(p) for p in segment[1:])
                canvas.op(
                    "%s %s %s %s %s %s c"
                    % (num(a), num(b), num(c), num(d), num(e), num(f))
                )
            elif segment[0] == "z":
                canvas.op("h")

    def render_line(self, canvas, element, box):
        """A `line` shape: a horizontal rule across the box, plus its tips."""
        x, y, w, h = box
        width = max(number(element.get("strokeWidth")), 2.0)
        color = parse_color(element.get("fill"))
        if not is_visible(color):
            return

        def tip(kind):
            return width * 2.6 if kind and kind != "none" else 0.0

        start = tip(element.get("lineStart"))
        end = tip(element.get("lineEnd"))
        mid = y + h / 2.0
        dash, _ = _dash_pattern(element, width)
        canvas.save()
        canvas.line_path(x + start, mid, x + w - end, mid)
        canvas.paint(
            stroke=color,
            width=width,
            dash=dash,
            cap=0 if element.get("strokeStyle") == "dashed" else 1,
        )
        for kind, at_start in (
            (element.get("lineStart"), True),
            (element.get("lineEnd"), False),
        ):
            if kind and kind != "none":
                self.render_tip(
                    canvas,
                    kind,
                    color,
                    width,
                    x + (start if at_start else w - end),
                    mid,
                    at_start,
                )
        canvas.restore()

    def render_tip(self, canvas, kind, color, width, at_x, at_y, reversed_):
        """Match the app's markers: an 8x8 viewBox scaled by 5.5 stroke units."""
        unit = 5.5 / 8.0 * width
        direction = -1.0 if reversed_ else 1.0
        if kind == "arrow":
            ref = 6.4 * unit
            tip_x = at_x + direction * (7.6 * unit - ref)
            back_x = at_x - direction * ref
            canvas.polygon_path(
                [
                    (back_x, at_y - (4 - 0.4) * unit),
                    (tip_x, at_y),
                    (back_x, at_y + (7.6 - 4) * unit),
                ]
            )
        elif kind == "dot":
            canvas.ellipse_path(at_x, at_y, 2.6 * unit, 2.6 * unit)
        else:  # bar
            canvas.rect_path(
                at_x - 0.8 * unit, at_y - 3.6 * unit, 1.6 * unit, 7.2 * unit
            )
        canvas.paint(fill=color)

    def render_image(self, canvas, element, box):
        x, y, w, h = box
        got = self.image_for(element.get("src"))
        if not got:
            return
        name, natural_w, natural_h = got
        radius = number(element.get("radius"))
        fit = element.get("fit") or "cover"
        canvas.save()
        if radius > 0 or fit == "cover":
            canvas.rect_path(x, y, w, h, radius)
            canvas.clip()
        if fit in ("cover", "contain") and natural_w and natural_h:
            scale = (
                max(w / natural_w, h / natural_h)
                if fit == "cover"
                else min(w / natural_w, h / natural_h)
            )
            draw_w, draw_h = natural_w * scale, natural_h * scale
            canvas.draw_image(
                name, x + (w - draw_w) / 2.0, y + (h - draw_h) / 2.0, draw_w, draw_h
            )
        else:
            canvas.draw_image(name, x, y, w, h)
        canvas.restore()

    def render_media(self, canvas, element, box):
        """The app's print path draws media as a neutral poster block; so do we."""
        x, y, w, h = box
        kind = element.get("kind")
        radius = number(element.get("radius"))
        backdrop = (
            (0.043, 0.059, 0.078, 1.0)
            if kind == "video"
            else (0.906, 0.929, 0.957, 1.0)
        )
        canvas.save()
        canvas.rect_path(x, y, w, h, radius)
        canvas.paint(fill=backdrop)
        poster = element.get("poster") if kind == "video" else None
        got = self.image_for(poster) if poster else None
        if got:
            canvas.save()
            canvas.rect_path(x, y, w, h, radius)
            canvas.clip()
            canvas.draw_image(got[0], x, y, w, h)
            canvas.restore()
        else:
            glyph = (1, 1, 1, 1.0) if kind == "video" else (0.369, 0.463, 0.6, 1)
            size = min(w, h) * 0.18
            cx, cy = x + w / 2.0, y + h / 2.0
            if kind == "video":
                canvas.polygon_path(
                    [
                        (cx - size * 0.4, cy - size * 0.55),
                        (cx + size * 0.6, cy),
                        (cx - size * 0.4, cy + size * 0.55),
                    ]
                )
                canvas.paint(fill=glyph)
            else:
                canvas.ellipse_path(
                    cx - size * 0.25, cy + size * 0.35, size * 0.28, size * 0.22
                )
                canvas.paint(fill=glyph)
                canvas.rect_path(cx, cy - size * 0.6, size * 0.12, size * 0.95)
                canvas.paint(fill=glyph)
            self.warn(
                "video and audio elements are drawn as a poster block — "
                "a PDF cannot play media.",
                "media",
            )
        canvas.restore()


class TableMixin:
    """A `table` element: `table-layout: fixed`, 100% width, 100% height.

    Column widths come from the fractional `columns[].w` weights. Row heights
    are the interesting part: the app gives the table `height: 100%`, so a
    browser gives every row its natural height and then shares the leftover
    space out. Doing the same here is what keeps a two-row table from looking
    top-heavy in the PDF.
    """

    def render_table(self, canvas, element, box):
        x, y, w, h = box
        columns = element.get("columns")
        rows = element.get("rows")
        if (
            not isinstance(columns, list)
            or not isinstance(rows, list)
            or not columns
            or not rows
        ):
            return
        style = element.get("style") if isinstance(element.get("style"), dict) else {}
        weights = [
            max(0.0, number(c.get("w"))) if isinstance(c, dict) else 0.0
            for c in columns
        ]
        total_weight = sum(weights) or 1.0
        widths = [w * weight / total_weight for weight in weights]
        header = bool(element.get("header"))
        font_size = number(style.get("fontSize"), 16.0) or 16.0
        family = family_of(style.get("fontFamily") or self.theme_font)
        pad_x = number(style.get("cellPadX"))
        pad_y = number(style.get("cellPadY"))
        border_width = number(style.get("borderWidth"))
        border = parse_color(style.get("borderColor"))
        radius = number(style.get("radius"))
        line_height = 1.3
        ascent, descent = VMETRICS[family]

        # Lay every cell out once: the wrap decides the row's natural height.
        grid = []
        natural = []
        for row_index, row in enumerate(rows):
            cells = row.get("cells") if isinstance(row, dict) else None
            cells = cells if isinstance(cells, list) else []
            is_header = header and row_index == 0
            laid = []
            tallest = font_size * line_height + pad_y * 2
            for column_index in range(len(widths)):
                cell = cells[column_index] if column_index < len(cells) else {}
                cell = cell if isinstance(cell, dict) else {}
                bold = bool(cell.get("bold")) or is_header
                inner = max(widths[column_index] - pad_x * 2 - border_width * 2, 1.0)
                lines = layout_text(
                    self.encoder,
                    parse_inline(str(cell.get("html") or "")),
                    inner,
                    family,
                    font_size,
                    700 if bold else 400,
                )
                laid.append((cell, lines, bold))
                tallest = max(tallest, len(lines) * font_size * line_height + pad_y * 2)
            grid.append(laid)
            natural.append(tallest)

        used = sum(natural)
        if used < h and used > 0:
            extra = (h - used) / len(natural)
            heights = [value + extra for value in natural]
        else:
            heights = natural

        canvas.save()
        if radius > 0:
            canvas.rect_path(x, y, w, h, radius)
            canvas.clip()
        top = y
        for row_index, laid in enumerate(grid):
            is_header = header and row_index == 0
            body_index = row_index - 1 if header else row_index
            zebra = None
            if not is_header and style.get("zebra") and body_index % 2 == 1:
                zebra = style.get("zebra")
            left = x
            for column_index, (cell, lines, _bold) in enumerate(laid):
                width = widths[column_index]
                height = heights[row_index]
                background = cell.get("bg") or (
                    style.get("headerBg") if is_header else zebra
                )
                fill = parse_color(background, (0, 0, 0, 0.0))
                if is_visible(fill) or (is_visible(border) and border_width):
                    canvas.rect_path(left, top, width, height)
                    canvas.paint(fill=fill, stroke=border, width=border_width)
                color = cell.get("color") or (
                    style.get("headerColor") if is_header else style.get("color")
                )
                text_color = parse_color(color, self.theme_color)
                if not is_visible(text_color) or not lines:
                    left += width
                    continue
                canvas.apply_alpha(text_color[3])
                canvas.fill_color(text_color)
                align = _ALIGN.get(cell.get("align"), 0.0)
                step = font_size * line_height
                block = step * len(lines)
                cell_top = top + (height - block) / 2.0  # vertical-align:middle
                inner_left = left + pad_x + border_width
                inner_width = max(width - pad_x * 2 - border_width * 2, 1.0)
                half_leading = (step - font_size * (ascent + descent)) / 2.0
                for line_index, line in enumerate(lines):
                    baseline = (
                        cell_top + line_index * step + half_leading + ascent * font_size
                    )
                    start = inner_left + (inner_width - line.width) * align
                    for piece in line.pieces:
                        if piece.text.strip():
                            canvas.show_text(
                                start + piece.x,
                                baseline,
                                piece.text,
                                piece.face,
                                piece.size,
                            )
                left += width
            top += heights[row_index]
        canvas.restore()


# ── charts ───────────────────────────────────────────────────────────────────
#
# Ported from the vendored runtime's charts-lite engine (`id`, `Vh`, `cg`, `yr`,
# `xw`, `Sw`, `ww`) rather than reinvented, so a chart lands where the app puts
# it: same default palette, same grid insets, same tick algorithm, same label
# placement. The engine deliberately honours only a subset of the ECharts option
# shape, and so does this — a key the app ignores is a key we ignore.

CHART_COLORS = [
    "#5470c6",
    "#91cc75",
    "#fac858",
    "#ee6666",
    "#73c0de",
    "#3ba272",
    "#fc8452",
    "#9a60b4",
]
AXIS_TEXT = "#6B7280"
AXIS_LINE = (0.431, 0.471, 0.529, 0.45)
SPLIT_LINE = (0.431, 0.471, 0.529, 0.15)


def opt_num(value, default):
    return (
        float(value) if isinstance(value, (int, float)) and value == value else default
    )


def format_number(value):
    """The engine's `yr`: two decimals max, thousands separated above 1000."""
    rounded = round(value * 100) / 100.0
    text = ("%d" % rounded) if float(rounded).is_integer() else ("%s" % rounded)
    if abs(rounded) >= 1000:
        negative = text.startswith("-")
        digits = text.lstrip("-")
        whole, _, fraction = digits.partition(".")
        grouped = "{:,}".format(int(whole))
        text = (
            ("-" if negative else "") + grouped + ("." + fraction if fraction else "")
        )
    return text


def nice_ticks(low, high, count=5):
    """The engine's `cg`."""
    if high == low:
        high = low + 1
    span = high - low
    power = math.pow(10, math.floor(math.log10(span / count)))
    ratio = span / count / power
    step = power * (
        10 if ratio >= 7.5 else 5 if ratio >= 3.5 else 2 if ratio >= 1.5 else 1
    )
    start = math.floor(low / step) * step
    stop = math.ceil(high / step) * step
    ticks = []
    value = start
    while value <= stop + step / 2:
        ticks.append(round(value * 1e6) / 1e6)
        value += step
    return ticks


def _even_ticks(low, high, count):
    return [low + (high - low) * (i / float(count - 1)) for i in range(count)]


def _step_ticks(low, high, count):
    def magnitude(value):
        if value <= 0:
            return 1
        exponent = math.floor(math.log10(value))
        mantissa = value / math.pow(10, exponent)
        pick = (
            1 if mantissa <= 1 else 2 if mantissa <= 2 else 5 if mantissa <= 5 else 10
        )
        return pick * math.pow(10, exponent)

    slots = max(1, count)
    step = magnitude((high - low) / slots) or 1
    guard = 0
    while low + step * slots < high and guard < 20:
        step = magnitude(step * 1.5)
        guard += 1
    return [round((low + step * i) * 1e6) / 1e6 for i in range(slots + 1)]


def axis_scale(values, axis, tick_count=None):
    """The engine's `Vh`: y range and labels for one value axis."""
    low = 0.0
    high = 0.0
    for value in values:
        high = max(high, value)
        low = min(low, value)
    bottom = axis.get("min") if axis.get("min") is not None else min(0.0, low)
    top = axis.get("max") if axis.get("max") is not None else (high or 1.0)
    pinned = axis.get("min") is not None or axis.get("max") is not None
    if tick_count:
        ticks = (
            _even_ticks(bottom, top, tick_count)
            if pinned
            else _step_ticks(bottom, top, tick_count - 1)
        )
    else:
        ticks = _even_ticks(bottom, top, 6) if pinned else nice_ticks(bottom, top)
    formatter = axis.get("formatter")
    labels = [
        formatter.replace("{value}", format_number(t))
        if formatter
        else format_number(t)
        for t in ticks
    ]
    return {"lo": ticks[0], "hi": ticks[-1], "labels": labels}


def normalize_chart(option, width, height):
    """The engine's `id`: every default the drawing code depends on."""
    option = option if isinstance(option, dict) else {}
    series = option.get("series")
    if isinstance(series, dict):
        series = [series]
    series = (
        [s for s in series if isinstance(s, dict)] if isinstance(series, list) else []
    )
    is_pie = any(s.get("type") == "pie" for s in series)
    grid = option.get("grid") if isinstance(option.get("grid"), dict) else {}
    legend_option = option.get("legend")
    legend = None
    if legend_option:
        legend_option = legend_option if isinstance(legend_option, dict) else {}
        text_style = (
            legend_option.get("textStyle")
            if isinstance(legend_option.get("textStyle"), dict)
            else {}
        )
        legend = {
            "color": text_style.get("color") or AXIS_TEXT,
            "size": opt_num(text_style.get("fontSize"), 12),
            "weight": text_style.get("fontWeight", 400),
            "itemWidth": opt_num(legend_option.get("itemWidth"), 14),
            "itemHeight": opt_num(legend_option.get("itemHeight"), 10),
            "itemGap": opt_num(legend_option.get("itemGap"), 16),
            "top": legend_option.get("top")
            if isinstance(legend_option.get("top"), (int, float))
            else None,
            "bottom": opt_num(legend_option.get("bottom"), 0),
        }
    band = max(legend["itemHeight"], legend["size"]) + 12 if legend else 0
    x_axis = option.get("xAxis") if isinstance(option.get("xAxis"), dict) else {}
    x_label = (
        x_axis.get("axisLabel") if isinstance(x_axis.get("axisLabel"), dict) else {}
    )
    y_option = option.get("yAxis")
    if isinstance(y_option, dict):
        y_option = [y_option]
    if not isinstance(y_option, list) or not y_option:
        y_option = [{}]
    y_axes = []
    for axis in y_option[:2]:
        axis = axis if isinstance(axis, dict) else {}
        label = axis.get("axisLabel") if isinstance(axis.get("axisLabel"), dict) else {}
        y_axes.append(
            {
                "name": axis.get("name") if isinstance(axis.get("name"), str) else None,
                "min": axis.get("min")
                if isinstance(axis.get("min"), (int, float))
                else None,
                "max": axis.get("max")
                if isinstance(axis.get("max"), (int, float))
                else None,
                "formatter": label.get("formatter")
                if isinstance(label.get("formatter"), str)
                else None,
                "label": {
                    "color": label.get("color") or x_label.get("color") or AXIS_TEXT,
                    "size": opt_num(label.get("fontSize"), 12),
                    "weight": label.get("fontWeight", 400),
                },
            }
        )
    dual = not is_pie and len(y_axes) > 1
    top_band = legend["top"] + band if legend and legend["top"] is not None else 0
    bottom_band = legend["bottom"] + band if legend and legend["top"] is None else 0
    overflow = max(0.0, band - 12)
    grid_bottom = (
        opt_num(grid.get("bottom"), 44)
        if "bottom" in grid
        else 44 + (overflow if legend and legend["top"] is None else 0)
    )
    grid_top = (
        opt_num(grid.get("top"), 24)
        if "top" in grid
        else 24 + (overflow if legend and legend["top"] is not None else 0)
    )
    if is_pie:
        plot = {
            "x": 0.0,
            "y": top_band,
            "w": width,
            "h": max(0.0, height - top_band - bottom_band),
        }
    else:
        left = opt_num(grid.get("left"), 48)
        right = opt_num(grid.get("right"), 56 if dual else 16)
        plot = {
            "x": left,
            "y": grid_top,
            "w": width - left - right,
            "h": max(0.0, height - grid_top - grid_bottom),
        }
    colors = option.get("color")
    colors = (
        [c for c in colors if isinstance(c, str)]
        if isinstance(colors, list) and colors
        else CHART_COLORS
    )
    text_style = (
        option.get("textStyle") if isinstance(option.get("textStyle"), dict) else {}
    )
    axis_line = (
        x_axis.get("axisLine") if isinstance(x_axis.get("axisLine"), dict) else {}
    )
    axis_line_style = (
        axis_line.get("lineStyle")
        if isinstance(axis_line.get("lineStyle"), dict)
        else {}
    )
    first_y = y_option[0] if isinstance(y_option[0], dict) else {}
    split = (
        first_y.get("splitLine") if isinstance(first_y.get("splitLine"), dict) else {}
    )
    split_style = (
        split.get("lineStyle") if isinstance(split.get("lineStyle"), dict) else {}
    )
    pie = next((s for s in series if s.get("type") == "pie"), None)
    pie_label = (
        pie.get("label")
        if isinstance(pie, dict) and isinstance(pie.get("label"), dict)
        else {}
    )
    categories = x_axis.get("data")
    return {
        "w": width,
        "h": height,
        "font": family_of(text_style.get("fontFamily") or "sans-serif"),
        "colors": colors,
        "series": series,
        "isPie": is_pie,
        "categories": [str(c) for c in categories]
        if isinstance(categories, list)
        else [],
        "grid": plot,
        "legend": legend,
        "xAxisLabel": {
            "color": x_label.get("color") or y_axes[0]["label"]["color"],
            "size": opt_num(x_label.get("fontSize"), 12),
            "weight": x_label.get("fontWeight", 400),
        },
        "axisLine": {
            "color": parse_color(axis_line_style.get("color"), AXIS_LINE),
            "width": opt_num(axis_line_style.get("width"), 1),
        },
        "splitLine": {
            "color": parse_color(split_style.get("color"), SPLIT_LINE),
            "width": opt_num(split_style.get("width"), 1),
        },
        "yAxes": y_axes,
        "labelColor": pie_label.get("color") or AXIS_TEXT,
    }


class ChartMixin:
    def render_chart(self, canvas, element, box):
        x, y, w, h = box
        if w <= 0 or h <= 0:
            return
        chart = normalize_chart(element.get("option"), w, h)
        if not chart["series"]:
            # The app draws the bare axis frame in this case, so we do too —
            # the warning is for the agent, not a reason to diverge.
            self.warn(
                "a chart element has no series; only its axes were "
                "drawn (the app does the same).",
                "chart-empty",
            )
        canvas.save()
        canvas.translate(x, y)
        if chart["isPie"]:
            self.draw_pie(canvas, chart)
        else:
            self.draw_cartesian(canvas, chart)
        if chart["legend"] and any(
            s.get("name") or s.get("type") == "pie" for s in chart["series"]
        ):
            self.draw_legend(canvas, chart)
        canvas.restore()

    # -- helpers -----------------------------------------------------------
    def chart_text(
        self,
        canvas,
        chart,
        cx,
        baseline,
        text,
        color,
        size,
        anchor="middle",
        weight=400,
    ):
        """`si()` in the runtime: an SVG <text> with a text-anchor."""
        if text is None or text == "":
            return
        face = face_name(chart["font"], is_bold(weight), False)
        width = text_width(self.encoder, text, face, size)
        start = cx
        if anchor == "middle":
            start = cx - width / 2.0
        elif anchor == "end":
            start = cx - width
        rgba = (
            color
            if isinstance(color, tuple)
            else parse_color(color, (0.42, 0.45, 0.5, 1.0))
        )
        canvas.apply_alpha(rgba[3])
        canvas.fill_color(rgba)
        canvas.show_text(start, baseline, text, face, size)

    def series_color(self, chart, series, index):
        item_style = (
            series.get("itemStyle") if isinstance(series.get("itemStyle"), dict) else {}
        )
        line_style = (
            series.get("lineStyle") if isinstance(series.get("lineStyle"), dict) else {}
        )
        for candidate in (item_style.get("color"), line_style.get("color")):
            if isinstance(candidate, str):
                return parse_color(candidate)
        return parse_color(chart["colors"][index % len(chart["colors"])])

    # -- cartesian ---------------------------------------------------------
    def draw_cartesian(self, canvas, chart):
        plot = chart["grid"]
        bars = [s for s in chart["series"] if s.get("type") == "bar"]
        lines = [s for s in chart["series"] if s.get("type") == "line"]
        points = [s for s in chart["series"] if s.get("type") == "scatter"]
        categories = chart["categories"]
        count = max(1, len(categories))
        axis_count = len(chart["yAxes"])

        def axis_index(series):
            return min(
                axis_count - 1, max(0, int(round(opt_num(series.get("yAxisIndex"), 0))))
            )

        buckets = [[] for _ in range(axis_count)]
        for series in bars + lines:
            data = series.get("data")
            data = data if isinstance(data, list) else []
            buckets[axis_index(series)].extend(
                opt_num(value, 0) for value in data[:count]
            )
        low = float("inf")
        high = float("-inf")
        for series in points:
            for entry in series.get("data") or []:
                px, py = (
                    (entry[0], entry[1])
                    if isinstance(entry, list) and len(entry) >= 2
                    else (0, 0)
                )
                low = min(low, px)
                high = max(high, px)
                buckets[0].append(opt_num(py, 0))
        primary = axis_scale(buckets[0], chart["yAxes"][0])
        ticks = len(primary["labels"])
        secondary = (
            axis_scale(buckets[1], chart["yAxes"][1], ticks) if axis_count > 1 else None
        )

        def value_y(value, axis=0):
            scale = secondary if axis == 1 and secondary else primary
            span = (scale["hi"] - scale["lo"]) or 1
            return plot["y"] + plot["h"] - (value - scale["lo"]) / span * plot["h"]

        def zero_y(axis=0):
            return max(plot["y"], min(plot["y"] + plot["h"], value_y(0, axis)))

        # split lines + y labels
        for index in range(ticks):
            line_y = plot["y"] + plot["h"] - index / max(1, ticks - 1) * plot["h"]
            canvas.line_path(plot["x"], line_y, plot["x"] + plot["w"], line_y)
            canvas.paint(
                stroke=chart["splitLine"]["color"], width=chart["splitLine"]["width"]
            )
            label = chart["yAxes"][0]["label"]
            self.chart_text(
                canvas,
                chart,
                plot["x"] - 8,
                line_y + label["size"] * 0.35,
                primary["labels"][index],
                parse_color(label["color"], (0.42, 0.45, 0.5, 1)),
                label["size"],
                "end",
                label["weight"],
            )
            if secondary:
                label2 = chart["yAxes"][1]["label"]
                self.chart_text(
                    canvas,
                    chart,
                    plot["x"] + plot["w"] + 8,
                    line_y + label2["size"] * 0.35,
                    secondary["labels"][index],
                    parse_color(label2["color"], (0.42, 0.45, 0.5, 1)),
                    label2["size"],
                    "start",
                    label2["weight"],
                )
        for axis, anchor, at_x in (
            (0, "end", plot["x"] - 8),
            (1, "start", plot["x"] + plot["w"] + 8),
        ):
            if (
                axis < axis_count
                and chart["yAxes"][axis].get("name")
                and (axis == 0 or secondary)
            ):
                self.chart_text(
                    canvas,
                    chart,
                    at_x,
                    plot["y"] - 9,
                    chart["yAxes"][axis]["name"],
                    parse_color(chart["xAxisLabel"]["color"], (0.42, 0.45, 0.5, 1)),
                    11,
                    anchor,
                )

        baseline = zero_y(0)
        canvas.line_path(plot["x"], baseline, plot["x"] + plot["w"], baseline)
        canvas.paint(
            stroke=chart["axisLine"]["color"], width=chart["axisLine"]["width"]
        )

        # A scatter with no categories gets a numeric x axis, like the app.
        if points and not categories:
            span_ticks = nice_ticks(
                0 if low == float("inf") else min(0, low),
                1 if high == float("-inf") else high,
            )
            first, last = span_ticks[0], span_ticks[-1]
            width = (last - first) or 1

            def point_x(value):
                return plot["x"] + (value - first) / width * plot["w"]

            for tick in span_ticks:
                self.chart_text(
                    canvas,
                    chart,
                    point_x(tick),
                    plot["y"] + plot["h"] + chart["xAxisLabel"]["size"] + 6,
                    format_number(tick),
                    parse_color(chart["xAxisLabel"]["color"], (0.42, 0.45, 0.5, 1)),
                    chart["xAxisLabel"]["size"],
                    "middle",
                    chart["xAxisLabel"]["weight"],
                )
            for index, series in enumerate(points):
                color = self.series_color(chart, series, chart["series"].index(series))
                radius = opt_num(series.get("symbolSize"), 10) / 2.0
                for entry in series.get("data") or []:
                    px, py = (
                        (entry[0], entry[1])
                        if isinstance(entry, list) and len(entry) >= 2
                        else (0, 0)
                    )
                    at_x = point_x(px)
                    if (
                        at_x < plot["x"] - radius
                        or at_x > plot["x"] + plot["w"] + radius
                    ):
                        continue
                    canvas.save()
                    canvas.alpha(0.85)
                    canvas.ellipse_path(at_x, value_y(opt_num(py, 0)), radius, radius)
                    canvas.paint(fill=color)
                    canvas.restore()
                del index
            return

        band = plot["w"] / count
        stride = math.ceil(count / max(1, math.floor(plot["w"] / 56)))
        for index, category in enumerate(categories):
            if index % stride:
                continue
            self.chart_text(
                canvas,
                chart,
                plot["x"] + band * (index + 0.5),
                plot["y"] + plot["h"] + chart["xAxisLabel"]["size"] + 6,
                category,
                parse_color(chart["xAxisLabel"]["color"], (0.42, 0.45, 0.5, 1)),
                chart["xAxisLabel"]["size"],
                "middle",
                chart["xAxisLabel"]["weight"],
            )

        if bars:
            group = band * 0.62
            slot = group / len(bars)
            for order, series in enumerate(bars):
                axis = axis_index(series)
                base = zero_y(axis)
                color = self.series_color(chart, series, chart["series"].index(series))
                item_style = (
                    series.get("itemStyle")
                    if isinstance(series.get("itemStyle"), dict)
                    else {}
                )
                corner = item_style.get("borderRadius")
                corner = (
                    opt_num(corner[0], 0)
                    if isinstance(corner, list)
                    else opt_num(corner, 0)
                )
                data = series.get("data")
                data = data if isinstance(data, list) else []
                for index, raw in enumerate(data[:count]):
                    value = opt_num(raw, 0)
                    left = (
                        plot["x"] + band * index + (band - group) / 2.0 + slot * order
                    )
                    top = value_y(value, axis)
                    height = abs(base - top)
                    canvas.rect_path(
                        left + 1,
                        top if top <= base else base,
                        max(1.0, slot - 2),
                        max(0.0, height),
                        min(corner, slot / 2.0),
                    )
                    canvas.paint(fill=color)

        for series in lines:
            order = chart["series"].index(series)
            axis = axis_index(series)
            color = self.series_color(chart, series, order)
            width = opt_num(
                (series.get("lineStyle") or {}).get("width")
                if isinstance(series.get("lineStyle"), dict)
                else None,
                2,
            )
            data = series.get("data")
            data = data if isinstance(data, list) else []
            values = [opt_num(value, 0) for value in data[:count]]
            pts = [
                (plot["x"] + band * (index + 0.5), value_y(value, axis))
                for index, value in enumerate(values)
            ]
            if len(pts) < 2:
                continue
            path = self._line_path(pts, bool(series.get("smooth")))
            area = series.get("areaStyle")
            if area is not None:
                canvas.save()
                canvas.alpha(
                    1.0 if isinstance(area, dict) and area.get("color") else 0.25
                )
                self._emit(canvas, path)
                canvas.op("%s %s l" % (num(pts[-1][0]), num(zero_y(axis))))
                canvas.op("%s %s l" % (num(pts[0][0]), num(zero_y(axis))))
                canvas.op("h")
                fill = color
                if isinstance(area, dict) and isinstance(area.get("color"), str):
                    fill = parse_color(area["color"], color)
                canvas.paint(fill=fill)
                canvas.restore()
            self._emit(canvas, path)
            canvas.op("1 J")
            canvas.paint(stroke=color, width=width)
            if series.get("symbol") != "none":
                radius = opt_num(series.get("symbolSize"), 7) / 2.0
                for at_x, at_y in pts:
                    canvas.ellipse_path(at_x, at_y, radius, radius)
                    canvas.paint(fill=color)

    def _line_path(self, pts, smooth):
        """The engine's line geometry, including its Catmull-Rom smoothing."""
        path = [("m", pts[0])]
        if smooth:
            for index in range(len(pts) - 1):
                previous = pts[max(0, index - 1)]
                current = pts[index]
                following = pts[index + 1]
                after = pts[min(len(pts) - 1, index + 2)]
                path.append(
                    (
                        "c",
                        (
                            current[0] + (following[0] - previous[0]) / 6.0,
                            current[1] + (following[1] - previous[1]) / 6.0,
                        ),
                        (
                            following[0] - (after[0] - current[0]) / 6.0,
                            following[1] - (after[1] - current[1]) / 6.0,
                        ),
                        following,
                    )
                )
        else:
            for point in pts[1:]:
                path.append(("l", point))
        return path

    def _emit(self, canvas, path):
        for segment in path:
            if segment[0] == "m":
                canvas.op("%s %s m" % (num(segment[1][0]), num(segment[1][1])))
            elif segment[0] == "l":
                canvas.op("%s %s l" % (num(segment[1][0]), num(segment[1][1])))
            else:
                (a, b), (c, d), (e, f) = segment[1:]
                canvas.op(
                    "%s %s %s %s %s %s c"
                    % (num(a), num(b), num(c), num(d), num(e), num(f))
                )

    # -- pie ---------------------------------------------------------------
    def draw_pie(self, canvas, chart):
        series = next(s for s in chart["series"] if s.get("type") == "pie")
        data = series.get("data")
        data = data if isinstance(data, list) else []
        slices = []
        for index, entry in enumerate(data):
            entry = entry if isinstance(entry, dict) else {}
            slices.append(
                (
                    str(entry.get("name", index)),
                    max(0.0, opt_num(entry.get("value"), 0)),
                )
            )
        total = sum(value for _, value in slices) or 1.0
        plot = chart["grid"]
        cx = plot["x"] + plot["w"] / 2.0
        cy = plot["y"] + plot["h"] / 2.0
        limit = min(plot["w"], plot["h"]) / 2.0
        radius = series.get("radius")
        pair = radius if isinstance(radius, list) else ["0%", radius or "70%"]

        def resolve(value):
            if isinstance(value, str) and value.strip().endswith("%"):
                return float(value.strip()[:-1]) / 100.0 * limit
            return opt_num(value, 0)

        inner, outer = resolve(pair[0]), resolve(pair[1] if len(pair) > 1 else "70%")
        item_style = (
            series.get("itemStyle") if isinstance(series.get("itemStyle"), dict) else {}
        )
        border = parse_color(item_style.get("borderColor"), (0, 0, 0, 0.0))
        border_width = opt_num(item_style.get("borderWidth"), 0)
        label = series.get("label")
        formatter = None
        if label is not False:
            formatter = (
                label.get("formatter")
                if isinstance(label, dict) and isinstance(label.get("formatter"), str)
                else "{b}"
            )
        angle = -math.pi / 2
        for index, (name, value) in enumerate(slices):
            share = value / total
            end = angle + share * math.pi * 2
            color = parse_color(chart["colors"][index % len(chart["colors"])])
            if end > angle + 1e-4:
                self._pie_slice(canvas, cx, cy, inner, outer, angle, end)
                canvas.paint(fill=color, stroke=border, width=border_width)
            if formatter:
                middle = (angle + end) / 2.0
                at_x = cx + math.cos(middle) * (outer + 12)
                at_y = cy + math.sin(middle) * (outer + 12)
                right = math.cos(middle) >= 0
                text = (
                    formatter.replace("{b}", name)
                    .replace("{c}", format_number(value))
                    .replace("{d}", format_number(round(share * 1000) / 10.0))
                )
                canvas.line_path(
                    cx + math.cos(middle) * outer,
                    cy + math.sin(middle) * outer,
                    at_x,
                    at_y,
                )
                canvas.paint(stroke=color, width=1)
                self.chart_text(
                    canvas,
                    chart,
                    at_x + (4 if right else -4),
                    at_y + 4,
                    text,
                    parse_color(chart["labelColor"], (0.42, 0.45, 0.5, 1)),
                    12,
                    "start" if right else "end",
                )
            angle = end

    def _pie_slice(self, canvas, cx, cy, inner, outer, start, end):
        def arc(radius, from_angle, to_angle, move):
            span = to_angle - from_angle
            steps = max(1, int(math.ceil(abs(span) / (math.pi / 2))))
            step = span / steps
            k = 4.0 / 3.0 * math.tan(step / 4.0)
            angle = from_angle
            if move:
                canvas.op(
                    "%s %s m"
                    % (
                        num(cx + math.cos(angle) * radius),
                        num(cy + math.sin(angle) * radius),
                    )
                )
            for _ in range(steps):
                nxt = angle + step
                x1 = cx + math.cos(angle) * radius
                y1 = cy + math.sin(angle) * radius
                x2 = cx + math.cos(nxt) * radius
                y2 = cy + math.sin(nxt) * radius
                canvas.op(
                    "%s %s %s %s %s %s c"
                    % (
                        num(x1 - k * math.sin(angle) * radius),
                        num(y1 + k * math.cos(angle) * radius),
                        num(x2 + k * math.sin(nxt) * radius),
                        num(y2 - k * math.cos(nxt) * radius),
                        num(x2),
                        num(y2),
                    )
                )
                angle = nxt

        arc(outer, start, end, True)
        if inner > 0:
            canvas.op(
                "%s %s l"
                % (num(cx + math.cos(end) * inner), num(cy + math.sin(end) * inner))
            )
            arc(inner, end, start, False)
        else:
            canvas.op("%s %s l" % (num(cx), num(cy)))
        canvas.op("h")

    # -- legend ------------------------------------------------------------
    def draw_legend(self, canvas, chart):
        legend = chart["legend"]
        pie = next((s for s in chart["series"] if s.get("type") == "pie"), None)
        if pie is not None:
            data = pie.get("data") if isinstance(pie.get("data"), list) else []
            entries = [
                (
                    str((entry or {}).get("name", index))
                    if isinstance(entry, dict)
                    else str(index),
                    parse_color(chart["colors"][index % len(chart["colors"])]),
                )
                for index, entry in enumerate(data)
            ]
        else:
            entries = [
                (
                    str(series.get("name") or "Series %d" % (index + 1)),
                    self.series_color(chart, series, index),
                )
                for index, series in enumerate(chart["series"])
            ]
        if not entries:
            return
        face = face_name(chart["font"], is_bold(legend["weight"]), False)
        widths = [
            legend["itemWidth"]
            + 8
            + text_width(self.encoder, name, face, legend["size"])
            for name, _ in entries
        ]
        span = sum(widths) + legend["itemGap"] * max(0, len(entries) - 1)
        left = max(8.0, (chart["w"] - span) / 2.0)
        height = max(legend["itemHeight"], legend["size"])
        top = (
            legend["top"]
            if legend["top"] is not None
            else chart["h"] - legend["bottom"] - height - 10
        )
        swatch_y = top + (height - legend["itemHeight"]) / 2.0
        baseline = top + height / 2.0 + legend["size"] * 0.35
        for index, (name, color) in enumerate(entries):
            canvas.rect_path(
                left,
                swatch_y,
                legend["itemWidth"],
                legend["itemHeight"],
                min(3.0, legend["itemHeight"] / 2.0),
            )
            canvas.paint(fill=color)
            self.chart_text(
                canvas,
                chart,
                left + legend["itemWidth"] + 8,
                baseline,
                name,
                parse_color(legend["color"], (0.42, 0.45, 0.5, 1)),
                legend["size"],
                "start",
                legend["weight"],
            )
            left += widths[index] + (
                legend["itemGap"] if index < len(entries) - 1 else 0
            )


class DeckRenderer(Renderer, ShapeMixin, TableMixin, ChartMixin):
    """The whole renderer: document plumbing plus every element type."""


def render_document(doc):
    """Bento document -> (pdf bytes, [warnings])."""
    renderer = DeckRenderer(doc)
    return renderer.render(), renderer.warnings
