# Generating the README screenshots (Web + TUI)

The README embeds four automated PNG screenshots generated directly from the
shipped web bundle and terminal UI against deterministic, in-memory/mocked
state — **no live model, no API key, and no cost**:

- `docs/screenshots/web/chat.png` — **Chat** (*Talk it through*)
- `docs/screenshots/web/patrols.png` — **Patrols** (*Chats that check in on a schedule*)
- `docs/screenshots/web/orchestrator.png` — **Background tasks** (*Runs without a chat*)
- `docs/screenshots/tui/chat.png` — **Terminal chat (`fleet chat`)**

## Regenerating the automated screenshots

 Both generators run in CI via [`.github/workflows/screenshots.yml`](../.github/workflows/screenshots.yml) and can be run locally after any UI or TUI change:

```sh
# 1. Web screenshots (Chat, Patrols, Background tasks)
cd web && npm run test:e2e:screenshots && cd ..

# 2. TUI screenshot (fleet chat via charmbracelet/freeze)
./scripts/generate-tui-screenshot.sh
```

- **Web screenshots** (`web/e2e/screenshots/gui.spec.ts`): Playwright boots the
  production web bundle behind `cmd/web-e2e-server`, intercepts `/api/*` with
  deterministic fixtures, and captures `chat.png`, `patrols.png`, and
  `orchestrator.png` into `docs/screenshots/web/`.
- **TUI screenshot** (`scripts/generate-tui-screenshot.sh` +
  `internal/chattui/screenshot_gen_test.go`): renders a representative
  `fleet chat` frame from in-memory Bubble Tea model state and converts the
  ANSI output to `docs/screenshots/tui/chat.png` via `charmbracelet/freeze`.

## Optional: recording animated demos (TUI + web)

If you need an animated GIF or video for release notes or talks, the opt-in
recording scripts remain available in `docs/scripts/`.

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
an optional `docs/screenshots/tui/demo.gif` (1080×620 px) and `demo.png` still
for release notes or presentations (not committed to the repo by default; the
README uses the automated static PNG `docs/screenshots/tui/chat.png` from
`scripts/generate-tui-screenshot.sh` + `freeze`).


## The web demo GIFs

The opt-in web recording scripts capture the current web app (the SPA behind the
Go gateway) with deterministic, fictional API responses for release notes or
demos, without requiring live model credentials or incurring model cost.

```sh
cd web && npm ci && npx playwright install chromium && cd ..
node docs/scripts/record-web-demos.mjs
docs/scripts/generate-web-gifs.sh /tmp/fleet-web-demos/chat.webm /tmp/fleet-web-demos/ops.webm
```

Playwright starts the app, creates a throwaway authenticated session, records
the kickoff conversation and Background tasks view, and asserts the expected
content before saving each video. A missing tab or reply fails the recording.
The capture lives in `web/e2e/screenshots/demos.spec.ts` and is opt-in, so normal
screenshot jobs do not record videos. `DEMO_OUT_DIR` overrides the video directory.
The harness always drives the production build (the Go gateway serving the
embedded bundle); `E2E_SKIP_BUILD=1` reuses the last one.

The converter uses ffmpeg and optionally gifsicle. Both the web and terminal
demo recordings use deterministic SSE fixtures, so neither requires credentials
or incurs model cost.

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
