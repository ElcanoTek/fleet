// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/handlers"
)

// TestOrchestratorClientIPHonorsTrustedProxies pins the FLEET_TRUSTED_PROXIES
// contract on the real buildOrchestratorMux through the login rate limiter
// (20 attempts per minute per client IP), which keys on getClientIP
// (GHSA-j6qf-3m9c-vwj3). Each attempt carries a fresh X-Forwarded-For value:
//
//   - nil trusted proxies (the default): the header is never consulted, so
//     every attempt from one TCP peer shares its bucket and the 21st is
//     throttled — a direct client cannot rotate its identity via the header.
//   - 127.0.0.1 trusted: a request from that peer is keyed by its XFF
//     client, so 21 distinct proxied clients are each within their budget.
func TestOrchestratorClientIPHonorsTrustedProxies(t *testing.T) {
	const host = "fleet.example.com"
	attempts := func(trusted []net.IP, remoteAddr string) (throttled int) {
		h := handlers.New(handlers.Config{}, nil, nil)
		mux := buildOrchestratorMux(h, handlers.NewNotesHandlers(nil, h), reloadConfigHandler(nil), mcpReloadHandler(nil), trusted)
		for i := range 21 {
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.Host = host
			req.Header.Set("Origin", "https://"+host)
			req.RemoteAddr = remoteAddr
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i+1))
			rr := httptest.NewRecorder()
			mux.ServeHTTP(rr, req)
			if rr.Code == http.StatusTooManyRequests {
				throttled++
			}
		}
		return throttled
	}

	if got := attempts(nil, "203.0.113.9:4242"); got != 1 {
		t.Errorf("no trusted proxies: %d of 21 spoofed-XFF attempts throttled, want 1 (keyed by RemoteAddr)", got)
	}
	if got := attempts([]net.IP{net.ParseIP("127.0.0.1")}, "203.0.113.9:4242"); got != 1 {
		t.Errorf("untrusted peer with trusted proxies set: %d of 21 throttled, want 1 (XFF ignored)", got)
	}
	if got := attempts([]net.IP{net.ParseIP("127.0.0.1")}, "127.0.0.1:4242"); got != 0 {
		t.Errorf("trusted peer: %d of 21 throttled, want 0 (keyed by each XFF client)", got)
	}
}
