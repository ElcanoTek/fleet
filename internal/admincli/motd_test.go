package admincli

import (
	"strings"
	"testing"
)

// TestRenderMOTD checks the login banner (#461): it carries the version, the
// unit, the service state, and the command hints — and emits ANSI escapes ONLY
// when color is on. The MOTD runs on every login, so it must stay fast and
// secret-free; this guards the no-escape-when-piped property a /etc/profile.d
// hook relies on.
func TestRenderMOTD(t *testing.T) {
	out := renderMOTD("1.2.3 (abc123)", "fleet.service", "active", false)
	for _, want := range []string{"fleet", "1.2.3 (abc123)", "fleet.service", "active", "fleet chat", "fleet update", "fleet --help"} {
		if !strings.Contains(out, want) {
			t.Errorf("renderMOTD missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("color=false must emit no ANSI escapes, got:\n%q", out)
	}

	// color=true emits escapes; the dev-box (no systemd) state reads clearly.
	colored := renderMOTD("dev", "fleet.service", "n/a", true)
	if !strings.Contains(colored, "\x1b[") {
		t.Error("color=true should emit ANSI escapes")
	}
	if !strings.Contains(colored, "no systemd") {
		t.Errorf("n/a state should mention 'no systemd', got:\n%s", colored)
	}
}
