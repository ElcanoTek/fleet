package mcp

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
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
		eqStringMap(a.Env, b.Env) &&
		eqStringMap(a.Headers, b.Headers) &&
		reflect.DeepEqual(a.TLS, b.TLS)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// buildServer creates and initializes a Server from a def WITHOUT registering it
// on the client (holds no lock). The caller inserts it under c.mu. Handles the
// operator-configured stdio and HTTP (incl. #280 TLS hardening) shapes — the
// per-user SSRF-client shape (AddHTTPServerWithOptions' HTTPClient override) is
// never manifest-reloaded, so it is intentionally not covered here.
func buildServer(ctx context.Context, def ServerDef) (*Server, error) {
	var transport Transport
	switch {
	case def.Command != "":
		t, err := NewStdioTransportInDir(def.Command, def.Args, def.Env, def.Dir)
		if err != nil {
			return nil, fmt.Errorf("stdio transport: %w", err)
		}
		transport = t
	case def.URL != "":
		t := NewHTTPTransportWithHeaders(def.URL, def.Headers)
		if def.TLS != nil && !def.TLS.IsZero() {
			// Fail closed, matching AddHTTPServerWithOptions: a TLSClientConfig
			// only applies to https, so hardening a plaintext url would connect
			// unverified.
			if !strings.HasPrefix(strings.ToLower(def.URL), "https://") {
				return nil, fmt.Errorf("mcp server %q: TLS hardening requires an https url, got %q", def.Name, def.URL)
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
		name:         def.Name,
		transport:    transport,
		def:          def,
		stdioCommand: def.Command,
		stdioArgs:    def.Args,
		stdioEnv:     def.Env,
		stdioDir:     def.Dir,
	}
	if err := s.initialize(ctx); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize server %q: %w", def.Name, err)
	}
	return s, nil
}

// drainAndClose retires an old server: it waits for any in-flight tool call to
// finish (callTool holds Server.mu for the whole call, so acquiring it IS the
// drain) up to inFlightTimeout, then closes the transport. If the drain times
// out it force-closes anyway — safe because StdioTransport.Close kills+reaps the
// subprocess (the blocked call's read then errors out) and HTTPTransport.Close
// is a no-op (the in-flight request completes on its own, bounded by its ctx).
// Closing is what reaps a stdio subprocess, so it must always run.
func drainAndClose(s *Server, inFlightTimeout time.Duration) {
	deadline := time.Now().Add(inFlightTimeout)
	for {
		if s.mu.TryLock() {
			_ = s.transport.Close()
			s.mu.Unlock()
			return
		}
		if time.Now().After(deadline) {
			log.Printf("MCP reload: server %q still has an in-flight call after %s; force-closing", s.name, inFlightTimeout)
			_ = s.transport.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
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
// The synthetic inline-http-tools server (HTTPToolServerName) is never touched.
// On a build failure Reload rolls back any servers it already started and
// returns the error with a partial summary; the live registry is left unchanged
// in that case (the swap is all-or-nothing — it happens only after every new
// server initializes).
func (c *Client) Reload(ctx context.Context, newServers []ServerDef, inFlightTimeout time.Duration) (*ReloadSummary, error) {
	want := make(map[string]ServerDef, len(newServers))
	for _, d := range newServers {
		if d.Name == HTTPToolServerName {
			continue // never manage the synthetic inline-http tools server
		}
		want[d.Name] = d
	}

	// Snapshot the current registry (names + defs) without holding the lock
	// across the blocking build below.
	c.mu.RLock()
	currentDefs := make(map[string]ServerDef, len(c.servers))
	for name, s := range c.servers {
		if name == HTTPToolServerName {
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
		s, err := buildServer(ctx, def)
		if err != nil {
			rollback()
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
			drainAndClose(s, inFlightTimeout)
		}(s)
	}
	wg.Wait()

	sort.Strings(summary.Added)
	sort.Strings(summary.Removed)
	sort.Strings(summary.Restarted)
	sort.Strings(summary.Unchanged)
	return summary, nil
}
