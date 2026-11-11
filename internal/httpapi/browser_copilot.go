package httpapi

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

func (s *Server) handleBrowserSessions(w http.ResponseWriter, r *http.Request) {
	// Path shape: /browser-sessions/{id}/{action}
	rest := strings.TrimPrefix(r.URL.Path, "/browser-sessions/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sessionID := strings.TrimSpace(parts[0])
	action := strings.ToLower(strings.TrimSpace(parts[1]))
	userEmail := userFromCtx(r.Context())
	viewToken := strings.TrimSpace(r.URL.Query().Get("token"))

	sess, err := playwrightrunner.DefaultSessionManager().GetSession(sessionID, userEmail, viewToken)
	if err != nil {
		// One answer whatever the reason (gone, closed, someone else's), so a
		// session id does not tell anyone whether that session is still live.
		// The view is the link a person opens from chat, so it gets a page;
		// the endpoints its script calls get the same answer as JSON.
		if action == "view" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			serveBrowserSessionUnavailable(w)
			return
		}
		writeJSONStatus(w, r, http.StatusNotFound, map[string]string{"error": "this live browser session is no longer available"})
		return
	}

	switch action {
	case "view":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveBrowserSessionView(w, r, sess, viewToken)
	case "copilot.js":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(browserCopilotScript))
	case "stream":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveBrowserSessionStream(w, r, sess)
	case "input":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleBrowserSessionInput(w, r, sess)
	case "close":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_ = playwrightrunner.DefaultSessionManager().CloseSession(sessionID, userEmail, viewToken)
		writeJSON(w, r, map[string]any{"ok": true, "session_id": sessionID})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) handleBrowserSessionInput(w http.ResponseWriter, r *http.Request, sess *playwrightrunner.LiveSession) {
	var ev playwrightrunner.InputEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&ev); err != nil {
		writeJSONStatus(w, r, http.StatusBadRequest, map[string]string{"error": "invalid input payload"})
		return
	}
	if err := sess.SendInput(r.Context(), ev); err != nil {
		writeJSONStatus(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	info := sess.Info(nil)
	writeJSON(w, r, map[string]any{
		"ok":          true,
		"current_url": info.CurrentURL,
		"title":       info.Title,
	})
}

func (s *Server) serveBrowserSessionStream(w http.ResponseWriter, r *http.Request, sess *playwrightrunner.LiveSession) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	frames, unsub, err := sess.SubscribeFrames()
	if err != nil {
		writeJSONStatus(w, r, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-frames:
			if !open {
				_, _ = fmt.Fprint(w, "event: closed\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			raw, merr := json.Marshal(frame)
			if merr != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "event: frame\ndata: %s\n\n", raw)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// serveBrowserSessionUnavailable answers a live-view link whose session is
// gone or is not the caller's, with one page for every case.
func serveBrowserSessionUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(browserSessionUnavailablePage))
}

const browserSessionUnavailablePage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Live browser session unavailable — Fleet</title>
<style>
  :root { color-scheme: dark; }
  body {
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    background: #0b0e14;
    color: #e6edf3;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  main { max-width: 32rem; padding: 24px; line-height: 1.5; }
  h1 { font-size: 20px; margin: 0 0 12px; }
  p { color: #8b949e; margin: 0 0 16px; }
  a { color: #3b82f6; }
</style>
</head>
<body>
<main>
  <h1>This live browser session is no longer available</h1>
  <p>It has ended, it was closed after a while without activity, or it belongs to someone else.</p>
  <p>Go back to your chat and ask the assistant to open a new live browser session if you still need one.</p>
  <p><a href="/chat">Back to chat</a></p>
</main>
</body>
</html>
`

func (s *Server) serveBrowserSessionView(w http.ResponseWriter, _ *http.Request, sess *playwrightrunner.LiveSession, viewToken string) {
	info := sess.Info(nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")

	sid := html.EscapeString(info.ID)
	tok := html.EscapeString(viewToken)
	scriptSrc := "/api/browser-sessions/" + url.PathEscape(info.ID) + "/copilot.js"
	if viewToken != "" {
		scriptSrc += "?token=" + url.QueryEscape(viewToken)
	}
	scriptSrcAttr := html.EscapeString(scriptSrc)
	initURL := html.EscapeString(info.CurrentURL)
	initTitle := html.EscapeString(info.Title)
	if initTitle == "" {
		initTitle = "Co-Pilot Browser Session"
	}

	pageHTML := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>%s — Fleet Co-Pilot Browser</title>
<style>
  :root {
    color-scheme: dark;
    --bg: #0b0e14;
    --panel: #131822;
    --border: #252e3f;
    --text: #e6edf3;
    --muted: #8b949e;
    --accent: #3b82f6;
    --success: #10b981;
    --danger: #ef4444;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    display: flex;
    flex-direction: column;
    height: 100vh;
    overflow: hidden;
  }
  header {
    background: var(--panel);
    border-bottom: 1px solid var(--border);
    padding: 10px 16px;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 10px;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    font-weight: 600;
    padding: 4px 10px;
    border-radius: 999px;
    background: rgba(16, 185, 129, 0.14);
    color: #34d399;
    border: 1px solid rgba(16, 185, 129, 0.35);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%%;
    background: var(--success);
  }
  .nav-group {
    display: flex;
    align-items: center;
    gap: 6px;
    flex: 1;
    min-width: 260px;
  }
  button {
    background: #1c2433;
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 6px 11px;
    font-size: 13px;
    font-weight: 500;
    cursor: pointer;
  }
  button:hover { background: #263147; }
  button.primary {
    background: var(--accent);
    border-color: var(--accent);
    color: #fff;
  }
  button.success {
    background: var(--success);
    border-color: var(--success);
    color: #052e16;
    font-weight: 600;
  }
  button.danger {
    background: rgba(239, 68, 68, 0.16);
    border-color: rgba(239, 68, 68, 0.4);
    color: #fca5a5;
  }
  input[type="text"] {
    background: #0d1117;
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 6px 10px;
    font-size: 13px;
  }
  #url-bar { flex: 1; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .type-bar {
    background: #0f141d;
    border-bottom: 1px solid var(--border);
    padding: 8px 16px;
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 12px;
    color: var(--muted);
  }
  #paste-input { width: 280px; }
  #banner {
    display: none;
    background: rgba(16, 185, 129, 0.18);
    border-bottom: 1px solid rgba(16, 185, 129, 0.4);
    color: #a7f3d0;
    padding: 10px 16px;
    font-size: 13px;
    text-align: center;
  }
  main {
    position: relative;
    flex: 1;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 12px;
    overflow: auto;
  }
  #stage-wrap {
    position: relative;
    aspect-ratio: 16 / 9;
    max-width: 100%%;
    max-height: 100%%;
    border: 1px solid var(--border);
    border-radius: 8px;
    overflow: hidden;
    background: #000;
    box-shadow: 0 12px 40px rgba(0,0,0,0.6);
    outline: none;
  }
  #stage-wrap:focus {
    border-color: var(--accent);
    box-shadow: 0 0 0 2px rgba(59, 130, 246, 0.35);
  }
  #screen {
    display: block;
    width: 100%%;
    height: 100%%;
    user-select: none;
    -webkit-user-drag: none;
    cursor: crosshair;
  }
  #notice {
    position: absolute;
    top: 8px;
    left: 50%%;
    transform: translateX(-50%%);
    z-index: 2;
    max-width: calc(100%% - 32px);
    background: rgba(48, 16, 18, 0.96);
    border: 1px solid rgba(239, 68, 68, 0.6);
    border-radius: 6px;
    color: var(--text);
    font-size: 13px;
    padding: 8px 12px;
    overflow-wrap: anywhere;
    pointer-events: none;
  }
  #notice:empty {
    padding: 0;
    border: 0;
    background: none;
  }
  #empty-hint {
    position: absolute;
    inset: 0;
    display: flex;
    align-items: center;
    justify-content: center;
    color: var(--muted);
    font-size: 14px;
    pointer-events: none;
  }
</style>
</head>
<body data-session-id="%s" data-token="%s">
<header>
  <span class="badge" id="status-badge"><span class="dot" id="status-dot"></span><span id="status-text">Co-Pilot Live Browser</span></span>
  <div class="nav-group">
    <button type="button" id="btn-back" title="Back">←</button>
    <button type="button" id="btn-reload" title="Reload">↻</button>
    <input type="text" id="url-bar" value="%s" aria-label="Browser URL" />
    <button type="button" id="btn-go">Go</button>
  </div>
  <button type="button" class="success" id="btn-done">✓ Done — Return to Chat</button>
  <button type="button" class="danger" id="btn-close">End Session</button>
</header>
<div class="type-bar">
  <span>Click directly on the browser below to focus &amp; type, or paste text/2FA code here:</span>
  <input type="text" id="paste-input" placeholder="Paste text, email, or 2FA code…" />
  <button type="button" class="primary" id="btn-send-text">Type Text</button>
  <button type="button" id="btn-enter">Enter ↵</button>
  <button type="button" id="btn-tab">Tab ⇥</button>
  <button type="button" id="btn-backspace">Backspace ⌫</button>
</div>
<div id="banner">
  <strong>Session ready!</strong> Your login &amp; cookies are active in this Co-Pilot session. Switch back to your Fleet Chat tab and reply (e.g. <em>"Done, I'm logged in"</em>) so the assistant can continue or save the connector.
</div>
<main>
  <div id="notice" role="status" aria-live="polite"></div>
  <div id="stage-wrap" tabindex="0" aria-label="Live Chromium viewport">
    <div id="empty-hint">Connecting to live Chromium session…</div>
    <img id="screen" alt="Live browser stream" />
  </div>
</main>
<script src="%s" defer></script>
</body>
</html>`, initTitle, sid, tok, initURL, scriptSrcAttr)

	_, _ = w.Write([]byte(pageHTML))
}

const browserCopilotScript = `(() => {
  const sessionId = document.body.dataset.sessionId || "";
  const token = document.body.dataset.token || "";
  const qs = token ? ("?token=" + encodeURIComponent(token)) : "";
  const basePath = "/api/browser-sessions/" + encodeURIComponent(sessionId);

  const screenEl = document.getElementById("screen");
  const hintEl = document.getElementById("empty-hint");
  const stageWrap = document.getElementById("stage-wrap");
  const urlBar = document.getElementById("url-bar");
  const statusText = document.getElementById("status-text");
  const statusDot = document.getElementById("status-dot");
  const banner = document.getElementById("banner");
  const pasteInput = document.getElementById("paste-input");
  const noticeEl = document.getElementById("notice");

  const VIEW_W = 1280;
  const VIEW_H = 720;

  // The notice line stays rendered (empty when there is nothing to say), so
  // a screen reader announces a message when its text appears. The same
  // message again only restarts its timer, so it is not re-announced.
  let noticeTimer = 0;
  let sessionEnded = false;
  let signedOut = false;
  function showNotice(message, sticky) {
    // Once the session has ended, its notice stays: a late reply to input
    // sent before it ended must not replace it.
    if (sessionEnded && !sticky) return;
    clearTimeout(noticeTimer);
    if (noticeEl.textContent !== message) noticeEl.textContent = message;
    if (!sticky) noticeTimer = setTimeout(() => { noticeEl.textContent = ""; }, 8000);
  }

  const ENDED = "This live browser session has ended. Go back to your chat and ask the assistant to open a new one.";
  let es = null;
  function endSession(message) {
    if (sessionEnded) return;
    showNotice(message, true);
    sessionEnded = true;
    statusText.textContent = "Session Ended";
    statusDot.style.background = "#ef4444";
    if (es) es.close();
  }

  // Pointer motion is sent many times a second; a refusal of one is noise, so
  // only an ended session or a lost connection is worth a notice.
  const quietInputs = { mousemove: true, wheel: true };

  // sendInput forwards one event to the live browser. A refused event is
  // shown in the notice line rather than dropped silently: the reason an
  // address was refused, an ended session (after which nothing more is
  // sent), a signed-out or refused request, or a lost connection.
  async function sendInput(payload) {
    if (sessionEnded) return;
    let resp;
    try {
      resp = await fetch(basePath + "/input" + qs, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
    } catch (_) {
      showNotice("Couldn't reach Fleet. Check your connection and try again.");
      return;
    }
    if (sessionEnded) return;
    if (resp.ok) {
      if (signedOut) {
        signedOut = false;
        noticeEl.textContent = "";
      }
      try {
        const data = await resp.json();
        if (data.current_url && document.activeElement !== urlBar) {
          urlBar.value = data.current_url;
        }
      } catch (_) {}
      return;
    }
    if (resp.status === 404) {
      endSession(ENDED);
      return;
    }
    if (resp.status === 401) {
      signedOut = true;
      showNotice("You're signed out of Fleet. Sign in again, then reload this page.", true);
      return;
    }
    if (quietInputs[payload.type]) return;
    if (resp.status === 403) {
      showNotice("Fleet didn't allow that action.");
      return;
    }
    if (payload.type === "navigate") {
      let detail = "";
      try {
        const data = await resp.json();
        detail = (data && data.error) || "";
      } catch (_) {}
      showNotice(detail ? "Couldn't open " + payload.url + ": " + detail : "Couldn't open " + payload.url + ".");
      return;
    }
    showNotice("The browser didn't take that action. Try again.");
  }

  function mapCoords(ev) {
    const rect = screenEl.getBoundingClientRect();
    if (!rect.width || !rect.height) return { x: 0, y: 0 };
    const x = Math.max(0, Math.min(VIEW_W, ((ev.clientX - rect.left) / rect.width) * VIEW_W));
    const y = Math.max(0, Math.min(VIEW_H, ((ev.clientY - rect.top) / rect.height) * VIEW_H));
    return { x: Math.round(x), y: Math.round(y) };
  }

  screenEl.addEventListener("click", (ev) => {
    stageWrap.focus();
    const { x, y } = mapCoords(ev);
    sendInput({ type: "click", x, y, button: "left", click_count: ev.detail || 1 });
  });

  screenEl.addEventListener("contextmenu", (ev) => {
    ev.preventDefault();
    stageWrap.focus();
    const { x, y } = mapCoords(ev);
    sendInput({ type: "click", x, y, button: "right", click_count: 1 });
  });

  let lastMove = 0;
  screenEl.addEventListener("mousemove", (ev) => {
    const now = Date.now();
    if (now - lastMove < 90) return;
    lastMove = now;
    const { x, y } = mapCoords(ev);
    sendInput({ type: "mousemove", x, y });
  });

  screenEl.addEventListener("wheel", (ev) => {
    ev.preventDefault();
    sendInput({ type: "wheel", delta_x: ev.deltaX, delta_y: ev.deltaY });
  }, { passive: false });

  stageWrap.addEventListener("keydown", (ev) => {
    if (ev.target !== stageWrap) return;
    if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "v") {
      return;
    }
    ev.preventDefault();
    if (ev.key && ev.key.length === 1 && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
      sendInput({ type: "text", text: ev.key });
    } else if (ev.key) {
      sendInput({ type: "keydown", key: ev.key });
    }
  });

  stageWrap.addEventListener("paste", (ev) => {
    const text = (ev.clipboardData || window.clipboardData).getData("text");
    if (text) {
      ev.preventDefault();
      sendInput({ type: "text", text });
    }
  });

  document.getElementById("btn-send-text").addEventListener("click", () => {
    const val = pasteInput.value;
    if (!val) return;
    sendInput({ type: "text", text: val });
    pasteInput.value = "";
    stageWrap.focus();
  });
  pasteInput.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") {
      ev.preventDefault();
      document.getElementById("btn-send-text").click();
    }
  });

  document.getElementById("btn-enter").addEventListener("click", () => sendInput({ type: "keydown", key: "Enter" }));
  document.getElementById("btn-tab").addEventListener("click", () => sendInput({ type: "keydown", key: "Tab" }));
  document.getElementById("btn-backspace").addEventListener("click", () => sendInput({ type: "keydown", key: "Backspace" }));
  document.getElementById("btn-back").addEventListener("click", () => sendInput({ type: "back" }));
  document.getElementById("btn-reload").addEventListener("click", () => sendInput({ type: "reload" }));
  document.getElementById("btn-go").addEventListener("click", () => {
    if (urlBar.value.trim()) sendInput({ type: "navigate", url: urlBar.value.trim() });
  });
  urlBar.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") {
      ev.preventDefault();
      document.getElementById("btn-go").click();
    }
  });

  document.getElementById("btn-done").addEventListener("click", () => {
    banner.style.display = "block";
  });

  document.getElementById("btn-close").addEventListener("click", async () => {
    try {
      await fetch(basePath + "/close" + qs, { method: "POST" });
    } catch (_) {
      showNotice("Couldn't reach Fleet, so the session is still open. Try again.");
      return;
    }
    endSession("You ended this live browser session.");
    window.close();
  });

  es = new EventSource(basePath + "/stream" + qs);
  es.addEventListener("frame", (ev) => {
    try {
      const f = JSON.parse(ev.data);
      if (f.data) {
        screenEl.src = "data:image/jpeg;base64," + f.data;
        hintEl.style.display = "none";
      }
      if (f.url && document.activeElement !== urlBar) {
        urlBar.value = f.url;
      }
      if (f.title) {
        document.title = f.title + " — Fleet Co-Pilot Browser";
      }
    } catch (_) {}
  });
  es.addEventListener("closed", () => endSession(ENDED));
  es.onerror = () => {
    if (sessionEnded) return;
    statusText.textContent = "Reconnecting…";
    // The stream stops retrying once the session is gone (its URL answers
    // 404); check, so the view says so without waiting for the next input.
    if (es.readyState === EventSource.CLOSED) {
      fetch(basePath + "/copilot.js" + qs, { method: "HEAD" })
        .then((r) => { if (r.status === 404) endSession(ENDED); })
        .catch(() => {});
    }
  };
})();
`
