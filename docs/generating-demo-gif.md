# Generating the demo GIFs (TUI + web)

The README demonstrates `fleet chat` with an animated GIF
(`docs/screenshots/tui/demo.gif`) showing a real two-turn session: a question,
live tool calls (`bash`, `run_python`) resolving to ✓, a glamour-rendered
markdown answer streaming in, `/help`, and a follow-up. This page documents how
that GIF is produced **deterministically, with no model, no API key, and no
cost** — so anyone can regenerate it after a TUI change.

## How it works

```
┌──────────────┐   vhs tape    ┌──────────────┐   HTTP/SSE   ┌─────────────────────┐
│  vhs (ttyd + │ ────keys────▶ │  fleet chat  │ ───────────▶ │ mock_chat_server.py │
│   Chromium)  │ ◀───pixels─── │  (real TUI)  │ ◀──frames─── │  (canned SSE turns) │
└──────────────┘               └──────────────┘              └─────────────────────┘
```

1. **A mock chat server** ([`scripts/mock_chat_server.py`](scripts/mock_chat_server.py))
   serves the only two endpoints the TUI touches — `GET /healthz` and
   `POST /chat` — and streams a scripted exchange as SSE with human-readable
   pacing: `conversation` → `tool.call`/`tool.result` pairs → word-sized
   `text.delta`s → `turn.completed`. The second POST gets a shorter,
   tool-free reply, so the recording shows both shapes. Being canned, the
   demo never leaks a real conversation and renders identically every run.
2. **The real binary, really recorded.** [`scripts/demo.tape`](scripts/demo.tape)
   is a [vhs](https://github.com/charmbracelet/vhs) script: it launches the
   actual `./fleet chat` (built from your checkout) inside vhs's headless
   terminal, types the demo messages at 35 ms/keystroke, and sleeps long
   enough for a viewer to read each beat. vhs renders true pixels via a real
   terminal emulator, so what you see is exactly what a user gets — colors,
   pills, borders, spinner and all.
   *(An earlier pty+pyte+Pillow pipeline — the approach middle-manager uses —
   was tried first; bubbletea v2's diff renderer confuses pyte's emulation
   into stale-cell tearing, so fleet records with vhs instead.)*
3. **Setup is hidden.** The tape starts the mock server and exports the dummy
   `FLEET_SERVER_TOKEN`/`FLEET_USER_EMAIL` inside a `Hide` block, so the GIF
   opens directly on the TUI. It ends by holding the finished transcript for
   3.5 s, then quits inside another `Hide` — recording the alt-screen teardown
   would capture restore garbage.

## Regenerating

```sh
# Fedora: sudo dnf install vhs ttyd ffmpeg    (vhs drives ttyd + ffmpeg)
docs/scripts/generate-tui-gif.sh
```

The script builds `./fleet`, sets `VHS_NO_SANDBOX=true` when running as root
(headless Chromium refuses the sandbox as root), runs the tape, and writes
`docs/screenshots/tui/demo.gif` (1080×620 px).

If you change the TUI's layout, keybindings, or the demo script, re-run the
generator and eyeball the GIF end to end — timing is the only fragile part
(the `Sleep`s in the tape must outlast the mock server's streaming delays).

The static PNG screenshot (`docs/screenshots/tui/chat.png`, from
`scripts/generate-tui-screenshot.sh` + `freeze`) still exists for contexts
where an animation is inappropriate; the README embeds the GIF.


## The web demo GIFs

The GIFs record the current Next.js app with deterministic, fictional API
responses, just like the mocked browser suite. They demonstrate the interface;
they do not certify live model, sandbox, or scheduler execution. The README
labels them accordingly. The separate live e2e suite covers that integration.

```sh
cd web && npm ci && npx playwright install chromium && cd ..
node docs/scripts/record-web-demos.mjs
docs/scripts/generate-web-gifs.sh /tmp/fleet-web-demos/chat.webm /tmp/fleet-web-demos/ops.webm
```

Playwright starts the app, creates a throwaway authenticated session, records
the kickoff conversation and Operations Center, and asserts the expected
content before saving each video. A missing tab or reply fails the recording.
The capture lives in `web/e2e/screenshots/demos.spec.ts` and is opt-in, so normal
screenshot jobs do not record videos. `DEMO_OUT_DIR` overrides the video directory.
Use `E2E_PROD=1` after `make ci-web` to capture the production build.

The converter uses ffmpeg and optionally gifsicle. Inspect the videos and the
`chat-final.png` / `ops-final.png` captures before committing all three GIFs.
The web conversation uses a scripted completed response; the terminal demo
uses paced SSE frames. Neither requires credentials or incurs model cost.

## The bug the recordings caught

The first TUI takes froze mid-answer or typed garbage like
`]11;rgb:1717/…[13;22R` into the composer. That was a REAL product bug, not a
recording artifact: glamour's `WithAutoStyle()` raw-queries the terminal
(OSC 11) on first render, mid-session, while bubbletea owns stdin — the reply
raced bubbletea's input parser on every real terminal. The fix (in
`internal/chattui`): the TUI resolves dark/light via bubbletea's own
`RequestBackgroundColor` handshake and always hands glamour an explicit style.
If you touch the TUI's rendering, re-record the GIF — it is an honest
end-to-end test of the terminal I/O path.
