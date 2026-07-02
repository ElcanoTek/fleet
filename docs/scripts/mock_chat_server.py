#!/usr/bin/env python3
"""Deterministic mock fleet chat server for the TUI demo GIF (#540).

Serves exactly the two endpoints `fleet chat` touches — GET /healthz and
POST /chat (SSE) — and streams a scripted, realistic-looking exchange with
human-readable pacing. No model is invoked, no credentials are read, nothing
is billed: the demo is free and fully reproducible.

Turn script (keyed off how many turns this process has served):
  1. a data question → two tool calls (bash, run_python) then a markdown
     answer streamed in word-sized deltas,
  2. a follow-up → a short streamed answer, no tools.

Usage: python3 mock_chat_server.py <port>   (generate_tui_gif.py starts it)
"""

import json
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

ANSWER_1 = """**3** scheduled tasks failed in the last 24h:

| task | failure |
| --- | --- |
| `nightly-etl` | upstream API returned 503 |
| `weekly-digest` | SMTP timeout |
| `catalog-sync` | schema drift on `products` |

The `nightly-etl` failure is transient — I'd retry it first. Want me to
re-queue all three?"""

ANSWER_2 = """Done — re-queued `nightly-etl` with a 5-minute backoff. It's
scheduled to run next at **09:05 UTC**. I'll leave the other two for your
review since their failures look non-transient."""


def sse(handler, event, data):
    handler.wfile.write(f"event: {event}\n".encode())
    handler.wfile.write(f"data: {json.dumps(data)}\n\n".encode())
    handler.wfile.flush()


class Handler(BaseHTTPRequestHandler):
    turns_served = 0

    def log_message(self, *args):  # keep the recording's stderr quiet
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if self.path != "/chat":
            self.send_response(404)
            self.end_headers()
            return
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)  # body content doesn't matter to the script

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        Handler.turns_served += 1
        sse(self, "conversation", {"id": "demo1234abcd"})
        sse(self, "turn.started", {})

        if Handler.turns_served == 1:
            sse(self, "tool.call", {"name": "bash"})
            time.sleep(1.1)
            sse(self, "tool.result", {"is_err": False})
            sse(self, "tool.call", {"name": "run_python"})
            time.sleep(1.3)
            sse(self, "tool.result", {"is_err": False})
            time.sleep(0.4)
            answer = ANSWER_1
        else:
            time.sleep(0.8)
            answer = ANSWER_2

        # Stream the answer in word-sized deltas so the GIF shows live text.
        words = answer.split(" ")
        for i, w in enumerate(words):
            chunk = w if i == len(words) - 1 else w + " "
            sse(self, "text.delta", {"text": chunk})
            time.sleep(0.045)

        sse(self, "turn.completed", {})


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8199
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
