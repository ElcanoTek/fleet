// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
)

// The two tests in this file are the automated half of #986 ("test official
// MCPs"): they run fleet's own add-time handshake — probeServer, the same
// initialize + tools/list over the SSRF-safe client that Connect runs after
// it has validated the entry — against the built-in catalog's live vendor
// endpoints. They hit the network, so they are gated on FLEET_CATALOG_LIVE=1
// and never run in the PR gate; the nightly workflow
// .github/workflows/mcp-catalog-smoke.yml runs them against main.
//
// What they prove is deliberately narrow. For an `open` entry: the endpoint
// answers an unauthenticated handshake and lists at least one tool — the
// listing still points at a live MCP server. For an `api_key` entry with a key
// in CI secrets: fleet's api_key shape (bearer, named header, or query
// parameter) is what the vendor expects. Neither calls a tool, and neither
// says anything about OAuth entries, whose browser consent step cannot run
// headless — those are verified by hand (docs/MCP-CATALOG-STATUS.md).

const catalogLiveEnv = "FLEET_CATALOG_LIVE"

// catalogLiveService is the smallest Service that can run probeServer: the
// same SSRF-safe client and timeout the real one is built with, no store. The
// entries are the directory AS SHIPPED, not a bundle's merged view, so an
// entry the default bundle hides (community provenance is off by default) is
// still checked.
func catalogLiveService(t *testing.T) (*Service, []clientconfig.RemoteMCPCatalogEntry) {
	t.Helper()
	if os.Getenv(catalogLiveEnv) != "1" {
		t.Skipf("set %s=1 to run the live catalog smoke (hits vendor endpoints)", catalogLiveEnv)
	}
	entries, err := clientconfig.BuiltinRemoteCatalog()
	if err != nil {
		t.Fatalf("built-in catalog: %v", err)
	}
	cfg := Config{}
	cfg.withDefaults()
	return &Service{cfg: cfg, httpClient: mcpoauth.SafeHTTPClient(cfg.HTTPTimeout)}, entries
}

// probe runs one add-time handshake under the service's own timeout.
func (s *Service) probeForTest(t *testing.T, url, header, query, credential string) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTPTimeout)
	defer cancel()
	return s.probeServer(ctx, url, header, query, credential)
}

// TestCatalogLiveOpenEntries: every built-in `open` entry whose URL carries no
// {placeholder} must complete an unauthenticated handshake and list a tool.
// One subtest per entry, in parallel (one request each, to distinct hosts),
// so a single dead vendor names itself instead of hiding the rest of the
// shelf or holding it up.
func TestCatalogLiveOpenEntries(t *testing.T) {
	svc, entries := catalogLiveService(t)
	ran := 0
	for _, e := range entries {
		if e.Auth != "open" || strings.Contains(e.URL, "{") {
			continue
		}
		ran++
		t.Run(e.Name, func(t *testing.T) {
			t.Parallel()
			tools, err := svc.probeForTest(t, e.URL, "", "", "")
			if err != nil {
				t.Fatalf("%s: unauthenticated handshake failed (docs: %s): %v", e.URL, e.DocsURL, err)
			}
			if tools == 0 {
				t.Fatalf("%s: handshake succeeded but the server lists no tools", e.URL)
			}
			t.Logf("%s: %d tools", e.Name, tools)
		})
	}
	if ran == 0 {
		t.Fatal("the built-in catalog has no probeable open entries; the smoke has nothing to check")
	}
}

