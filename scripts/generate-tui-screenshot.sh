#!/usr/bin/env bash
# scripts/generate-tui-screenshot.sh — render the `fleet chat` TUI to a PNG (#487).
#
# The chat TUI pings a live server before entering the alt-screen, so we can't
# just run it headless. Instead we render a representative frame DETERMINISTICALLY
# from in-memory model state (the FLEET_TUI_SCREENSHOT_ANSI-gated generator test
# in internal/chattui) — no server, no LLM, no real conversation data — then turn
# that ANSI into a PNG with charmbracelet/freeze (a self-contained Go tool; no
# ffmpeg / headless browser needed). Mirrors how the GUI screenshots are captured
# off the mocked Next app.
#
# Usage: scripts/generate-tui-screenshot.sh [output.png]
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

out_png="${1:-docs/screenshots/tui/chat.png}"
mkdir -p "$(dirname "$out_png")"

# Pin freeze so the render is reproducible; install it to a scratch GOBIN if the
# `freeze` on PATH isn't this version.
FREEZE_VERSION="${FREEZE_VERSION:-v0.2.2}"
freeze_bin="$(command -v freeze || true)"
if [ -z "$freeze_bin" ]; then
  tmp_gobin="$(mktemp -d)"
  echo "installing charmbracelet/freeze@${FREEZE_VERSION}…"
  GOBIN="$tmp_gobin" GOTOOLCHAIN=auto go install "github.com/charmbracelet/freeze@${FREEZE_VERSION}"
  freeze_bin="$tmp_gobin/freeze"
fi

ansi_file="$(mktemp --suffix=.ansi)"
trap 'rm -f "$ansi_file"' EXIT

echo "rendering TUI frame → $ansi_file"
# Force a color profile so glamour + lipgloss render their styled (dark) output
# instead of the no-TTY "notty" plain fallback — the frame then matches what a
# real terminal shows (bold, colored markdown), which is the whole point of the
# screenshot. termenv honors CLICOLOR_FORCE.
CLICOLOR_FORCE=1 COLORTERM=truecolor TERM=xterm-256color \
  FLEET_TUI_SCREENSHOT_ANSI="$ansi_file" GOTOOLCHAIN=auto \
  go test ./internal/chattui/ -run '^TestGenerateTUIScreenshot$' -count=1 >/dev/null

if [ ! -s "$ansi_file" ]; then
  echo "error: generator produced no ANSI output" >&2
  exit 1
fi

echo "freezing → $out_png"
# --window gives it a titled terminal chrome; padding + a dark background match
# the app's look. freeze auto-detects the ANSI SGR codes.
"$freeze_bin" "$ansi_file" \
  --output "$out_png" \
  --window \
  --background "#171717" \
  --padding 20 \
  --margin 0

echo "wrote $out_png"
