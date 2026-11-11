package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// TestBrowserSessionViewReportsRefusedInput pins the live view's notice line:
// the page carries a polite status region, and the view script shows a
// refused event there (the server's error for a refused address, a message
// for an ended session or a network failure) instead of dropping it. The
// script's behaviour in a browser was checked by hand; this guards the wiring.
func TestBrowserSessionViewReportsRefusedInput(t *testing.T) {
	t.Parallel()
	s := &Server{}
	req := httptest.NewRequestWithContext(asUser(context.Background(), "alice@example.com"), http.MethodGet, "/browser-sessions/x/view", nil)
	rec := httptest.NewRecorder()
	s.serveBrowserSessionView(rec, req, &playwrightrunner.LiveSession{}, "")
	page := rec.Body.String()
	// Always rendered (emptied, never hidden) so a screen reader announces the
	// first message, and inside <main>, floating over the stream, so it does
	// not move the screen under the cursor.
	notice := `<div id="notice" role="status" aria-live="polite"></div>`
	mainAt, noticeAt, stageAt := strings.Index(page, "<main>"), strings.Index(page, notice), strings.Index(page, `<div id="stage-wrap"`)
	if mainAt < 0 || noticeAt < mainAt || stageAt < noticeAt {
		t.Errorf("the notice line is not rendered first inside <main> (main %d, notice %d, stage %d):\n%s", mainAt, noticeAt, stageAt, page)
	}
	for _, want := range []string{
		`const noticeEl = document.getElementById("notice");`,
		`if (noticeEl.textContent !== message) noticeEl.textContent = message;`,
		`showNotice("Couldn't reach Fleet. Check your connection and try again.");`,
		`if (resp.status === 404) {`,
		`if (sessionEnded) return;`,
		`if (sessionEnded && !sticky) return;`,
		`endSession("You ended this live browser session.");`,
		`if (resp.status === 401) {`,
		`const quietInputs = { mousemove: true, wheel: true };`,
		`detail = (data && data.error) || "";`,
	} {
		if !strings.Contains(browserCopilotScript, want) {
			t.Errorf("the view script no longer reports refused input as designed (missing %q)", want)
		}
	}
	// Server text and the typed URL reach the page only as text.
	if strings.Contains(browserCopilotScript, "innerHTML") {
		t.Error("the view script writes HTML; use textContent")
	}
}