// catalogKeyFixtures names the api_key entries the nightly smoke exercises when
// a key is present in the environment as FLEET_CATALOG_KEY_<ENTRY> (the entry
// name upper-cased, `-` → `_`; see catalogKeyEnv). Chosen to cover each way
// fleet can attach a key — the default `Authorization: Bearer`, a raw named
// header, and a URL query parameter — and to include vendors that reject a
// wrong key at the handshake, so a fixture also proves the header shape rather
// than only that the endpoint is up. RejectsBadKey is false for vendors that
// answer initialize/tools/list to any bearer and check the key at the first
// tool call (F14 in docs/MCP-CATALOG-STATUS.md); for them a fixture proves
// reachability and the tool list, nothing about the key itself.
//
// The other half of each fixture is the `env:` block of
// .github/workflows/mcp-catalog-smoke.yml, which forwards the repository
// secret of the same name; scripts/check_catalog_smoke_fixtures_test.go
// fails CI when the two drift.
var catalogKeyFixtures = []struct {
	Entry         string
	RejectsBadKey bool
}{
	{Entry: "tavily", RejectsBadKey: true},    // Authorization: Bearer
	{Entry: "pagerduty", RejectsBadKey: true}, // raw key under a named Authorization header
	{Entry: "exa"},         // x-api-key header
	{Entry: "browserbase"}, // browserbaseApiKey query parameter
	{Entry: "firecrawl"},   // Authorization: Bearer, versioned path
}

// catalogKeyEnv is the environment variable that arms a fixture.
func catalogKeyEnv(entry string) string {
	return "FLEET_CATALOG_KEY_" + strings.ToUpper(strings.ReplaceAll(entry, "-", "_"))
}

// isCredentialRefusal reports whether a handshake error is the vendor
// refusing the credential — an HTTP 401/403, or a JSON-RPC error reply (a
// server that answers initialize with an error object is speaking, not down)
// — as opposed to a timeout, a resolver failure or a 5xx, which say nothing
// about the key.
func isCredentialRefusal(err error) bool {
	var hs *mcp.HTTPStatusError
	if errors.As(err, &hs) {
		return hs.StatusCode == http.StatusUnauthorized || hs.StatusCode == http.StatusForbidden
	}
	var rpc *mcp.RPCError
	return errors.As(err, &rpc)
}

// TestCatalogLiveAPIKeyFixtures runs fleet's add-time key validation for each
// fixture whose key is set. The real key goes first, so a vendor outage is
// reported as a failed handshake rather than as a bad secret; then, where the
// vendor checks the key at the handshake, an obviously wrong key must be
// refused with a credential-shaped error — the only way to know the header
// shape (and not just the URL) is right.
func TestCatalogLiveAPIKeyFixtures(t *testing.T) {
	svc, entries := catalogLiveService(t)
	byName := make(map[string]clientconfig.RemoteMCPCatalogEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	for _, f := range catalogKeyFixtures {
		t.Run(f.Entry, func(t *testing.T) {
			t.Parallel()
			e, ok := byName[f.Entry]
			if !ok {
				t.Fatalf("fixture %q is not in the built-in catalog; update catalogKeyFixtures", f.Entry)
			}
			if e.Auth != "api_key" {
				t.Fatalf("fixture %q has auth %q in the catalog, want api_key", f.Entry, e.Auth)
			}
			envName := catalogKeyEnv(f.Entry)
			key := strings.TrimSpace(os.Getenv(envName))
			if key == "" {
				t.Skipf("%s not set; skipping the %s fixture", envName, f.Entry)
			}
			tools, err := svc.probeForTest(t, e.URL, e.APIKeyHeader, e.APIKeyQuery, key)
			if err != nil {
				t.Fatalf("%s: handshake with the fixture key failed: %v", f.Entry, err)
			}
			if tools == 0 {
				t.Fatalf("%s: handshake succeeded but the server lists no tools", f.Entry)
			}
			t.Logf("%s: %d tools", f.Entry, tools)
			if !f.RejectsBadKey {
				return
			}
			badTools, badErr := svc.probeForTest(t, e.URL, e.APIKeyHeader, e.APIKeyQuery, "fleet-catalog-smoke-invalid-key")
			switch {
			case badErr == nil:
				t.Fatalf("%s accepted an invalid key at the handshake (%d tools); the vendor changed, or the key is not being sent where it expects it", f.Entry, badTools)
			case !isCredentialRefusal(badErr):
				t.Fatalf("%s: the invalid-key handshake failed, but not as a credential refusal: %v", f.Entry, badErr)
			}
		})
	}
}
