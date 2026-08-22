#!/usr/bin/env python3
"""Generate the open-source vs Elcano-engagement comparison SVG (light + dark)."""

import os

OUT_DIR = os.path.dirname(os.path.abspath(__file__))

# ---------------------------------------------------------------- layout
PITCH = 142  # horizontal distance between item centers
C0 = 140  # x of first item center
N = 12  # total items
N_OSS = 6  # items under the open-source brace
W = 2 * C0 + (N - 1) * PITCH
H = 368

TILE = 74  # tile side
TILE_TOP = 116
TILE_CY = TILE_TOP + TILE / 2
LABEL_Y1 = TILE_TOP + TILE + 26
LABEL_Y2 = LABEL_Y1 + 20

TOP_BRACE_Y = TILE_TOP - 12  # endpoints (just above tiles)
TOP_BRACE_H = 12
TOP_LABEL_Y = TOP_BRACE_Y - TOP_BRACE_H * 2 - 16

BOT_BRACE_Y = LABEL_Y2 + 18
BOT_BRACE_H = 13
BOT_LABEL_Y = BOT_BRACE_Y + BOT_BRACE_H * 2 + 34

THEMES = {
    "light": dict(
        ink="#1f2328",
        text="#1f2328",
        muted="#59636e",
        brace_top="#848d97",
        brace_bot="#59636e",
        fill_op="0.16",
        stroke_op="0.5",
    ),
    "dark": dict(
        ink="#e6edf3",
        text="#e6edf3",
        muted="#9198a1",
        brace_top="#767d86",
        brace_bot="#9198a1",
        fill_op="0.22",
        stroke_op="0.6",
    ),
}

ACCENTS = [
    "#3b82f6",
    "#10b981",
    "#8b5cf6",
    "#f59e0b",
    "#f43f5e",
    "#06b6d4",
    "#8b5cf6",
    "#3b82f6",
    "#f59e0b",
    "#10b981",
    "#f43f5e",
    "#06b6d4",
]

FONT = "-apple-system, 'Segoe UI', 'Helvetica Neue', Arial, sans-serif"


def sparkle(cx, cy, r):
    """Four-point star, filled with ink."""
    k = r * 0.14
    return (
        f'<path d="M {cx} {cy - r} C {cx + k} {cy - k * 2} {cx + k * 2} {cy - k} {cx + r} {cy} '
        f"C {cx + k * 2} {cy + k} {cx + k} {cy + k * 2} {cx} {cy + r} "
        f"C {cx - k} {cy + k * 2} {cx - k * 2} {cy + k} {cx - r} {cy} "
        f'C {cx - k * 2} {cy - k} {cx - k} {cy - k * 2} {cx} {cy - r} Z" '
        f'fill="{{I}}" stroke="none"/>'
    )


