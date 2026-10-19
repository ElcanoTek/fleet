// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package boxdoctor

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"strings"
)

// CaddyMarker is line 1 of every Caddyfile scripts/bootstrap.sh has ever
// written; it is how a fleet-managed TLS front is told apart from an
// operator's own. It MUST equal CADDY_MARKER in scripts/lib/caddyfile.sh (the
// one renderer) — internal/admincli's TestCaddyfileMarkerParity pins that.
const CaddyMarker = "# Managed by fleet (scripts/bootstrap.sh) — re-runs overwrite this file."

// caddyfilePath is the installed Caddyfile; FLEET_CADDYFILE overrides it
// (doctor.sh honours the same knob, and tests point it at a temp file).
func caddyfilePath() string {
	if p := strings.TrimSpace(os.Getenv("FLEET_CADDYFILE")); p != "" {
		return p
	}
	return "/etc/caddy/Caddyfile"
}

// orchestratorAddr mirrors cmd/fleet's resolver: FLEET_ORCHESTRATOR_ADDR,
// else the loopback default the deploy docs promise.
func orchestratorAddr() string {
	if a := strings.TrimSpace(os.Getenv("FLEET_ORCHESTRATOR_ADDR")); a != "" {
		return a
	}
	return "127.0.0.1:8000"
}

// checkCaddyfile reports whether the TLS front routes the public HTTP API to
// the Go backends. The failure it exists for: a Caddyfile that only knows the
// web tier sends /v1/…, /api-info, the A2A agent card, /triggers/… and
// /webhooks/… to Next.js, which 404s them — "the API isn't routing" while
// every unit is green. It is a structural read (the shell renderer in
// scripts/lib/caddyfile.sh is the source of truth and doctor.sh diffs against
// it); here we only need the three facts an admin cannot see from the UI: is
// the file fleet's, does it carry a /v1 route to the orchestrator, and does it
// strip the header-trust channel on the way (ADR-0053). A file fleet did not
// write is advisory-only — an operator front that routes no /v1 may be
// deliberate, so it warns and never fails the box.
func checkCaddyfile(path, orchAddr string) Check {
	c := Check{Name: "caddy: API routing"}
	body, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured path (FLEET_CADDYFILE or the fixed default), never request input.
	if errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Detail = StatusSkip, path+" absent (no Caddy front on this box)"
		return c
	}
	if err != nil {
		c.Status, c.Detail = StatusSkip, path+" not readable from this process: "+firstLine(err.Error())
		return c
	}
	s := string(body)
	managed := strings.Contains(s, CaddyMarker)
	routesAPI := strings.Contains(s, "/v1/*") && strings.Contains(s, orchAddr)
	stripsTrust := strings.Contains(s, "header_up -X-Orchestrator-Server-Token")
	switch {
	case managed && routesAPI && stripsTrust:
		c.Status, c.Detail = StatusOK, path+" (fleet-managed) routes /v1 + the fixed API paths to the orchestrator at "+orchAddr+", header-trust stripped"
	case managed:
		c.Status = StatusFail
		c.Detail = path + " is fleet-managed but predates the API routes — /v1/*, /api-info, the A2A agent card, /triggers/* and /webhooks/* reach the web tier and 404 there"
		c.Fix = sudoDoctorFix + "   (rewrites it from scripts/lib/caddyfile.sh, backup kept, reloads caddy; or: sudo fleet update --adopt-units)"
	case routesAPI:
		c.Status, c.Detail = StatusOK, path+" (operator-managed) routes /v1 to the orchestrator at "+orchAddr
	default:
		c.Status = StatusWarn
		c.Detail = path + " is operator-managed and routes no /v1 to " + orchAddr + " — API clients get the web tier's 404 unless another front serves the API"
		c.Fix = "merge the @fleet_api + @fleet_chat_webhooks blocks from deploy/Caddyfile into it, then: systemctl reload caddy"
	}
	return c
}

// trustedProxiesFromEnv parses FLEET_TRUSTED_PROXIES the way config.Load does
// (comma-separated bare IPs), for a caller that did not hand Run the process
// config. A malformed entry is skipped here rather than failing: config.Load
// already refuses to boot on one, so this path only ever sees valid input.
func trustedProxiesFromEnv() []net.IP {
	var out []net.IP
	for _, p := range strings.Split(os.Getenv("FLEET_TRUSTED_PROXIES"), ",") {
		if ip := net.ParseIP(strings.TrimSpace(p)); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}

// checkTrustedProxies reports whether, behind a fleet-managed Caddy front,
// the running process trusts Caddy for X-Forwarded-For. Caddy dials both Go
// listeners from 127.0.0.1, so without it in FLEET_TRUSTED_PROXIES every
// client looks like 127.0.0.1: per-client rate limits share one bucket and
// FLEET_IP_ALLOWLIST/DENYLIST judge Caddy. That degrades the box without
// breaking it — requests are still served — so it is a WARN, like the other
// degraded-not-down checks, never a fail. trusted is the process's own
// cfg.TrustedProxies (what it actually resolves client IPs with), so an env
// file edited since the last restart reads as not yet applied, which is true.
// An operator-managed front is skipped: its proxy addresses are the
// operator's to name, and doctor.sh never writes them either.
func checkTrustedProxies(path string, trusted []net.IP) Check {
	c := Check{Name: "caddy: trusted proxies"}
	body, err := os.ReadFile(path) //nolint:gosec // G304: operator-configured path (FLEET_CADDYFILE or the fixed default), never request input.
	if errors.Is(err, fs.ErrNotExist) {
		c.Status, c.Detail = StatusSkip, path+" absent (no Caddy front on this box)"
		return c
	}
	if err != nil {
		c.Status, c.Detail = StatusSkip, path+" not readable from this process: "+firstLine(err.Error())
		return c
	}
	if !strings.Contains(string(body), CaddyMarker) {
		c.Status, c.Detail = StatusSkip, path+" is operator-managed — FLEET_TRUSTED_PROXIES should name the addresses that front connects from"
		return c
	}
	loopback := net.IPv4(127, 0, 0, 1)
	names := make([]string, 0, len(trusted))
	for _, ip := range trusted {
		names = append(names, ip.String())
		if ip.Equal(loopback) {
			c.Status, c.Detail = StatusOK, "FLEET_TRUSTED_PROXIES includes Caddy's 127.0.0.1 — the backends read the client IP it forwards"
			return c
		}
	}
	c.Status = StatusWarn
	if len(trusted) == 0 {
		c.Detail = "FLEET_TRUSTED_PROXIES is unset in the running process behind the fleet-managed Caddy front — every client looks like 127.0.0.1 (shared rate limits; the IP allow/deny filter judges Caddy)"
	} else {
		c.Detail = "FLEET_TRUSTED_PROXIES=" + strings.Join(names, ",") + " does not include Caddy's 127.0.0.1 — behind the fleet-managed front every client looks like 127.0.0.1 (shared rate limits; the IP allow/deny filter judges Caddy)"
	}
	c.Fix = sudoDoctorFix + "   (adds 127.0.0.1,::1 when the key is absent, then restarts fleet; or: sudo fleet config set-env FLEET_TRUSTED_PROXIES --value 127.0.0.1,::1 && sudo fleet restart)"
	return c
}
