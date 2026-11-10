//go:build linux || darwin

package playwrightrunner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubBrowserWorker puts a python3 on PATH that stands in for both Playwright
// workers and behaves like Chromium on a refused tunnel: it asks the run's
// guard proxy for the target (curl, CONNECT through proxy_server), then
// reports Chromium's "net::ERR_TUNNEL_CONNECTION_FAILED at <url>". So the
// tests below exercise the real proxy and the real error paths of Run and of
// a live session, without a browser, in CI. Callers must not be parallel.
func stubBrowserWorker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	dir := t.TempDir()
	script := `#!/bin/sh
field() { printf '%s' "$1" | sed -n "s/.*\"$2\":\"\([^\"]*\)\".*/\1/p"; }
fail() { curl -q -s -o /dev/null --max-time 5 -x "$1" "$2"; }
if [ "$1" = "-c" ] && [ "$2" = "import playwright" ]; then exit 0; fi
if [ "$1" = "-u" ]; then
  while IFS= read -r line; do
    id=$(field "$line" req_id); url=$(field "$line" url); proxy=$(field "$line" proxy_server)
    [ -n "$proxy" ] && fail "$proxy" "$url"
    [ -n "$id" ] && printf '{"event":"cmd_result","req_id":"%s","ok":false,"error":"Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at %s\\nCall log:\\n  - navigating"}\n' "$id" "$url"
  done
  exit 0
fi
payload=$(cat)
url=$(field "$payload" url); proxy=$(field "$payload" proxy_server)
fail "$proxy" "$url"
printf '{"ok":false,"error":"Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at %s\\nCall log:\\n  - navigating"}\n' "$url"
`
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRefusedTunnelErrorsNameTheHostEndToEnd: Run and a live session start
// both report which host the guard refused and why, after the first line of
// the browser's error. Deleting the explain call in either path fails it.
func TestRefusedTunnelErrorsNameTheHostEndToEnd(t *testing.T) {
	stubBrowserWorker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const want = "Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at https://127.0.0.1:9/ (127.0.0.1:9 is a private or internal network address, which Fleet's browser does not open)\nCall log:"

	_, err := Run(ctx, RunRequest{URL: "https://127.0.0.1:9/"})
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Run error = %v\nwant it to start with %q", err, want)
	}

	info, err := NewSessionManager().StartSession(ctx, SessionStartRequest{OwnerEmail: "alice@example.com", URL: "https://127.0.0.1:9/"})
	if err == nil {
		t.Fatalf("StartSession succeeded (%s), want the refused tunnel", info.ID)
	}
	if !strings.HasPrefix(err.Error(), want) {
		t.Errorf("StartSession error = %v\nwant it to start with %q", err, want)
	}
}
