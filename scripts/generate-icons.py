#!/usr/bin/env python3
"""Regenerate all Fleet app icons from the master SVG.

Usage:    python3 scripts/generate-icons.py
Requires: pip install cairosvg pillow

Master:   web/public/logos/fleet-mark.svg  (single flattened path, 512 viewBox)
Outputs:  web/src/app/            favicon.ico, icon.svg, apple-icon.png
          web/public/app-icons/   favicon-16/32, icon-192/512, maskable-icon-512
"""

import io
from pathlib import Path

import cairosvg
from PIL import Image

ROOT = Path(__file__).resolve().parent.parent
MASTER = ROOT / "web/public/logos/fleet-mark.svg"
APP = ROOT / "web/src/app"
PUB = ROOT / "web/public/app-icons"
PLUM = "#1A0B1E"  # matches manifest background_color / theme_color


def render(px: int, scale: float = 1.0, bg: str | None = None) -> Image.Image:
    """Rasterize the master at px, optionally scaled down onto a square background."""
    png = cairosvg.svg2png(
        url=str(MASTER), output_width=int(px * scale), output_height=int(px * scale)
    )
    glyph = Image.open(io.BytesIO(png)).convert("RGBA")
    if scale == 1.0 and bg is None:
        return glyph
    canvas = Image.new("RGBA", (px, px), bg if bg else (0, 0, 0, 0))
    off = (px - glyph.width) // 2
    canvas.alpha_composite(glyph, (off, off))
    return canvas


PUB.mkdir(parents=True, exist_ok=True)

# --- web app manifest icons (Android home screen / install / splash) --------
render(192).save(PUB / "icon-192.png")
render(512).save(PUB / "icon-512.png")
# maskable: full-bleed background, glyph inside the center-80% safe zone,
# so every Android launcher shape (circle, squircle, rounded square) crops clean
render(512, scale=0.82, bg=PLUM).save(PUB / "maskable-icon-512.png")

# --- classic favicons --------------------------------------------------------
render(16).save(PUB / "favicon-16.png")
render(32).save(PUB / "favicon-32.png")
ico = [render(s) for s in (48, 32, 16)]
ico[0].save(
    APP / "favicon.ico",
    format="ICO",
    append_images=ico[1:],
    sizes=[(48, 48), (32, 32), (16, 16)],
)

# --- apple touch icon (iOS Add to Home Screen) -------------------------------
# Must be opaque: iOS fills transparency with black. iOS applies its own
# corner mask, so this is a full-bleed square with the glyph inset.
render(180, scale=0.78, bg=PLUM).convert("RGB").save(APP / "apple-icon.png")

# --- modern SVG favicon (crisp at any DPI in current browsers) ---------------
(APP / "icon.svg").write_bytes(MASTER.read_bytes())

print(f"regenerated all icons from {MASTER.relative_to(ROOT)}")
