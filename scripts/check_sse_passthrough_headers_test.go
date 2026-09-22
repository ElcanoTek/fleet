package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Both SSE proxy routes in web/ REBUILD the response header set instead of
// passing chat-server's through, so a header the Go side adds reaches the
// browser only if web/src/app/lib/sseHeaders.ts lists it. Nothing about that
// arrangement fails loudly when the list falls behind: the header is simply
// absent, and the client takes its "absent" branch.
//
// That is not hypothetical. X-Fleet-Heartbeat-Interval-Ms was advertised by
// internal/httpapi/capabilities.go and read by useTurnStream to size the
// stream-liveness watchdog, but neither proxy forwarded it. The client read
// absent as 0 — "keepalives are off, silence proves nothing" — which pins
// streamDeadSilenceMs at +Infinity and disables the missed-keepalive branch of
// checkStreamLiveness. Every Next-proxied deployment ran the watchdog in its
// weakest mode, and it looked healthy the whole time.
//
// So this test pins the Go header constants against the TypeScript list. A
// header added on one side and not the other fails here instead of degrading
// a client behaviour quietly months later.

// goSideHeaderConsts are the X-Fleet-* SSE response headers the Go server sets
// on a stream, paired with the file that declares each one. Adding a header
// here without adding it to sseHeaders.ts is the failure this test catches.
var goSideHeaderConsts = map[string]string{
	"X-Fleet-Heartbeat-Interval-Ms": filepath.Join("internal", "httpapi", "capabilities.go"),
	"X-Fleet-Conversation-Id":       filepath.Join("internal", "httpapi", "chat.go"),
}

func TestSSEPassthroughHeadersCoverEveryGoSideHeader(t *testing.T) {
	root := repoRoot(t)

	tsPath := filepath.Join("web", "src", "app", "lib", "sseHeaders.ts")
	raw, err := os.ReadFile(filepath.Join(root, tsPath))
	if err != nil {
		t.Fatalf("read %s: %v", tsPath, err)
	}
	// Match the passthroughHeaders ARRAY, not the file. The file also explains
	// the bug in prose and names the header there, so a substring search over
	// the whole source passes even with the entry deleted — measured, by
	// deleting it. The list is the contract; the comment is not.
	listed := passthroughHeaderList(t, string(raw))

	for header, declaredIn := range goSideHeaderConsts {
		// The Go side must really still set it — otherwise this table is the
		// thing that is stale, and a green test would be meaningless.
		goRaw, err := os.ReadFile(filepath.Join(root, declaredIn))
		if err != nil {
			t.Fatalf("read %s: %v", declaredIn, err)
		}
		if !strings.Contains(string(goRaw), header) {
			t.Errorf("%s no longer names %q — update goSideHeaderConsts in this test", declaredIn, header)
			continue
		}
		if !listed[header] {
			t.Errorf("%s does not forward %q, which %s sets.\n"+
				"The proxies rebuild their header set, so the browser will never see it.",
				tsPath, header, declaredIn)
		}
	}
}

// The list is only load-bearing if both proxy routes actually use it. A route
// that goes back to an inline header literal silently opts out of everything
// above, so pin the call instead of trusting the comment.
func TestBothSSEProxyRoutesUseTheSharedHeaderBuilder(t *testing.T) {
	root := repoRoot(t)

	routes := []string{
		filepath.Join("web", "src", "app", "api", "chat", "route.ts"),
		filepath.Join("web", "src", "app", "api", "conversations", "[conversationId]", "stream", "route.ts"),
	}
	inlineContentType := regexp.MustCompile(`"Content-Type":\s*"text/event-stream`)

	for _, rel := range routes {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		src := string(raw)
		if !strings.Contains(src, "sseProxyHeaders(upstream)") {
			t.Errorf("%s does not build its response headers with sseProxyHeaders(upstream)", rel)
		}
		if inlineContentType.MatchString(src) {
			t.Errorf("%s still spells the SSE headers inline; that is the drift this helper exists to stop", rel)
		}
	}
}

// passthroughHeaderList extracts the string entries of the passthroughHeaders
// array literal. Deliberately narrow: it reads the declaration, not the file,
// so prose that merely mentions a header name cannot satisfy the check.
func passthroughHeaderList(t *testing.T, src string) map[string]bool {
	t.Helper()

	const decl = "const passthroughHeaders = ["
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("sseHeaders.ts no longer declares %q — this guard cannot read the list", decl)
	}
	rest := src[start+len(decl):]
	end := strings.Index(rest, "]")
	if end < 0 {
		t.Fatal("sseHeaders.ts: passthroughHeaders array is not closed")
	}

	entries := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(rest[:end], -1)
	if len(entries) == 0 {
		t.Fatal("sseHeaders.ts: passthroughHeaders is empty")
	}
	out := make(map[string]bool, len(entries))
	for _, m := range entries {
		out[m[1]] = true
	}
	return out
}
