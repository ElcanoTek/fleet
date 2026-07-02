#!/usr/bin/env bash
# Regenerate docs/screenshots/tui/demo.gif — the `fleet chat` TUI demo (#540).
#
# Records the REAL TUI with charm's vhs (a headless terminal recorder) against
# the deterministic local mock server in mock_chat_server.py, so the demo is
# free, key-less, and reproducible. See docs/generating-demo-gif.md.
#
# Requirements: vhs + ttyd + ffmpeg on PATH (Fedora: dnf install vhs ttyd ffmpeg).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

for tool in vhs ttyd ffmpeg python3; do
    command -v "$tool" >/dev/null || { echo "generate-tui-gif: $tool is required" >&2; exit 1; }
done

# vhs drives a headless Chromium; running as root needs the sandbox waiver.
if [ "$(id -u)" = "0" ]; then
    export VHS_NO_SANDBOX=true
fi

go build -o fleet ./cmd/fleet
vhs docs/scripts/demo.tape
ls -la docs/screenshots/tui/demo.gif
