package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
)

// ServerDef is a portable, transport-agnostic description of one MCP server
// entry (#218). It is what the agent layer diffs against the live registry to
// hot-reload servers: a Command makes it a stdio server, a URL an HTTP server.
// It mirrors the fields BuildMCPClient needs without pulling in the config-layer
// types, so internal/mcp stays dependency-free.
//
// NOTE: Env and Headers may contain RESOLVED secrets (e.g. an expanded
// ${API_KEY}). Never log a ServerDef — diff it in memory only.
type ServerDef struct {
	Name string
	// stdio
	Command string
	Args    []string
	Env     map[string]string
	Dir     string
	// http
	URL     string
	Headers map[string]string
	TLS     *TLSOptions
}

// ReloadSummary reports what a Reload changed. Names are sorted for
// determinism (stable output for the admin endpoint / CLI).
type ReloadSummary struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	Restarted []string `json:"restarted"`
	Unchanged []string `json:"unchanged"`
}

// serverDefEqual reports whether two defs describe the same server, treating a
// nil slice/map the same as an empty one (so a manifest re-read that produces
// an empty-vs-nil difference doesn't force a spurious restart).
func serverDefEqual(a, b ServerDef) bool {
	return a.Name == b.Name &&
		a.Command == b.Command &&
		a.Dir == b.Dir &&
		a.URL == b.URL &&
		eqStrings(a.Args, b.Args) &&
		maps.Equal(a.Env, b.Env) &&
		maps.Equal(a.Headers, b.Headers) &&
		reflect.DeepEqual(a.TLS, b.TLS)
}

// eqStrings compares two string slices, treating nil and empty as equal.
// slices.Equal (rather than a hand-rolled index loop) keeps this gosec-clean
// (no G602 bounds concern) and is used by tests too.
func eqStrings(a, b []string) bool { return slices.Equal(a, b) }

// buildServer creates and initializes a Server from a def WITHOUT registering it
// on the client (holds no lock); the caller inserts it under c.mu. It is the one
// constructor for every shape: Reload's manifest defs, AddStdioServer and
// AddHTTPServerWithOptions. hc, when non-nil, overrides the HTTP client (the
// per-user SSRF-safe client); that caller owns the full client, so def.TLS is
// then not applied. Otherwise a non-zero def.TLS hardens the handshake (#280).
//
// A failure of the transport or initialize step comes back as a
// *serverBuildError so each caller keeps its own wording (wrapBuildError).
func buildServer(ctx context.Context, def ServerDef, hc *http.Client) (*Server, error) {
	var transport Transport
	switch {
	case def.Command != "":
		t, err := NewStdioTransportInDir(def.Command, def.Args, def.Env, def.Dir)
		if err != nil {
			return nil, &serverBuildError{err: err}
		}
		transport = t
	case def.URL != "":
		t := NewHTTPTransportWithHeaders(def.URL, def.Headers)
		switch {
		case hc != nil:
			t.client = hc
		case def.TLS != nil && !def.TLS.IsZero():
			// Fail closed: a TLSClientConfig is applied by http.Transport only to
			// https requests, so requesting hardening for a plaintext url would
			// silently connect unverified. Refuse to register rather than mislead.
			if !strings.HasPrefix(strings.ToLower(def.URL), "https://") {
				return nil, fmt.Errorf("mcp server %q: TLS hardening (tls) requires an https url, got %q", def.Name, def.URL)
			}
			tlsCfg, err := def.TLS.build()
			if err != nil {
				return nil, fmt.Errorf("mcp server %q: tls: %w", def.Name, err)
			}
			if tlsCfg != nil {
				t.client = tlsHTTPClient(tlsCfg)
			}
		}
		transport = t
	default:
		return nil, fmt.Errorf("mcp server %q: def has neither Command nor URL", def.Name)
	}

	s := &Server{
		name:      def.Name,
		transport: transport,
		def:       def,
	}
	if err := s.initialize(ctx); err != nil {
		_ = transport.Close()
		return nil, &serverBuildError{initialize: true, err: err}
	}
	return s, nil
}

// serverBuildError is a buildServer failure in its transport or initialize
// step, carrying the bare cause so the caller can word it.
type serverBuildError struct {
	initialize bool // false = the transport step
	err        error
}

func (e *serverBuildError) Error() string { return e.err.Error() }
func (e *serverBuildError) Unwrap() error { return e.err }

// wrapBuildError prefixes a buildServer step failure with the caller's wording
// for that step (transportPrefix / initPrefix); any other error (the TLS
// refusal, a def with neither shape) is already fully worded and passes through.
func wrapBuildError(err error, transportPrefix, initPrefix string) error {
	var be *serverBuildError
	if !errors.As(err, &be) {
		return err
	}
	if be.initialize {
		return fmt.Errorf("%s: %w", initPrefix, be.err)
	}
	return fmt.Errorf("%s: %w", transportPrefix, be.err)
}

