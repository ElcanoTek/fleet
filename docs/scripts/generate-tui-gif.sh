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

# The vhs → ttyd → headless-Chromium pipeline occasionally freezes its
# rendering mid-take under load (the app itself is fine — verified against a
# raw pty). The tape also emits a .txt of the FINAL screen, so a good take is
# verifiable: the closing frame must show the scheduled-brief confirmation.
# Retry up to 3 times; fail loudly rather than shipping a frozen take.
for attempt in 1 2 3; do
    vhs docs/scripts/demo.tape
    if grep -q "meridian-daily-brief" docs/screenshots/tui/demo.txt; then
        echo "take $attempt: good (final frame shows the scheduled brief)"
        break
    fi
    echo "take $attempt: rendering froze mid-take — retrying" >&2
    [ "$attempt" = 3 ] && { echo "generate-tui-gif: all takes froze" >&2; exit 1; }
done
rm -f docs/screenshots/tui/demo.txt
ls -la docs/screenshots/tui/demo.gif
