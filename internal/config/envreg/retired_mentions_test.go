package envreg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// retiredNamePattern matches any spelling RetiredAlias reports (ADR-0106) as
// a whole word: DATA_DIR inside FLEET_DATA_DIR does not count.
func retiredNamePattern(t *testing.T, prefixedOnly bool) *regexp.Regexp {
	t.Helper()
	var names []string
	for _, e := range All() {
		for _, n := range RetiredAliasesOf(e.Name) {
			if prefixedOnly && !retiredPrefixed(n) {
				continue
			}
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		t.Fatal("no retired spellings derived")
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for i, n := range names {
		names[i] = regexp.QuoteMeta(n)
	}
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(` + strings.Join(names, "|") + `)(?:$|[^A-Za-z0-9_])`)
}

// TestNoStringLiteralNamesARetiredAlias — ADR-0106: no help text, usage line,
// error message or other string in non-test Go code may name a removed
// CHAT_/CUTLASS_ spelling (or the bare DATA_DIR), so nothing tells an operator
// to set one. The registry itself (this package) is exempt: it is where the
// retired spellings are recognised. The bare DATA_DIR is left out here: Go
// strings pass "DATA_DIR" as the suffix of FLEET_DATA_DIR (getenvFleet), and
// the legacy-import messages name moc's own data directory.
func TestNoStringLiteralNamesARetiredAlias(t *testing.T) {
	re := retiredNamePattern(t, true)
	root := moduleRoot(t)
	var problems []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch rel {
			case ".git", "web", ".gocache", "vendor", "node_modules", filepath.Join("internal", "config", "envreg"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil //nolint:nilerr // an unparsable file is the compiler's problem, not this test's
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			if m := re.FindStringSubmatch(s); m != nil {
				problems = append(problems, rel+": string literal names "+m[1])
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("%s, a spelling ADR-0106 removed; name the FLEET_ key", p)
	}
}

// retiredDocExempt are docs that may name a removed spelling: the history.
var retiredDocExempt = map[string]string{
	"FEATURE-NOTES.md":   "historical feature notes, recorded as they shipped",
	"LEGACY-IMPORT.md":   "DATA_DIR there is the legacy moc deployment's own directory, not a fleet setting",
	"CONFIGURATION.md":   "generated from the registry, which names no retired spelling",
	"CONFIG-REGISTRY.md": "the registry's design note; its Removed aliases section documents the removal",
}

// TestDocsDoNotPointAtRetiredAliases — ADR-0106: a paragraph in docs/ (ADRs
// aside) that names a removed spelling must say it was removed or retired,
// so no page tells an operator to set one.
func TestDocsDoNotPointAtRetiredAliases(t *testing.T) {
	re := retiredNamePattern(t, false)
	docs := filepath.Join(moduleRoot(t), "docs")
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "adr" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		if _, ok := retiredDocExempt[d.Name()]; ok {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(docs, path)
		for _, para := range strings.Split(string(b), "\n\n") {
			m := re.FindStringSubmatch(para)
			if m == nil {
				continue
			}
			lower := strings.ToLower(para)
			if strings.Contains(lower, "removed") || strings.Contains(lower, "retired") || strings.Contains(para, "ADR-0106") {
				continue
			}
			t.Errorf("docs/%s names %s, a spelling ADR-0106 removed, without saying so; name the FLEET_ key", rel, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
