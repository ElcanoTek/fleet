// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
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

// catalogStrictEnv turns a vendor outage from a warning into a failure, for a
// deliberate sweep (the workflow's strict_smoke dispatch input) — the same
// switch the link lint's --strict is.
const catalogStrictEnv = "FLEET_CATALOG_STRICT"

// vendorOutageMarker starts the skip message of a probe lost to a vendor
// outage. The workflow greps for it to annotate the run, so keep the two in
// step.
const vendorOutageMarker = "VENDOR OUTAGE"

// vendorOutage reports whether a probe failure — after the connect retry —
// says the vendor is down right now rather than that the listing is wrong:
//
//   - an HTTP 429 or any 5xx but 501, whatever the body: plain text
//     (HTTPStatusError), a JSON-RPC error (RPCError) or a JSON-RPC error not
//     attributed to the request (UnattributedResponseError). The run
//     classifier retries only 500/502/503/504/429 and reads a JSON-RPC
//     body's wording; the smoke goes by the status, and also counts the
//     nonstandard ones a CDN answers for a down origin (Cloudflare's 520–524
//     and 530). 501 Not Implemented stays rot: the endpoint answered and does
//     not speak the protocol;
//   - a JSON-RPC error whose message says the caller is rate-limited
//     (rpcMessageSaysRateLimited), on any status: some vendors answer a
//     throttled request with HTTP 200 and a JSON-RPC error, so the 429 never
//     reaches the transport (#1844: Parallel Search's free tier, shared by
//     every GitHub runner on the same egress IP);
//   - otherwise whatever mcp.IsTransientConnectError calls transient: a
//     timeout, a refused or reset connection, a temporary DNS failure, a
//     JSON-RPC error that says it is temporary.
//
// A host that no longer resolves is rot, not an outage: the classifier calls
// DNS transient for a run's sake, but for a shipped listing a missing host is
// a dead entry, and the link lint fails an unresolvable docs host for the same
// reason.
func vendorOutage(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return false
	}
	switch status := httpStatusOf(err); {
	case status == http.StatusTooManyRequests:
		return true
	case status >= 500 && status <= 599:
		return status != http.StatusNotImplemented
	}
	if rpcMessageSaysRateLimited(rpcMessageOf(err)) {
		return true
	}
	return mcp.IsTransientConnectError(err)
}

// rpcMessageOf is the message of the JSON-RPC error a probe failure carried,
// answered to the request or unattributed; "" when there was none.
func rpcMessageOf(err error) string {
	var rpcErr *mcp.RPCError
	var unattributed *mcp.UnattributedResponseError
	switch {
	case errors.As(err, &rpcErr):
		return rpcErr.Message
	case errors.As(err, &unattributed) && unattributed.Carried != nil:
		return unattributed.Carried.Message
	}
	return ""
}

