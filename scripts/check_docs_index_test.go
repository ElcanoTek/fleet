// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocsIndexIsComplete — docs/README.md promises "every page under docs/,
// A–Z" (it replaced the index that used to live inside AGENTS.md, and it is
// what CONTRIBUTING.md and AGENTS.md now point contributors at). A promise
// like that rots the first time someone adds a page and forgets the row, and
// the reader who then cannot find DEPLOYMENT.md has no way to know the list
// is short. So: every top-level docs/*.md must be linked from docs/README.md.
// ADRs are excluded — docs/adr/ keeps its own index (adr/README.md), which
// the docs index links as a whole.
func TestDocsIndexIsComplete(t *testing.T) {
	root := repoRoot(t)
	index := readFile(t, root, filepath.Join("docs", "README.md"))

	linked := map[string]bool{}
	for _, m := range regexp.MustCompile(`\]\(([^)#\s]+)`).FindAllStringSubmatch(index, -1) {
		linked[strings.TrimPrefix(m[1], "./")] = true
	}

	entries, err := os.ReadDir(filepath.Join(root, "docs"))
	if err != nil {
		t.Fatalf("read docs/: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "README.md" {
			continue
		}
		if !linked[name] {
			t.Errorf("docs/%s is not linked from docs/README.md — add a row under \"All pages\" (A–Z, with the page's title)", name)
		}
	}
}