# ---------------------------------------------------------------- glyphs
# Each glyph is drawn in a coordinate system centered on the tile.
# {I} = ink color placeholder. Strokes get stroke="{I}" via the group wrapper.
GLYPHS = [
    # 1 chat + scheduler
    '<rect x="-23" y="-19" width="31" height="23" rx="6"/>'
    '<path d="M -17 4 L -17 13 L -8 4"/>'
    '<path d="M -17 -11 H 1"/><path d="M -17 -4 H -5"/>'
    '<circle cx="13" cy="10" r="9"/>'
    '<path d="M 13 5 L 13 10 L 17 12"/>',
    # 2 sandboxed tool calls (shield + prompt)
    '<path d="M 0 -20 L 15.5 -14 V -2 C 15.5 8 8 15.5 0 19.5 C -8 15.5 -15.5 8 -15.5 -2 V -14 Z"/>'
    '<path d="M -7 -7 L -1 -1.5 L -7 4"/>'
    '<path d="M 3 5 H 9"/>',
    # 3 MCP connector catalog (plug)
    '<rect x="-10" y="-9" width="20" height="15" rx="4.5"/>'
    '<path d="M -4.5 -9 V -17"/><path d="M 4.5 -9 V -17"/>'
    '<path d="M 0 6 V 10 C 0 16 -11 14.5 -11 20"/>',
    # 4 any model (chip + sparkle)
    '<rect x="-13" y="-13" width="26" height="26" rx="5"/>'
    '<path d="M -7 -13 V -19"/><path d="M 0 -13 V -19"/><path d="M 7 -13 V -19"/>'
    '<path d="M -7 13 V 19"/><path d="M 0 13 V 19"/><path d="M 7 13 V 19"/>'
    '<path d="M -13 -7 H -19"/><path d="M -13 0 H -19"/><path d="M -13 7 H -19"/>'
    '<path d="M 13 -7 H 19"/><path d="M 13 0 H 19"/><path d="M 13 7 H 19"/>'
    + sparkle(0, 0, 7),
    # 5 budgets & audit (gauge)
    '<path d="M -16 9 A 16 16 0 1 1 16 9"/>'
    '<path d="M 0 8 L 9.5 -3.5"/>'
    '<circle cx="0" cy="8" r="2.8" fill="{I}" stroke="none"/>',
    # 6 web / TUI / API (terminal monitor)
    '<rect x="-18" y="-16" width="36" height="25" rx="4"/>'
    '<path d="M 0 9 V 15"/><path d="M -9 18 H 9"/>'
    '<path d="M -11 -9 L -5 -3.5 L -11 2"/>'
    '<path d="M -1 3 H 8"/>',
    # 7 custom MCP connectors (plug + sparkle)
    '<rect x="-13" y="-6" width="20" height="15" rx="4.5"/>'
    '<path d="M -7.5 -6 V -14"/><path d="M 1.5 -6 V -14"/>'
    '<path d="M -3 9 V 13 C -3 18.5 -13 17 -13 22"/>' + sparkle(13, -12, 6),
    # 8 data integrations (database + arrow)
    '<ellipse cx="-5" cy="-12" rx="12" ry="5"/>'
    '<path d="M -17 -12 V 8 C -17 11 -11.6 13.5 -5 13.5 C 1.6 13.5 7 11 7 8 V -12"/>'
    '<path d="M -17 -2 C -17 1 -11.6 3.5 -5 3.5 C 1.6 3.5 7 1 7 -2"/>'
    '<path d="M 12 1 H 20"/><path d="M 16.5 -3 L 20.5 1 L 16.5 5"/>',
    # 9 add-on capabilities (envelope + sparkle)
    '<rect x="-17" y="-9" width="33" height="23" rx="4"/>'
    '<path d="M -17 -6 L -0.5 5 L 16 -6"/>' + sparkle(14, -16, 6),
    # 10 forward-deployed engineering (person + map pin)
    '<circle cx="-6" cy="-9" r="7"/>'
    '<path d="M -18 15 C -18 5 -14 3 -6 3 C 2 3 6 5 6 15"/>'
    '<path d="M 13 17 C 13 17 5.5 9.5 5.5 5 A 7.5 7.5 0 1 1 20.5 5 C 20.5 9.5 13 17 13 17 Z"/>'
    '<circle cx="13" cy="4.5" r="2.6" fill="{I}" stroke="none"/>',
    # 11 production-ready workflows (calendar + check)
    '<rect x="-16" y="-14" width="32" height="29" rx="4"/>'
    '<path d="M -16 -6 H 16"/>'
    '<path d="M -8 -14 V -20"/><path d="M 8 -14 V -20"/>'
    '<path d="M -7.5 3.5 L -2.5 8.5 L 8 -3"/>',
    # 12 support & operations (lifebuoy)
    '<circle cx="0" cy="0" r="17"/>'
    '<circle cx="0" cy="0" r="7.5"/>'
    '<path d="M 0 -17 V -7.5"/><path d="M 0 7.5 V 17"/>'
    '<path d="M -17 0 H -7.5"/><path d="M 7.5 0 H 17"/>',
]

LABELS = [
    ("Chat + scheduler,", "one binary"),
    ("Sandboxed", "tool calls"),
    ("MCP connector", "catalog"),
    ("Any model,", "per task"),
    ("Budgets, ceilings", "& audit"),
    ("Web · TUI ·", "HTTP API"),
    ("Custom MCP", "connectors"),
    ("Data", "integrations"),
    ("Add-on", "capabilities"),
    ("Forward-deployed", "engineering"),
    ("Production-ready", "workflows"),
    ("Support &", "operations"),
]


