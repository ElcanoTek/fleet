package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/webui"
)

func TestWebAddr(t *testing.T) {
	// The padded value checks that surrounding whitespace is trimmed.
	for _, c := range []struct{ v, want string }{{"", defaultWebAddr}, {"\t0.0.0.0:3000\n", "0.0.0.0:3000"}} {
		t.Setenv("FLEET_WEB_ADDR", c.v)
		if got, on := webAddr(); !on || got != c.want {
			t.Errorf("%q → %q %v", c.v, got, on)
		}
	}
	for _, v := range []string{"off", "OFF", "none", "disabled", "0"} {
		t.Setenv("FLEET_WEB_ADDR", v)
		if _, on := webAddr(); on {
			t.Errorf("%q left the web tier on", v)
		}
	}
}

func TestPrintWebBundleReportsTheEmbeddedBundle(t *testing.T) {
	var out bytes.Buffer
	code := printWebBundle(&out)
	if id := webui.EmbeddedBuildID(); id == "" {
		if code != 1 || !strings.Contains(out.String(), "missing") {
			t.Fatalf("%d %q", code, out.String())
		}
	} else if code != 0 || strings.TrimSpace(out.String()) != "web bundle: "+id {
		t.Fatalf("%d %q", code, out.String())
	}
}
