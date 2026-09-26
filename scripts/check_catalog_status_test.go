// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// TestCatalogStatusInventoryMatchesCatalog — docs/MCP-CATALOG-STATUS.md ends
// with the #986 inventory: one row per built-in remote MCP entry, and a
// "Counts —" paragraph summarising it. Both are hand-maintained derivations of
// internal/clientconfig/builtin_remote_catalog.yaml, and the Phase 4 review
// caught the summary drifting from the table by one. So: every catalog entry
// has exactly one row, each row's auth / provenance / category / featured
// match the entry, and every number in the summary equals what the table
// says — the same "check the claim mechanically" rule the guide-pinning tests
// in this directory follow.
func TestCatalogStatusInventoryMatchesCatalog(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "internal", "clientconfig", "builtin_remote_catalog.yaml"))
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var cf struct {
		Servers []struct {
			Name       string `yaml:"name"`
			URL        string `yaml:"url"`
			Auth       string `yaml:"auth"`
			Provenance string `yaml:"provenance"`
			Category   string `yaml:"category"`
			Featured   bool   `yaml:"featured"`
		} `yaml:"servers"`
	}
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("parse catalog: %v", err)
	}
	status := readFile(t, root, filepath.Join("docs", "MCP-CATALOG-STATUS.md"))

	// | entry | auth | provenance | category | featured | can CI hit? | last verified | probe verdict | notes |
	rowRe := regexp.MustCompile(`(?m)^\| ([a-z0-9-]+) \| (oauth|api_key|open|tenant) \| (official|third_party|community) \| ([a-z0-9-]+) \| (★?) \| ([a-z-]+) \| ([^|]*) \|`)
	type row struct {
		auth, prov, cat, ci, last string
		feat                      bool
	}
	rows := map[string]row{}
	for _, m := range rowRe.FindAllStringSubmatch(status, -1) {
		if _, dup := rows[m[1]]; dup {
			t.Errorf("inventory lists %q twice", m[1])
		}
		rows[m[1]] = row{auth: m[2], prov: m[3], cat: m[4], feat: m[5] == "★", ci: m[6], last: strings.TrimSpace(m[7])}
	}
	if len(rows) < 60 {
		t.Fatalf("found only %d inventory rows; the table layout moved", len(rows))
	}

	seen := map[string]bool{}
	for _, e := range cf.Servers {
		seen[e.Name] = true
		r, ok := rows[e.Name]
		if !ok {
			t.Errorf("catalog entry %q has no inventory row", e.Name)
			continue
		}
		if r.auth != e.Auth || r.prov != e.Provenance || r.cat != e.Category || r.feat != e.Featured {
			t.Errorf("entry %q: inventory says auth=%s provenance=%s category=%s featured=%v; catalog says %s/%s/%s/%v", e.Name, r.auth, r.prov, r.cat, r.feat, e.Auth, e.Provenance, e.Category, e.Featured)
		}
	}
	for name := range rows {
		if !seen[name] {
			t.Errorf("inventory row %q has no catalog entry", name)
		}
	}

	// The summary paragraph must equal the table it summarises.
	count := func(pred func(row) bool) int {
		n := 0
		for _, r := range rows {
			if pred(r) {
				n++
			}
		}
		return n
	}
	want := map[string]int{
		"yes":           count(func(r row) bool { return r.ci == "yes" }),
		"key-fixture":   count(func(r row) bool { return r.ci == "key-fixture" }),
		"oauth-manual":  count(func(r row) bool { return r.ci == "oauth-manual" }),
		"tenant":        count(func(r row) bool { return r.ci == "tenant" }),
		"dead-suspect":  count(func(r row) bool { return r.ci == "dead-suspect" }),
		"oauth":         count(func(r row) bool { return r.auth == "oauth" }),
		"auth-tenant":   count(func(r row) bool { return r.auth == "tenant" }),
		"api_key":       count(func(r row) bool { return r.auth == "api_key" }),
		"open":          count(func(r row) bool { return r.auth == "open" }),
		"official":      count(func(r row) bool { return r.prov == "official" }),
		"third_party":   count(func(r row) bool { return r.prov == "third_party" }),
		"community":     count(func(r row) bool { return r.prov == "community" }),
		"Featured":      count(func(r row) bool { return r.feat }),
		"live":          count(func(r row) bool { return strings.HasSuffix(r.last, "live") }),
		"not probeable": count(func(r row) bool { return r.last == "—" }),
	}
	summaryRe := regexp.MustCompile(`(?s)Counts — can CI hit\?: (.*?)\n\n`)
	sm := summaryRe.FindStringSubmatch(status)
	if sm == nil {
		t.Fatal("no \"Counts — can CI hit?:\" paragraph above the inventory")
	}
	summary := strings.ReplaceAll(sm[1], "\n", " ")
	get := func(label string) int {
		m := regexp.MustCompile(regexp.QuoteMeta(label) + `:? (\d+)`).FindStringSubmatch(summary)
		if m == nil {
			t.Errorf("summary lacks %q", label)
			return -1
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}
	for _, c := range []struct{ label, key string }{
		{"yes", "yes"}, {"key-fixture", "key-fixture"}, {"oauth-manual", "oauth-manual"}, {"dead-suspect", "dead-suspect"},
		{"oauth", "oauth"}, {"api_key", "api_key"}, {"open", "open"},
		{"official", "official"}, {"third_party", "third_party"}, {"community", "community"},
		{"Featured", "Featured"}, {"live", "live"}, {"not probeable", "not probeable"},
	} {
		if got := get(c.label); got != -1 && got != want[c.key] {
			t.Errorf("summary says %s %d, the table has %d", c.label, got, want[c.key])
		}
	}
	// "tenant" appears twice (a can-CI class and an auth value); check both in order.
	tenants := regexp.MustCompile(`tenant (\d+)`).FindAllStringSubmatch(summary, -1)
	if len(tenants) != 2 {
		t.Errorf("summary should mention tenant twice (class and auth), found %d", len(tenants))
	} else {
		if n, _ := strconv.Atoi(tenants[0][1]); n != want["tenant"] {
			t.Errorf("summary says can-CI tenant %d, the table has %d", n, want["tenant"])
		}
		if n, _ := strconv.Atoi(tenants[1][1]); n != want["auth-tenant"] {
			t.Errorf("summary says auth tenant %d, the table has %d", n, want["auth-tenant"])
		}
	}
	// Each "probe YYYY-MM-DD N" must equal the rows carrying that date.
	for _, m := range regexp.MustCompile(`probe (\d{4}-\d{2}-\d{2}) (\d+)`).FindAllStringSubmatch(summary, -1) {
		n, _ := strconv.Atoi(m[2])
		if got := count(func(r row) bool { return r.last == m[1]+" probe" }); got != n {
			t.Errorf("summary says probe %s %d, the table has %d", m[1], n, got)
		}
	}
	// The row-count sentence must name the table's size.
	if m := regexp.MustCompile(`All (\d+) entries of`).FindStringSubmatch(status); m == nil {
		t.Error("the inventory intro no longer states the entry count")
	} else if n, _ := strconv.Atoi(m[1]); n != len(cf.Servers) {
		t.Errorf("inventory intro says %d entries, the catalog has %d", n, len(cf.Servers))
	}
}