// drainAndClose retires an old server: it acquires Server.mu — which, because
// callTool holds that mutex for the whole call, waits for any in-flight tool
// call to finish (a graceful drain) — then marks the server retired and closes
// its transport. The wait is bounded: every transport Call respects its context
// (StdioTransport selects on ctx.Done for both the stdin write and the response
// read; HTTPTransport uses a bounded client), so an in-flight call cannot block
// the lock forever.
//
// Doing this under Server.mu is what makes retirement race-free: a call that
// captured this *Server before the registry swap either (a) hasn't taken the
// mutex yet, so it observes retired==true here and refuses, or (b) holds the
// mutex mid-call, so we wait — and if it restarts the transport meanwhile, we
// close whatever transport it leaves behind, so no stdio subprocess leaks.
func drainAndClose(s *Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
	_ = s.transport.Close()
}

// Reload diffs newServers against the live registry and applies the minimum set
// of mutations without tearing down unchanged servers (#218): new names are
// started, absent names are drained + closed, and changed entries are restarted
// (build-new, swap, then drain-close the old one). It is safe to call
// concurrently with CallToolOn:
//   - New servers are built + initialized OUTSIDE any lock (initialize blocks on
//     a subprocess spawn / HTTP handshake).
//   - The registry map is swapped under c.mu (write lock) — a fast, I/O-free
//     step, so tool calls (which take the read lock) are barely delayed.
//   - Old servers are drained + closed under their own Server.mu AFTER the swap,
//     so a call that captured the old pointer completes cleanly.
//
// The synthetic servers — inline http_tools (HTTPToolServerName) and a2a peers
// (A2AToolServerName, #1368) — are never touched: they have no manifest
// mcp_servers entry to diff against, so without the exemption a reload would
// read them as "removed" and close them.
// On a build failure Reload rolls back any servers it already started and
// returns the error with a partial summary; the live registry is left unchanged
// in that case (the swap is all-or-nothing — it happens only after every new
// server initializes).
//
// Reload is serialized with itself (reloadMu): concurrent reloads run one at a
// time, so two can't both build a server for the same name and leak the loser's
// transport.
func (c *Client) Reload(ctx context.Context, newServers []ServerDef) (*ReloadSummary, error) {
	c.reloadMu.Lock()
	defer c.reloadMu.Unlock()

	want := make(map[string]ServerDef, len(newServers))
	for _, d := range newServers {
		if isSyntheticServer(d.Name) {
			continue // never manage the synthetic tool servers
		}
		want[d.Name] = d
	}

	// Snapshot the current registry (names + defs) without holding the lock
	// across the blocking build below.
	c.mu.RLock()
	currentDefs := make(map[string]ServerDef, len(c.servers))
	for name, s := range c.servers {
		if isSyntheticServer(name) {
			continue
		}
		currentDefs[name] = s.def
	}
	c.mu.RUnlock()

	summary := &ReloadSummary{}
	var toAdd, toRestart []ServerDef
	for name, def := range want {
		cur, ok := currentDefs[name]
		switch {
		case !ok:
			toAdd = append(toAdd, def)
		case serverDefEqual(cur, def):
			summary.Unchanged = append(summary.Unchanged, name)
		default:
			toRestart = append(toRestart, def)
		}
	}
	var toRemove []string
	for name := range currentDefs {
		if _, ok := want[name]; !ok {
			toRemove = append(toRemove, name)
		}
	}

	// Build every new / restarted server outside the lock. On any failure, roll
	// back what we already started so a partial reload never leaks subprocesses
	// and never half-applies.
	built := make(map[string]*Server, len(toAdd)+len(toRestart))
	rollback := func() {
		for _, s := range built {
			_ = s.transport.Close()
		}
	}
	for _, def := range append(append([]ServerDef{}, toAdd...), toRestart...) {
		s, err := buildServer(ctx, def, nil)
		if err != nil {
			rollback()
			err = wrapBuildError(err, "stdio transport", fmt.Sprintf("initialize server %q", def.Name))
			return summary, fmt.Errorf("mcp reload: %w", err)
		}
		built[def.Name] = s
	}

	// Swap the registry under the write lock (fast, no I/O), collecting the old
	// servers that must be drained + closed afterward.
	var retire []*Server
	c.mu.Lock()
	for _, def := range toAdd {
		c.servers[def.Name] = built[def.Name]
		summary.Added = append(summary.Added, def.Name)
	}
	for _, def := range toRestart {
		if old, ok := c.servers[def.Name]; ok {
			retire = append(retire, old)
		}
		c.servers[def.Name] = built[def.Name]
		summary.Restarted = append(summary.Restarted, def.Name)
	}
	for _, name := range toRemove {
		if old, ok := c.servers[name]; ok {
			retire = append(retire, old)
		}
		delete(c.servers, name)
		summary.Removed = append(summary.Removed, name)
	}
	c.mu.Unlock()

	// Drain + close retired servers outside c.mu, concurrently so one server
	// with a long in-flight call doesn't delay the others.
	var wg sync.WaitGroup
	for _, s := range retire {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			drainAndClose(s)
		}(s)
	}
	wg.Wait()

	sort.Strings(summary.Added)
	sort.Strings(summary.Removed)
	sort.Strings(summary.Restarted)
	sort.Strings(summary.Unchanged)
	return summary, nil
}

// isSyntheticServer reports whether name is one of the synthetic tool servers
// that live outside the manifest's mcp_servers — registered by AddHTTPTools /
// AddA2APeers, boot-pinned, and never part of a reload diff.
func isSyntheticServer(name string) bool {
	return name == HTTPToolServerName || name == A2AToolServerName
}