def brace(x1, x2, y, h, up=True):
    """Curly brace from (x1,y) to (x2,y); cusp points up when up=True."""
    s = -h if up else h
    xm = (x1 + x2) / 2
    return (
        f"M {x1} {y} "
        f"C {x1} {y + s}, {x1 + h} {y + s}, {x1 + 2 * h} {y + s} "
        f"L {xm - 2 * h} {y + s} "
        f"C {xm - h} {y + s}, {xm} {y + s}, {xm} {y + 2 * s} "
        f"C {xm} {y + s}, {xm + h} {y + s}, {xm + 2 * h} {y + s} "
        f"L {x2 - 2 * h} {y + s} "
        f"C {x2 - h} {y + s}, {x2} {y + s}, {x2} {y}"
    )


def render(theme):
    t = THEMES[theme]
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" '
        f'font-family="{FONT}" role="img" '
        f'aria-label="Everything in this repo is the open-source fleet platform; '
        f'an Elcano engagement adds custom connectors, integrations, and forward-deployed engineering.">'
    ]

    for i in range(N):
        cx = C0 + i * PITCH
        a = ACCENTS[i]
        glyph = GLYPHS[i].replace("{I}", t["ink"])
        parts.append(
            f'<g transform="translate({cx} {TILE_CY})">'
            f'<rect x="{-TILE / 2}" y="{-TILE / 2}" width="{TILE}" height="{TILE}" rx="17" '
            f'fill="{a}" fill-opacity="{t["fill_op"]}" '
            f'stroke="{a}" stroke-opacity="{t["stroke_op"]}" stroke-width="2"/>'
            f'<g fill="none" stroke="{t["ink"]}" stroke-width="3" '
            f'stroke-linecap="round" stroke-linejoin="round">{glyph}</g>'
            f"</g>"
        )
        l1, l2 = (s.replace("&", "&amp;") for s in LABELS[i])
        parts.append(
            f'<text x="{cx}" y="{LABEL_Y1}" text-anchor="middle" font-size="15.5" '
            f'font-weight="500" fill="{t["text"]}">{l1}</text>'
            f'<text x="{cx}" y="{LABEL_Y2}" text-anchor="middle" font-size="15.5" '
            f'font-weight="500" fill="{t["text"]}">{l2}</text>'
        )

    # top brace: the open-source subset
    tx1, tx2 = C0 - 56, C0 + (N_OSS - 1) * PITCH + 56
    parts.append(
        f'<path d="{brace(tx1, tx2, TOP_BRACE_Y, TOP_BRACE_H, up=True)}" '
        f'fill="none" stroke="{t["brace_top"]}" stroke-width="2.5" stroke-linecap="round"/>'
    )
    parts.append(
        f'<text x="{(tx1 + tx2) / 2}" y="{TOP_LABEL_Y}" text-anchor="middle" '
        f'font-size="23" font-weight="600" fill="{t["muted"]}">fleet — open source (this repo)</text>'
    )

    # bottom brace: the engagement superset
    bx1, bx2 = C0 - 60, C0 + (N - 1) * PITCH + 60
    parts.append(
        f'<path d="{brace(bx1, bx2, BOT_BRACE_Y, BOT_BRACE_H, up=False)}" '
        f'fill="none" stroke="{t["brace_bot"]}" stroke-width="2.5" stroke-linecap="round"/>'
    )
    parts.append(
        f'<text x="{(bx1 + bx2) / 2}" y="{BOT_LABEL_Y}" text-anchor="middle" '
        f'font-size="25" font-weight="600" fill="{t["text"]}">fleet — with an Elcano engagement</text>'
    )

    parts.append("</svg>")
    return "\n".join(parts) + "\n"


os.makedirs(OUT_DIR, exist_ok=True)
for theme in THEMES:
    path = f"{OUT_DIR}/open-source-vs-elcano-{theme}.svg"
    with open(path, "w") as f:
        f.write(render(theme))
    print(path)
