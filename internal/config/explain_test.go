package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unsetForTest(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		t.Setenv(n, "")
		_ = os.Unsetenv(n)
	}
}

func TestExplainSourcesAndPrecedence(t *testing.T) {
	unsetForTest(t, "FLEET_MAX_COST_USD", "CHAT_MAX_COST_USD", "CUTLASS_MAX_COST_USD",
		"FLEET_TEMPERATURE", "CHAT_TEMPERATURE", "CUTLASS_TEMPERATURE",
		"OPENROUTER_API_KEY", "FLEET_MAX_ITERATIONS", "CHAT_MAX_ITERATIONS", "CUTLASS_MAX_ITERATIONS")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, strings.Join([]string{
		"# a comment",
		"CHAT_MAX_COST_USD=12",
		"OPENROUTER_API_KEY=placeholder",
		"FLEET_MAX_ITERATIONS=50",
		"CUTLASS_MAX_ITERATIONS=70",
	}, "\n")+"\n")
	t.Setenv("FLEET_MAX_ITERATIONS", "40")

	got, err := Explain(path, []string{"FLEET_MAX_COST_USD", "FLEET_TEMPERATURE", "OPENROUTER_API_KEY", "CHAT_MAX_ITERATIONS"}, false)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Explanation{}
	for _, x := range got {
		by[x.Key] = x
	}

	cost := by["FLEET_MAX_COST_USD"]
	if cost.Value != "12" || cost.Source != SourceEnvFile || cost.Spelling != "CHAT_MAX_COST_USD" || !cost.Deprecated {
		t.Errorf("FLEET_MAX_COST_USD = %+v, want 12 from the env file via the deprecated CHAT_ alias", cost)
	}
	temp := by["FLEET_TEMPERATURE"]
	if temp.Set || temp.Source != SourceDefault || temp.Value != "0.3" {
		t.Errorf("FLEET_TEMPERATURE = %+v, want the default 0.3", temp)
	}
	key := by["OPENROUTER_API_KEY"]
	if key.Value != Redacted || !key.Secret || key.Source != SourceEnvFile {
		t.Errorf("OPENROUTER_API_KEY = %+v, want redacted", key)
	}
	// An alias argument explains its canonical setting; the process env wins
	// over the file, and a file-only alias the env file does not admit is
	// reported as ignored.
	it := by["FLEET_MAX_ITERATIONS"]
	if it.Value != "40" || it.Source != SourceProcessEnv || it.Deprecated {
		t.Errorf("FLEET_MAX_ITERATIONS = %+v, want 40 from the process env", it)
	}
	if !strings.Contains(strings.Join(it.Notes, "\n"), "CUTLASS_MAX_ITERATIONS is set in the env file, which does not admit it") {
		t.Errorf("want an ignored-alias note, got %v", it.Notes)
	}
}

func TestExplainListsSetKeysByDefaultAndEverythingWithAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_GUARDRAIL_PROFILE=strict\n")
	unsetForTest(t, "FLEET_GUARDRAIL_PROFILE", "CHAT_GUARDRAIL_PROFILE", "CUTLASS_GUARDRAIL_PROFILE")
	set, err := Explain(path, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	all, err := Explain(path, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range set {
		if !x.Set {
			t.Errorf("%s listed without --all but not set", x.Key)
		}
		if x.Key == "FLEET_GUARDRAIL_PROFILE" {
			found = true
		}
	}
	if !found {
		t.Error("a key set only in the env file is missing from the default listing")
	}
	if len(all) <= len(set) {
		t.Errorf("--all listed %d keys, the default %d", len(all), len(set))
	}
	if _, err := Explain(path, []string{"FLEET_NOT_A_SETTING"}, false); err == nil {
		t.Error("an unknown key must be an error")
	}
}

func TestExplainNeverReturnsASecretValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_SMTP_PASSWORD=placeholder-password\nAPP_SESSION_SECRET=placeholder-session\n")
	unsetForTest(t, "FLEET_SMTP_PASSWORD", "APP_SESSION_SECRET")
	all, err := Explain(path, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		if strings.Contains(x.Value, "placeholder-") {
			t.Errorf("%s leaked its value", x.Key)
		}
	}
}

// ── migrate-env (LEGACY-ENV-ALIAS: remove after migrate-env) ──

func TestPlanEnvMigrationRewritesKeysOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	orig := strings.Join([]string{
		"# header comment",
		"",
		"export CHAT_MAX_COST_USD=12   # inline note",
		"CUTLASS_TASK_MODEL='openai/gpt-x'",
		"FLEET_TEMPERATURE=0.2",
		"CHAT_TEMPERATURE=0.2",
		"CUTLASS_MAX_ITERATIONS=70",
		"CHAT_RATE_PER_MIN=10",
		"PERSONA=helper",
	}, "\n") + "\n"
	writeEnv(t, path, orig)

	m, err := PlanEnvMigration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %v", m.Conflicts)
	}
	want := strings.Join([]string{
		"# header comment",
		"",
		"export FLEET_MAX_COST_USD=12   # inline note",
		"FLEET_TASK_MODEL='openai/gpt-x'",
		"FLEET_TEMPERATURE=0.2",
		"CUTLASS_MAX_ITERATIONS=70",
		"CHAT_RATE_PER_MIN=10",
		"PERSONA=helper",
	}, "\n") + "\n"
	now := time.Date(2026, 11, 7, 12, 30, 0, 0, time.UTC)
	backup, err := m.Write(now)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, path); got != want {
		t.Errorf("rewritten file:\n%s\nwant:\n%s", got, want)
	}
	if backup != path+".bak-20261107T123000Z" || readFileString(t, backup) != orig {
		t.Errorf("backup %s does not hold the original", backup)
	}
	for _, p := range []string{path, backup} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, fi.Mode().Perm())
		}
	}
	actions := map[string]MigrationAction{}
	for _, c := range m.Changes {
		actions[c.From] = c.Action
	}
	if actions["CHAT_MAX_COST_USD"] != ActionRename || actions["CHAT_TEMPERATURE"] != ActionDrop || actions["CUTLASS_MAX_ITERATIONS"] != ActionLeave {
		t.Errorf("actions = %v", actions)
	}
	if _, ok := actions["CHAT_RATE_PER_MIN"]; ok {
		t.Error("CHAT_RATE_PER_MIN is canonical, not an alias")
	}
	// Idempotent: a second plan has nothing to do.
	m2, err := PlanEnvMigration(path)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Pending() {
		t.Errorf("second run still pending: %+v", m2.Changes)
	}
}

func TestPlanEnvMigrationRefusesConflictingSpellings(t *testing.T) {
	for name, body := range map[string]string{
		"canonical vs alias": "FLEET_MAX_COST_USD=10\nCHAT_MAX_COST_USD=12\n",
		"two aliases":        "CHAT_MAX_COST_USD=10\nCUTLASS_MAX_COST_USD=12\n",
		"alias twice":        "CHAT_MAX_COST_USD=10\nCHAT_MAX_COST_USD=12\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fleet.env")
			writeEnv(t, path, body)
			m, err := PlanEnvMigration(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Conflicts) == 0 {
				t.Fatal("want a conflict")
			}
			if _, err := m.Write(time.Now()); err == nil {
				t.Fatal("Write must refuse")
			}
			if readFileString(t, path) != body {
				t.Error("the file changed despite the refusal")
			}
			for _, c := range m.Conflicts {
				if strings.Contains(c, "10") || strings.Contains(c, "12") {
					t.Errorf("conflict message leaks a value: %s", c)
				}
			}
		})
	}
}

func TestDeprecatedAliasFindings(t *testing.T) {
	unsetForTest(t, "CHAT_SERVER_TOKEN", "CUTLASS_TEMPERATURE", "CHAT_TEMPERATURE")
	t.Setenv("CUTLASS_TEMPERATURE", "0.5")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "CHAT_SERVER_TOKEN=placeholder-token\nCUTLASS_MAX_ITERATIONS=3\n")
	got, err := DeprecatedAliasFindings(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, len(got))
	for _, f := range got {
		lines = append(lines, f.String())
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"CHAT_SERVER_TOKEN is a deprecated alias (set in the env file); use FLEET_SERVER_TOKEN",
		"CUTLASS_TEMPERATURE is a deprecated alias (set in the process env); use FLEET_TEMPERATURE",
		"CUTLASS_MAX_ITERATIONS is a deprecated alias (set in the env file); use FLEET_MAX_ITERATIONS — the env file does not admit",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "placeholder-token") {
		t.Error("a finding leaked a value")
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
