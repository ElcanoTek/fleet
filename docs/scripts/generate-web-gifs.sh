#!/usr/bin/env bash
# Convert the recorded web demo takes (record-web-demos.mjs) into the README
# GIFs (#540). Recording and conversion are split so a good take can be
# re-converted without re-driving the app. See docs/generating-demo-gif.md.
#
# Usage: generate-web-gifs.sh <chat-take.webm> <ops-take.webm>
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHAT_IN="${1:?usage: generate-web-gifs.sh <chat-take.webm> <ops-take.webm>}"
OPS_IN="${2:?usage: generate-web-gifs.sh <chat-take.webm> <ops-take.webm>}"
OUT_DIR="$REPO_ROOT/docs/screenshots/web"
mkdir -p "$OUT_DIR"

# to_gif <in> <out> <trim-seconds> <speedup>
#   Trims the take, speeds it up so the GIF respects the reader's time, and
#   quantizes with a single generated palette (crisp text, stable colors).
to_gif() {
    local in="$1" out="$2" trim="$3" speed="$4"
    local tmp; tmp="$(mktemp -u).png"
    ffmpeg -y -v error -t "$trim" -i "$in" \
        -vf "setpts=PTS/${speed},fps=9,scale=880:-1:flags=lanczos,palettegen=stats_mode=diff" "$tmp"
    ffmpeg -y -v error -t "$trim" -i "$in" -i "$tmp" \
        -lavfi "setpts=PTS/${speed},fps=9,scale=880:-1:flags=lanczos [x]; [x][1:v] paletteuse=dither=bayer:bayer_scale=5" \
        "$out"
    rm -f "$tmp"
    # gifsicle squeezes another ~2x via lossy LZW + a tighter palette without
    # visibly hurting UI text at this scale.
    if command -v gifsicle >/dev/null 2>&1; then
        gifsicle -O3 --lossy=35 --colors 128 -b "$out"
    fi
    ls -la "$out"
}

# Chat: the take is ~100s (typing → send → the model thinks + runs tools →
# the answer streams). The useful arc is the first ~48s; 1.6x keeps the
# streaming feel without the dead air.
to_gif "$CHAT_IN" "$OUT_DIR/chat-demo.gif" 46 1.9

# Ops: sign-in → automation fleet → Upcoming. Short take, gentle speedup.
to_gif "$OPS_IN" "$OUT_DIR/ops-demo.gif" 40 1.5
