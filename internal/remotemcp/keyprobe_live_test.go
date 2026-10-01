// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
)

// TestAPIKeyProbeRefusesBogusKeyLive measures F14 against the real vendors:
// for every built-in api_key entry without a placeholder it runs the
// add-time probe with an obviously invalid key and records where the key was
// refused — at the handshake, at the verification call, or not at all. It
// is the number the fix exists to move, so it is kept as a live test rather
// than a one-off: gated on FLEET_CATALOG_LIVE=1, never in the PR gate. It
// fails only when a vendor ACCEPTS the bogus key as verified — a false
// positive of the refusal heuristics, which must be looked at — and reports
// the rest as a table in the log.
func TestAPIKeyProbeRefusesBogusKeyLive(t *testing.T) {
	if os.Getenv("FLEET_CATALOG_LIVE") != "1" {
		t.Skip("set FLEET_CATALOG_LIVE=1 to run the live bogus-key probe (hits vendor endpoints)")
	}
	bundle, err := clientconfig.Load(filepath.Join("..", "..", "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	cfg := Config{}
	cfg.withDefaults()
	svc := &Service{cfg: cfg, httpClient: mcpoauth.SafeHTTPClient(cfg.HTTPTimeout)}

	type outcome struct{ name, verdict, detail string }
	var (
		results  []outcome
		falsePos []string
	)
	for _, e := range bundle.RemoteMCPCatalog {
		if e.Auth != "api_key" || strings.Contains(e.URL, "{") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), cfg.HTTPTimeout)
		report, perr := svc.probeServer(ctx, e.URL, e.APIKeyHeader, e.APIKeyQuery, e.APIKeyPrefix, "fleet-live-probe-invalid-key")
		cancel()
		var kr *keyRejectedError
		switch {
		case perr == nil && report.KeyVerified:
			results = append(results, outcome{e.Name, "ACCEPTED-VERIFIED", "bogus key passed the verification call via " + report.CheckedWith})
			falsePos = append(falsePos, e.Name)
		case perr == nil:
			how := "not refused at " + report.CheckedWith
			if report.CheckedWith == "" {
				how = "no safe read-only tool to try"
			}
			results = append(results, outcome{e.Name, "not refused", how + "; the key is checked on first use"})
		case errors.As(perr, &kr):
			results = append(results, outcome{e.Name, "refused at tool call", kr.tool + ": " + kr.reason})
		default:
			results = append(results, outcome{e.Name, "refused at handshake", truncateReason(perr.Error())})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].verdict+results[i].name < results[j].verdict+results[j].name })
	counts := map[string]int{}
	for _, r := range results {
		counts[r.verdict]++
		t.Logf("%-22s %-22s %s", r.name, r.verdict, r.detail)
	}
	t.Logf("api_key entries probed: %d — %v", len(results), counts)
	if len(falsePos) > 0 {
		t.Errorf("a bogus key was reported VERIFIED at: %v — the refusal heuristics missed the vendor's wording", falsePos)
	}
}
