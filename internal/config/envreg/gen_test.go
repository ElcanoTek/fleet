package envreg

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// update regenerates the generated files instead of comparing them:
//
//	go test ./internal/config/envreg -run TestGeneratedFilesAreUpToDate -update
//
// (`make config-docs` runs exactly that).
var update = flag.Bool("update", false, "rewrite docs/CONFIGURATION.md, the Helm schema's config.env and scripts/lib/retired-env.sh from the registry")

// TestGeneratedFilesAreUpToDate is the drift check for the two files
// generated from the registry: docs/CONFIGURATION.md, and the config.env
// subtree of deploy/helm/fleet/values.schema.json.
func TestGeneratedFilesAreUpToDate(t *testing.T) {
	root := moduleRoot(t)

	docPath := filepath.Join(root, "docs", "CONFIGURATION.md")
	checkGenerated(t, docPath, Markdown())

	schemaPath := filepath.Join(root, "deploy", "helm", "fleet", "values.schema.json")
	cur, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := RewriteHelmSchema(cur)
	if err != nil {
		t.Fatal(err)
	}
	checkGenerated(t, schemaPath, want)

	checkGenerated(t, filepath.Join(root, "scripts", "lib", "retired-env.sh"), RetiredEnvShell())
}

func checkGenerated(t *testing.T, path string, want []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (run `make config-docs`)", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date with internal/config/envreg/table.go — run `make config-docs` and commit the result", path)
	}
}

// TestRewriteHelmSchemaIsIdempotentAndLocal — regenerating twice changes
// nothing, and the bytes outside config.env are untouched.
func TestRewriteHelmSchemaIsIdempotentAndLocal(t *testing.T) {
	src := []byte("{\n  \"properties\": {\n    \"a\": { \"type\": \"string\" },\n    \"config\": {\n      \"type\": \"object\",\n      \"properties\": {\n        \"existingSecret\": { \"type\": \"string\" },\n        \"env\": { \"type\": \"object\" }\n      }\n    },\n    \"z\": { \"type\": \"string\", \"description\": \"a } brace in a string\" }\n  }\n}\n")
	once, err := RewriteHelmSchema(src)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := RewriteHelmSchema(once)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatal("RewriteHelmSchema is not idempotent")
	}
	head := bytes.Index(src, []byte(`"env": `))
	tail := bytes.Index(src, []byte("\n      }\n    },\n    \"z\""))
	if head < 0 || tail < 0 {
		t.Fatal("fixture markers not found")
	}
	if !bytes.HasPrefix(once, src[:head]) || !bytes.HasSuffix(once, src[tail:]) {
		t.Fatal("RewriteHelmSchema touched bytes outside config.env")
	}
	var doc map[string]any
	if err := json.Unmarshal(once, &doc); err != nil {
		t.Fatal(err)
	}
}

// TestHelmTemplateEnvMatchesRegistryMapping — every variable the chart's
// Deployment sets itself must be a registry entry whose Helm field says how,
// and every entry with a Helm mapping must be set by the template. This is
// the explicit mapping table between chart values and settings.
func TestHelmTemplateEnvMatchesRegistryMapping(t *testing.T) {
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "deploy", "helm", "fleet", "templates", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*- name: ([A-Z][A-Z0-9_]+)\s*$`).FindAllStringSubmatch(string(b), -1) {
		set[m[1]] = true
	}
	if len(set) < 20 {
		t.Fatalf("found only %d env names in deployment.yaml; the extraction is broken", len(set))
	}
	var problems []string
	for name := range set {
		e, ok := Find(name)
		switch {
		case !ok:
			problems = append(problems, name+" is set by the chart but not registered")
		case e.Name != name:
			problems = append(problems, name+" is set by the chart under an alias spelling; use "+e.Name)
		case e.Helm == "":
			problems = append(problems, name+" is set by the chart but its registry entry has no Helm mapping")
		}
	}
	for _, e := range All() {
		if e.Helm != "" && !set[e.Name] {
			problems = append(problems, e.Name+" has Helm mapping "+e.Helm+" but the chart does not set it")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// TestMarkdownCoversEveryEntry — a cheap guard on the generator itself.
func TestMarkdownCoversEveryEntry(t *testing.T) {
	doc := string(Markdown())
	for _, e := range All() {
		if !strings.Contains(doc, "| `"+e.Name+"` |") {
			t.Errorf("%s is missing from the generated reference", e.Name)
		}
	}
}
