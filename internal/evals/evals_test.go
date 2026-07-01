package evals

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeEvalFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const validSetYAML = `set: smoke
threshold: 0.5
judge_model: openai/gpt-5.2
cases:
  - name: hello
    prompt: "Say hello"
    model: openai/gpt-5.2
    scorers:
      - contains: "hello"
      - regex: "(?i)hel+o"
  - name: judged
    prompt: "Summarize X"
    model: anthropic/claude-sonnet-4.6
    expected: "a fine summary"
    scorers:
      - llm_judge:
          rubric: "Is the summary faithful?"
          min_score: 0.6
`

func TestLoadSets_WellFormed(t *testing.T) {
	dir := t.TempDir()
	writeEvalFile(t, dir, "smoke.yaml", validSetYAML)
	sets, problems := LoadSets(dir)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if len(sets) != 1 {
		t.Fatalf("want 1 set, got %d", len(sets))
	}
	s := sets[0]
	if s.Name != "smoke" || s.File != "smoke.yaml" || len(s.Cases) != 2 {
		t.Fatalf("unexpected set: %+v", s)
	}
	if s.EffectiveThreshold() != 0.5 {
		t.Fatalf("threshold: got %v", s.EffectiveThreshold())
	}
	if got := s.Cases[1].Scorers[0].Kind(); got != "llm_judge" {
		t.Fatalf("scorer kind: got %q", got)
	}
	if got := s.Cases[1].Scorers[0].LLMJudge.EffectiveMinScore(); got != 0.6 {
		t.Fatalf("min_score: got %v", got)
	}
}

func TestLoadSets_DefaultsAndAbsentDir(t *testing.T) {
	if sets, problems := LoadSets(filepath.Join(t.TempDir(), "nope")); sets != nil || problems != nil {
		t.Fatalf("absent dir must be (nil, nil), got (%v, %v)", sets, problems)
	}
	dir := t.TempDir()
	writeEvalFile(t, dir, "noname.yaml", `cases:
  - name: a
    prompt: p
    model: m
    scorers: [{contains: x}]
`)
	sets, problems := LoadSets(dir)
	if len(problems) != 0 || len(sets) != 1 {
		t.Fatalf("got sets=%v problems=%v", sets, problems)
	}
	if sets[0].Name != "noname" {
		t.Fatalf("set name must default to file basename, got %q", sets[0].Name)
	}
	if sets[0].EffectiveThreshold() != 1.0 {
		t.Fatalf("absent threshold must default to 1.0, got %v", sets[0].EffectiveThreshold())
	}
}

func TestLoadSets_MalformedExcluded(t *testing.T) {
	dir := t.TempDir()
	// Each of these must produce a problem AND exclude the set.
	writeEvalFile(t, dir, "empty.yaml", "set: empty\ncases: []\n")
	writeEvalFile(t, dir, "nomodel.yaml", `set: nomodel
cases: [{name: a, prompt: p, scorers: [{contains: x}]}]
`)
	writeEvalFile(t, dir, "badregex.yaml", `set: badregex
cases: [{name: a, prompt: p, model: m, scorers: [{regex: "(["}]}]
`)
	writeEvalFile(t, dir, "noscorer.yaml", `set: noscorer
cases: [{name: a, prompt: p, model: m, scorers: []}]
`)
	writeEvalFile(t, dir, "twoscorers.yaml", `set: twoscorers
cases: [{name: a, prompt: p, model: m, scorers: [{contains: x, regex: y}]}]
`)
	writeEvalFile(t, dir, "unknownkey.yaml", "set: unknown\nbogus_key: true\ncases: [{name: a, prompt: p, model: m, scorers: [{contains: x}]}]\n")
	writeEvalFile(t, dir, "norubric.yaml", `set: norubric
cases: [{name: a, prompt: p, model: m, scorers: [{llm_judge: {model: m}}]}]
`)
	sets, problems := LoadSets(dir)
	if len(sets) != 0 {
		t.Fatalf("malformed sets must be excluded, got %v", sets)
	}
	if len(problems) < 7 {
		t.Fatalf("want ≥7 problems, got %d: %v", len(problems), problems)
	}
}

func TestLoadSets_DuplicateSetName(t *testing.T) {
	dir := t.TempDir()
	one := "set: dup\ncases: [{name: a, prompt: p, model: m, scorers: [{contains: x}]}]\n"
	writeEvalFile(t, dir, "a.yaml", one)
	writeEvalFile(t, dir, "b.yaml", one)
	sets, problems := LoadSets(dir)
	if len(sets) != 1 || len(problems) != 1 || !strings.Contains(problems[0], "duplicate set name") {
		t.Fatalf("got sets=%d problems=%v", len(sets), problems)
	}
}

func TestBundleFingerprint(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.yaml", "branding: {}\n")
	write("personas/a.yaml", "name: A\n")
	write("evals/smoke.yaml", validSetYAML)

	one, err := BundleFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(one, "sha256:") {
		t.Fatalf("fingerprint shape: %q", one)
	}
	two, err := BundleFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatal("fingerprint must be stable across calls")
	}
	// Quality-relevant edits change it…
	write("personas/a.yaml", "name: B\n")
	three, err := BundleFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if three == one {
		t.Fatal("persona edit must change the fingerprint")
	}
	// …irrelevant content (mcp/) does not.
	write("mcp/server.py", "print('x')\n")
	four, err := BundleFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if four != three {
		t.Fatal("mcp/ content must not affect the fingerprint")
	}
}

// TestDefaultBundleEvalsValid asserts the shipped generic bundle's evals/ dir
// is well-formed: the example set loads with zero problems — the evals
// analogue of clientconfig's TestDefaultBundleSkillsValid CI guard.
func TestDefaultBundleEvalsValid(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sets, problems := LoadSets(filepath.Join(root, "config", "default", "evals"))
	if len(problems) != 0 {
		t.Errorf("default bundle evals should be clean, got problems: %v", problems)
	}
	if FindSet(sets, "example") == nil {
		t.Fatal("default bundle should ship the example eval set")
	}
}
