// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package boxdoctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCaddyfile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCheckCaddyfile pins the four verdicts and the skip: the regression this
// check exists for is a fleet-managed Caddyfile that predates the API routes
// (every documented API URL 404s at the web tier) — that is a FAIL with the
// doctor fix; an operator's own front is advisory at most.
func TestCheckCaddyfile(t *testing.T) {
	const orch = "127.0.0.1:8000"

	if c := checkCaddyfile(filepath.Join(t.TempDir(), "missing"), orch); c.Status != StatusSkip {
		t.Errorf("absent file: got %s (%s), want skip", c.Status, c.Detail)
	}

	current := CaddyMarker + "\nfleet.example.com {\n\t@fleet_api path /v1 /v1/* /api-info\n\thandle @fleet_api {\n\t\treverse_proxy 127.0.0.1:8000 {\n\t\t\theader_up -X-Orchestrator-Server-Token\n\t\t}\n\t}\n\thandle {\n\t\treverse_proxy 127.0.0.1:3000\n\t}\n}\n"
	if c := checkCaddyfile(writeCaddyfile(t, current), orch); c.Status != StatusOK {
		t.Errorf("current managed layout: got %s (%s), want ok", c.Status, c.Detail)
	}

	// The pre-fix bootstrap output: marker + a single reverse_proxy to Next.
	stale := CaddyMarker + "\nfleet.example.com {\n\tencode zstd gzip\n\treverse_proxy 127.0.0.1:3000 {\n\t\tflush_interval -1\n\t}\n}\n"
	c := checkCaddyfile(writeCaddyfile(t, stale), orch)
	if c.Status != StatusFail || !strings.Contains(c.Fix, "sudo fleet doctor") {
		t.Errorf("stale managed layout: got %s fix=%q, want fail with the doctor fix (%s)", c.Status, c.Fix, c.Detail)
	}

	// Managed, routes /v1, but forwards the header-trust channel: not ok —
	// the impersonation path would be reachable from the internet.
	leaky := CaddyMarker + "\nfleet.example.com {\n\t@fleet_api path /v1/*\n\thandle @fleet_api {\n\t\treverse_proxy 127.0.0.1:8000\n\t}\n\treverse_proxy 127.0.0.1:3000\n}\n"
	if c := checkCaddyfile(writeCaddyfile(t, leaky), orch); c.Status != StatusFail {
		t.Errorf("managed without header stripping: got %s (%s), want fail", c.Status, c.Detail)
	}

	foreignRoutes := "fleet.example.com {\n\thandle /v1/* {\n\t\treverse_proxy 127.0.0.1:8000\n\t}\n\treverse_proxy 127.0.0.1:3000\n}\n"
	if c := checkCaddyfile(writeCaddyfile(t, foreignRoutes), orch); c.Status != StatusOK {
		t.Errorf("operator front that routes /v1: got %s (%s), want ok", c.Status, c.Detail)
	}

	foreignNoAPI := "fleet.example.com {\n\treverse_proxy 127.0.0.1:3000\n}\n"
	c = checkCaddyfile(writeCaddyfile(t, foreignNoAPI), orch)
	if c.Status != StatusWarn || c.Fix == "" {
		t.Errorf("operator front without /v1: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}

	// A moved orchestrator listener must be looked for at ITS address.
	moved := CaddyMarker + "\nfleet.example.com {\n\t@fleet_api path /v1/*\n\thandle @fleet_api {\n\t\treverse_proxy 127.0.0.1:9000 {\n\t\t\theader_up -X-Orchestrator-Server-Token\n\t\t}\n\t}\n}\n"
	if c := checkCaddyfile(writeCaddyfile(t, moved), "127.0.0.1:9000"); c.Status != StatusOK {
		t.Errorf("moved orchestrator addr: got %s (%s), want ok", c.Status, c.Detail)
	}
	if c := checkCaddyfile(writeCaddyfile(t, moved), orch); c.Status != StatusFail {
		t.Errorf("moved addr vs default: got %s (%s), want fail", c.Status, c.Detail)
	}
}

func TestCaddyfilePathAndOrchestratorAddr(t *testing.T) {
	t.Setenv("FLEET_CADDYFILE", "")
	t.Setenv("FLEET_ORCHESTRATOR_ADDR", "")
	if got := caddyfilePath(); got != "/etc/caddy/Caddyfile" {
		t.Errorf("default path = %q", got)
	}
	if got := orchestratorAddr(); got != "127.0.0.1:8000" {
		t.Errorf("default orch addr = %q", got)
	}
	t.Setenv("FLEET_CADDYFILE", "/tmp/x/Caddyfile")
	t.Setenv("FLEET_ORCHESTRATOR_ADDR", "127.0.0.1:9000")
	if got := caddyfilePath(); got != "/tmp/x/Caddyfile" {
		t.Errorf("override path = %q", got)
	}
	if got := orchestratorAddr(); got != "127.0.0.1:9000" {
		t.Errorf("override orch addr = %q", got)
	}
}
