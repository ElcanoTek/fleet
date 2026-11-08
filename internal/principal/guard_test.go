// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package principal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// identityHeaders are the headers that once carried a caller's identity
// between fleet's own layers. Identity now travels as a Principal in the
// request context (ADR-0108), so no production code may name one.
var identityHeaders = []string{
	"X-User-Email",
	"X-User-Session-Epoch",
	"X-User-Session-Source",
	"X-External-Issuer",
	"X-External-Subject",
}

// identityHeaderAllowlist names the non-test files still allowed to spell an
// identity header, each with the reason. Every entry is a listener EDGE (or
// its client) on the legacy internal-service-token channel; retiring that
// channel empties the list.
var identityHeaderAllowlist = map[string]string{
	"internal/httpapi/edge.go":         "chat listener edge: the legacy service-token channel names its user",
	"internal/sched/handlers/edge.go":  "orchestrator listener edge: the legacy service-token channel names its user",
	"internal/chattui/client.go":       "`fleet chat` / `fleet acp`: a client of the legacy service-token channel",
	"cmd/fleet-bench/chat.go":          "fleet-bench: a client of the legacy service-token channel",
	"internal/principal/guard_test.go": "this guard",
}

// TestNoIdentityHeaderReaders fails on any non-test Go file under cmd/ or
// internal/ that spells an identity header as a string constant — the only
// way to read (or write) one with net/http. A handler that needs to know who
// is calling reads principal.From(r.Context()).
func TestNoIdentityHeaderReaders(t *testing.T) {
	root := filepath.Join("..", "..")
	banned := map[string]bool{}
	for _, h := range identityHeaders {
		banned[http.CanonicalHeaderKey(h)] = true
	}
	var found []string
	seen := map[string]bool{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			hit := false
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err == nil && banned[http.CanonicalHeaderKey(strings.TrimSpace(v))] {
					hit = true
				}
				return true
			})
			if hit {
				seen[rel] = true
				if _, ok := identityHeaderAllowlist[rel]; !ok {
					found = append(found, rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(found)
	for _, f := range found {
		t.Errorf("%s names an identity header; read principal.From(r.Context()) instead (ADR-0108)", f)
	}
	// A stale allowlist entry is a hole waiting to be reused: drop it.
	for f := range identityHeaderAllowlist {
		if f != "internal/principal/guard_test.go" && !seen[f] {
			t.Errorf("allowlisted %s no longer names an identity header; remove it from identityHeaderAllowlist", f)
		}
	}
}