// rpcMessageSaysRateLimited reports whether a JSON-RPC error message says
// the caller hit a rate limit. That is the vendor's capacity tonight, not the
// listing: an open entry that rate-limits anonymous callers still works for
// a user within its limits. It is the smoke's rule only — mcp's run-time
// classifier does not retry on it, since a quota rarely clears in seconds.
func rpcMessageSaysRateLimited(message string) bool {
	m := strings.Join(strings.Fields(strings.ToLower(message)), " ")
	for _, phrase := range []string{"rate limit", "rate-limit", "ratelimit", "too many requests"} {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// httpStatusOf is the non-2xx HTTP status a probe failure arrived under, in
// whichever error form the transport reported it; 0 when there was none.
func httpStatusOf(err error) int {
	var statusErr *mcp.HTTPStatusError
	var rpcErr *mcp.RPCError
	var unattributed *mcp.UnattributedResponseError
	switch {
	case errors.As(err, &statusErr):
		return statusErr.StatusCode
	case errors.As(err, &rpcErr):
		return rpcErr.HTTPStatus
	case errors.As(err, &unattributed):
		return unattributed.HTTPStatus
	}
	return 0
}

// failUnlessOutagef fails the subtest on err, except that a vendor outage only
// skips it with a vendorOutageMarker warning unless FLEET_CATALOG_STRICT=1.
// One third-party service having a bad night is not a fault in fleet's
// catalog, and a lane that goes red for it trains people to ignore the alarm
// (#1683: one 503 from kiwi-flights filed one). What still fails is what the
// catalog's curation contract covers: an endpoint that answers but is not a
// working MCP server, refuses an open handshake, lists no tools or a broken
// schema, or whose host is gone.
func failUnlessOutagef(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if vendorOutage(err) && os.Getenv(catalogStrictEnv) != "1" {
		t.Skipf("%s (warning, not a catalog fault; %s=1 fails it): %s", vendorOutageMarker, catalogStrictEnv, msg)
	}
	t.Fatal(msg)
}

// fixtureKeyRefusedMarker starts the skip message of an api_key fixture whose
// own (real) key the vendor refused. The workflow greps for it to annotate the
// run, so keep the two in step.
const fixtureKeyRefusedMarker = "FIXTURE KEY REFUSED"

// fixtureKeyRefused reports whether a probe with a fixture's real key failed
// because the vendor refused that key: an HTTP 401 or 403 in any of the
// transport's error forms, or a key rejected at the read-only verification
// call. Outages are sorted out first (vendorOutage), so a 5xx never lands here.
//
// The secret lives in a third-party account fleet does not control: trials
// lapse, plans drop the permission a token needs, someone revokes the token.
// None of that is the listing being wrong, and #1776 (the PagerDuty EU key
// refused two nights running, no fleet change in between) showed a red lane
// for it reads as a fleet regression.
func fixtureKeyRefused(err error) bool {
	switch httpStatusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	var kr *keyRejectedError
	return errors.As(err, &kr)
}

// failFixturef fails an api_key fixture's real-key probe on err, except that
// an outage skips it with a vendorOutageMarker warning and a refused fixture
// key skips it with a fixtureKeyRefusedMarker warning, both unless
// FLEET_CATALOG_STRICT=1. What it costs: a vendor that changes its auth
// scheme also reads as a refused key here, and only warns. The invalid-key
// control probe still fails a vendor that stops checking keys, the offline
// keyprobe tests pin how fleet attaches a key, and TestCatalogLiveAPIKeyFixtures
// fails when no armed fixture gets through at all.
func failFixturef(t *testing.T, err error, format string, args ...any) {
	t.Helper()
	if fixtureKeyRefused(err) && !vendorOutage(err) && os.Getenv(catalogStrictEnv) != "1" {
		t.Skipf("%s (warning: rotate the fixture's repository secret, or delete it to skip the fixture; %s=1 fails it): %s", fixtureKeyRefusedMarker, catalogStrictEnv, fmt.Sprintf(format, args...))
	}
	failUnlessOutagef(t, err, format, args...)
}

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

// probeForTest runs one add-time probe under the service's own timeout and
// returns its report; a refused key (at the handshake or at the
// read-only verification call) comes back as the error.
//
// The probe gets the connect retry a run gets (mcp.RetryTransientConnect
// under a fresh WithConnectRetry allowance): a vendor 503 or 429 that clears
// within seconds would not have kept the server out of a user's run, so it
// must not file the nightly alarm either (#1683). What still fails is what a
// run would also fail on — an outage outlasting the retries, a refusal, a
// dead host — and the last attempt's error is the one reported.
func (s *Service) probeForTest(url, header, query, prefix, credential string) (ProbeReport, error) {
	ctx, cancel := context.WithTimeout(mcp.WithConnectRetry(context.Background()), s.cfg.HTTPTimeout+mcp.MaxConnectRetryBudget)
	defer cancel()
	var report ProbeReport
	err := mcp.RetryTransientConnect(ctx, url, func(attemptCtx context.Context) error {
		var perr error
		report, perr = s.probeServer(attemptCtx, url, header, query, prefix, credential)
		return perr
	})
	return report, err
}

// catalogConfirmDelayEnv overrides how long a failed probe waits before its
// confirmation probe (a Go duration; "0" turns confirmation off, for a quick
// local run).
const catalogConfirmDelayEnv = "FLEET_CATALOG_CONFIRM_DELAY"

// defaultCatalogConfirmDelay is long enough for a vendor's blip, a deploy or
// a per-minute rate-limit window to pass, and short enough that a red night
// costs the lane a couple of minutes (the subtests wait in parallel).
const defaultCatalogConfirmDelay = 2 * time.Minute

// catalogConfirmDelay is the wait before a confirmation probe.
func catalogConfirmDelay(t *testing.T) time.Duration {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(catalogConfirmDelayEnv))
	if v == "" {
		return defaultCatalogConfirmDelay
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", catalogConfirmDelayEnv, v, err)
	}
	return d
}

// probeConfirmed is probeForTest, except that a failure is only reported if
// it is still there after a pause: the probe runs once more after
// catalogConfirmDelay, and that second result is the one that counts. A
// failure is anything the caller would fail the entry on (probeTrouble):
// an error, an empty tool list, or an invalid tool schema. The schemas of
// the counted result are then held to checkCatalogToolSchemas.
//
// The connect retry inside probeForTest covers seconds; this covers minutes.
// Rot — a host gone, an endpoint moved, an open entry that now wants a key —
// is still there two minutes later, while most of what turned the nightly lane
// red for no catalog reason was not (#1683's 503, #1844's rate limit). It is
// the in-run form of the consecutive-nights rule the lane has no state for,
// and it catches the next kind of vendor weather without waiting for someone
// to teach vendorOutage its wording.
func (s *Service) probeConfirmed(t *testing.T, url, header, query, prefix, credential string) (int, error) {
	t.Helper()
	report, err := s.probeForTest(url, header, query, prefix, credential)
	if why := probeTrouble(report, err); why != "" && waitToConfirm(t, url, why) {
		report, err = s.probeForTest(url, header, query, prefix, credential)
		if probeTrouble(report, err) == "" {
			t.Logf("%s: the confirmation probe succeeded; the first failure was transient", url)
		}
	}
	if err == nil {
		checkCatalogToolSchemas(t, url, report.SchemaIssues)
	}
	return report.ToolCount, err
}

// probeTrouble says why a probe result would fail its entry, or "" when it
// would not: an error, no tools, or a tool schema Fleet would withhold.
func probeTrouble(report ProbeReport, err error) string {
	if err != nil {
		return fmt.Sprintf("probe failed (%v)", err)
	}
	if report.ToolCount == 0 {
		return "the server listed no tools"
	}
	for _, issue := range report.SchemaIssues {
		if issue.Status == agentcore.ToolSchemaInvalid {
			return fmt.Sprintf("tool %s has an invalid input schema", issue.Tool)
		}
	}
	return ""
}

// waitToConfirm waits catalogConfirmDelay before a confirmation probe and
// reports whether to run it: false when confirmation is off or the test is
// being torn down.
func waitToConfirm(t *testing.T, url, why string) bool {
	t.Helper()
	delay := catalogConfirmDelay(t)
	if delay <= 0 {
		return false
	}
	t.Logf("%s: %s; probing once more in %s before calling it", url, why, delay)
	select {
	case <-time.After(delay):
		return true
	case <-t.Context().Done():
		return false
	}
}

// invalidKeyRefused reports whether the invalid-key control probe came back
// the way a vendor that checks keys answers it: refused, as a credential
// problem, and not lost to an outage.
func invalidKeyRefused(err error) bool {
	if err == nil || vendorOutage(err) {
		return false
	}
	var kr *keyRejectedError
	return handshakeRefused(err) || errors.As(err, &kr)
}

// checkCatalogToolSchemas holds a listed vendor's tools to the schema bar the
// model boundary applies. An invalid schema fails the entry: Fleet withholds
// that tool from the model, so the listing overstates what users get and the
// vendor needs telling. A rewritten (older-draft) schema only logs: Fleet
// translates it and the tool works.
func checkCatalogToolSchemas(t *testing.T, url string, issues []agentcore.ToolSchemaIssue) {
	t.Helper()
	for _, issue := range issues {
		if issue.Status == agentcore.ToolSchemaInvalid {
			t.Errorf("%s: tool %s has an input schema that is invalid JSON Schema draft 2020-12, so Fleet withholds it from the model: %s", url, issue.Tool, issue.Detail)
		} else {
			t.Logf("%s: tool %s uses older JSON Schema drafts (Fleet translates them): %s", url, issue.Tool, issue.Detail)
		}
	}
}

// TestCatalogLiveOpenEntries: every built-in `open` entry whose URL carries no
// {placeholder} must complete an unauthenticated handshake and list a tool.
// One subtest per entry, in parallel (one request each, to distinct hosts),
// so a single dead vendor names itself instead of hiding the rest of the
// shelf or holding it up.
//
// Outages only warn (failUnlessOutagef), so a common-mode failure — the runner
// lost its network, DNS is down, a client regression times every probe out —
// would skip every subtest and pass. The parent therefore fails unless at
// least one entry actually completed a handshake: one vendor down is weather,
// every vendor down is the smoke not checking anything.
func TestCatalogLiveOpenEntries(t *testing.T) {
	svc, entries := catalogLiveService(t)
	ran := 0
	var succeeded atomic.Int32
	// Cleanup runs after the parallel subtests have all finished.
	t.Cleanup(func() {
		if ran > 0 && succeeded.Load() == 0 {
			t.Errorf("none of the %d open entries completed a handshake; every probe failed or was skipped as an outage, which is the runner or fleet's client, not %d vendors at once", ran, ran)
		}
	})
	for _, e := range entries {
		if e.Auth != "open" || strings.Contains(e.URL, "{") {
			continue
		}
		ran++
		t.Run(e.Name, func(t *testing.T) {
			t.Parallel()
			tools, err := svc.probeConfirmed(t, e.URL, "", "", "", "")
			if err != nil {
				failUnlessOutagef(t, err, "%s: unauthenticated handshake failed (docs: %s): %v", e.URL, e.DocsURL, err)
			}
			if tools == 0 {
				t.Fatalf("%s: handshake succeeded but the server lists no tools", e.URL)
			}
			succeeded.Add(1)
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
// than only that the endpoint is up. RejectsBadKey means the vendor refuses
// a wrong key somewhere the add-time probe looks — at the handshake or at the
// read-only verification call. It is false for vendors that answer both to
// any bearer and check the key only on a real call (F14 in
// docs/MCP-CATALOG-STATUS.md); for them a fixture proves reachability and
// the tool list, nothing about the key itself.
//
// The other half of each fixture is the `env:` block of
// .github/workflows/mcp-catalog-smoke.yml, which forwards the repository
// secret of the same name; scripts/check_catalog_smoke_fixtures_test.go
// fails CI when the two drift.
var catalogKeyFixtures = []struct {
	Entry string
	// Variant is one of the entry's url_variants ids ("" = the entry's own
	// url). An entry with regional endpoints gets one fixture per endpoint,
	// each armed by its own secret (FLEET_CATALOG_KEY_<ENTRY>_<VARIANT>), so
	// a key for either region tests that region and the other skips.
	Variant       string
	RejectsBadKey bool
}{
	{Entry: "tavily", RejectsBadKey: true},    // Authorization: Bearer
	{Entry: "pagerduty", RejectsBadKey: true}, // "Token token=" prefix under a named Authorization header, US host
	{Entry: "exa"},         // x-api-key header
	{Entry: "browserbase"}, // browserbaseApiKey query parameter
	{Entry: "firecrawl"},   // Authorization: Bearer, versioned path
}

// fixtureURL resolves a fixture's endpoint: the entry's url, or the variant
// with the given id.
func fixtureURL(t *testing.T, e clientconfig.RemoteMCPCatalogEntry, variant string) string {
	t.Helper()
	if variant == "" {
		return e.URL
	}
	for _, v := range e.URLVariants {
		if v.ID == variant {
			return v.URL
		}
	}
	t.Fatalf("fixture %q names variant %q, which the entry does not list", e.Name, variant)
	return ""
}

// catalogKeyEnv is the environment variable that arms a fixture.
func catalogKeyEnv(entry, variant string) string {
	name := "FLEET_CATALOG_KEY_" + strings.ToUpper(strings.ReplaceAll(entry, "-", "_"))
	if variant != "" {
		name += "_" + strings.ToUpper(strings.ReplaceAll(variant, "-", "_"))
	}
	return name
}

// TestCatalogLiveAPIKeyFixtures runs fleet's add-time key validation for each
// fixture whose key is set. The real key goes first, so a vendor outage is
// reported as a failed handshake rather than as a bad secret; then, where the
// vendor checks the key at the handshake, an obviously wrong key must be
// refused with a credential-shaped error — the only way to know the header
// shape (and not just the URL) is right.
//
// A refused real key only warns (failFixturef), so, as with the open entries,
// the parent fails when two or more fixtures were armed and none got through:
// one dead secret is an account lapsing, every armed key refused at once is
// fleet no longer sending keys the way vendors expect. With a single armed
// fixture the two cannot be told apart, and it only warns.
func TestCatalogLiveAPIKeyFixtures(t *testing.T) {
	svc, entries := catalogLiveService(t)
	byName := make(map[string]clientconfig.RemoteMCPCatalogEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	var armed, succeeded atomic.Int32
	// Cleanup runs after the parallel subtests have all finished.
	t.Cleanup(func() {
		if n := armed.Load(); n >= 2 && succeeded.Load() == 0 {
			t.Errorf("none of the %d armed api_key fixtures completed a handshake; every real key was refused or hit an outage, which is fleet's key handling or the runner, not %d vendors at once", n, n)
		}
	})
	for _, f := range catalogKeyFixtures {
		t.Run(fixtureName(f.Entry, f.Variant), func(t *testing.T) {
			t.Parallel()
			e, ok := byName[f.Entry]
			if !ok {
				t.Fatalf("fixture %q is not in the built-in catalog; update catalogKeyFixtures", f.Entry)
			}
			if e.Auth != "api_key" {
				t.Fatalf("fixture %q has auth %q in the catalog, want api_key", f.Entry, e.Auth)
			}
			// Resolve the endpoint before the secret check: a fixture naming
			// a variant the catalog no longer lists must fail, not skip forever.
			url := fixtureURL(t, e, f.Variant)
			envName := catalogKeyEnv(f.Entry, f.Variant)
			key := strings.TrimSpace(os.Getenv(envName))
			if key == "" {
				t.Skipf("%s not set; skipping the %s fixture", envName, fixtureName(f.Entry, f.Variant))
			}
			armed.Add(1)
			tools, err := svc.probeConfirmed(t, url, e.APIKeyHeader, e.APIKeyQuery, e.APIKeyPrefix, key)
			if err != nil {
				failFixturef(t, err, "%s: handshake with the fixture key failed: %v", fixtureName(f.Entry, f.Variant), err)
			}
			if tools == 0 {
				t.Fatalf("%s: handshake succeeded but the server lists no tools", fixtureName(f.Entry, f.Variant))
			}
			succeeded.Add(1)
			t.Logf("%s: %d tools", fixtureName(f.Entry, f.Variant), tools)
			if !f.RejectsBadKey {
				return
			}
			const invalidKey = "fleet-catalog-smoke-invalid-key"
			badReport, badErr := svc.probeForTest(url, e.APIKeyHeader, e.APIKeyQuery, e.APIKeyPrefix, invalidKey)
			// The control is confirmed like the real-key probe: a vendor
			// mid-deploy that lets one bad key through, or answers it with
			// something that is not MCP, gets a second look before it fails.
			if !invalidKeyRefused(badErr) && waitToConfirm(t, url, fmt.Sprintf("the invalid-key probe was not refused (%d tools, err: %v)", badReport.ToolCount, badErr)) {
				badReport, badErr = svc.probeForTest(url, e.APIKeyHeader, e.APIKeyQuery, e.APIKeyPrefix, invalidKey)
			}
			badTools := badReport.ToolCount
			if badErr == nil {
				t.Fatalf("%s let an invalid key through the handshake and the read-only verification call (%d tools); the vendor changed where it checks keys, or the key is not being sent where it expects it", fixtureName(f.Entry, f.Variant), badTools)
			}
			// An outage between the two probes proves nothing about the key:
			// a 503 would otherwise pass handshakeRefused as a refusal, and a
			// dropped connection would fail the fixture.
			if vendorOutage(badErr) {
				failUnlessOutagef(t, badErr, "%s: the invalid-key probe hit an outage, so the key shape was not checked: %v", fixtureName(f.Entry, f.Variant), badErr)
			}
			if !invalidKeyRefused(badErr) {
				t.Fatalf("%s: the invalid-key probe failed, but not as the vendor refusing it: %v", fixtureName(f.Entry, f.Variant), badErr)
			}
		})
	}
}

// fixtureName is the subtest name: the entry, plus the variant id when the
// fixture targets one of the entry's regional endpoints.
func fixtureName(entry, variant string) string {
	if variant == "" {
		return entry
	}
	return entry + "/" + variant
}
