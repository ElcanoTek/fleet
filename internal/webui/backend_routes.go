// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// Backend names the in-process handler a table-driven gateway row calls.
type Backend string

const (
	// BackendChat is the chat server (Options.Chat).
	BackendChat Backend = "chat"
	// BackendOrchestrator is the orchestrator (Options.Orchestrator).
	BackendOrchestrator Backend = "orchestrator"
)

// BackendRoute is one table-driven gateway row seen from the backend side:
// the browser pattern it answers and the upstream request it makes for a
// sample request, every path wildcard filled with "x1".
type BackendRoute struct {
	Backend Backend
	// Pattern is the gateway's own ServeMux pattern ("METHOD /api/...").
	Pattern string
	// Method is the upstream method (the gateway forwards it unchanged).
	Method string
	// Target is the upstream path, query string removed.
	Target string
}

// BackendRoutes lists every chatRoute and orchRoute row with the upstream path
// it produces. It exists for the cross-package drift test in cmd/fleet, which
// owns the real backend muxes: this package cannot import them, and a gateway
// row whose target no backend route serves is a browser feature that answers
// 404 while every mocked web test stays green.
//
// The rows with logic of their own (streams, files, login, discuss, models,
// brand) are not table rows and are not listed.
func BackendRoutes() []BackendRoute {
	chat, orch := chatRoutes(), orchRoutes()
	out := make([]BackendRoute, 0, len(chat)+len(orch))
	for _, rt := range chat {
		out = append(out, sampleBackendRoute(BackendChat, rt.pattern, rt.target))
	}
	for _, rt := range orch {
		out = append(out, sampleBackendRoute(BackendOrchestrator, rt.pattern, rt.target))
	}
	return out
}

func sampleBackendRoute(b Backend, pattern string, target func(*http.Request) string) BackendRoute {
	method, path, _ := strings.Cut(pattern, " ")
	r := &http.Request{Method: method, URL: &url.URL{Path: "/"}, Header: http.Header{}}
	for _, seg := range strings.Split(path, "/") {
		if name, ok := strings.CutPrefix(seg, "{"); ok {
			r.SetPathValue(strings.TrimSuffix(strings.TrimSuffix(name, "}"), "..."), "x1")
		}
	}
	t, _, _ := strings.Cut(target(r), "?")
	return BackendRoute{Backend: b, Pattern: pattern, Method: method, Target: t}
}
