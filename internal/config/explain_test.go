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

	got, err := explainRun(path, []string{"FLEET_MAX_COST_USD", "FLEET_TEMPERATURE", "OPENROUTER_API_KEY", "CHAT_MAX_ITERATIONS"}, false, nil)
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
	if !strings.Contains(strings.Join(it.Notes, "\n"), "CUTLASS_MAX_ITERATIONS is set in the env file, which fleet's own loader does not admit") {
		t.Errorf("want an ignored-alias note, got %v", it.Notes)
	}
}

func TestExplainListsSetKeysByDefaultAndEverythingWithAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_GUARDRAIL_PROFILE=strict\n")
	unsetForTest(t, "FLEET_GUARDRAIL_PROFILE", "CHAT_GUARDRAIL_PROFILE", "CUTLASS_GUARDRAIL_PROFILE")
	set, err := explainRun(path, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := explainRun(path, nil, true, nil)
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
	if _, err := explainRun(path, []string{"FLEET_NOT_A_SETTING"}, false, nil); err == nil {
		t.Error("an unknown key must be an error")
	}
}

func TestExplainNeverReturnsASecretValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_SMTP_PASSWORD=placeholder-password\nAPP_SESSION_SECRET=placeholder-session\n")
	unsetForTest(t, "FLEET_SMTP_PASSWORD", "APP_SESSION_SECRET")
	all, err := explainRun(path, nil, true, nil)
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
		"CHAT_TLS_MODE=auto",
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
	// CUTLASS_MAX_ITERATIONS is not admitted by fleet's own loader, but the
	// standard install's systemd unit exports every line, so it IS effective
	// there and is renamed. CHAT_TLS_MODE is reader-specific (only the CLI
	// health probe reads it; the server ignores it), so it is left alone:
	// renaming it would turn TLS on for the server.
	want := strings.Join([]string{
		"# header comment",
		"",
		"export FLEET_MAX_COST_USD=12   # inline note",
		"FLEET_TASK_MODEL='openai/gpt-x'",
		"FLEET_TEMPERATURE=0.2",
		"FLEET_MAX_ITERATIONS=70",
		"CHAT_TLS_MODE=auto",
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
	changes := map[string]MigrationChange{}
	for _, c := range m.Changes {
		changes[c.From] = c
	}
	if changes["CHAT_MAX_COST_USD"].Action != ActionRename || changes["CHAT_MAX_COST_USD"].LoaderIgnored ||
		changes["CHAT_TEMPERATURE"].Action != ActionDrop {
		t.Errorf("changes = %+v", changes)
	}
	if c := changes["CUTLASS_MAX_ITERATIONS"]; c.Action != ActionRename || !c.LoaderIgnored {
		t.Errorf("CUTLASS_MAX_ITERATIONS = %+v, want a rename flagged loader-ignored (effective via systemd)", c)
	}
	if c := changes["CHAT_TLS_MODE"]; c.Action != ActionLeave || c.ReadBy == "" {
		t.Errorf("CHAT_TLS_MODE = %+v, want left in place as reader-specific", c)
	}
	if _, ok := changes["CHAT_RATE_PER_MIN"]; ok {
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
		"canonical vs alias":         "FLEET_MAX_COST_USD=10\nCHAT_MAX_COST_USD=12\n",
		"two aliases":                "CHAT_MAX_COST_USD=10\nCUTLASS_MAX_COST_USD=12\n",
		"alias twice":                "CHAT_MAX_COST_USD=10\nCHAT_MAX_COST_USD=12\n",
		"loader-ignored alias":       "FLEET_MAX_ITERATIONS=10\nCUTLASS_MAX_ITERATIONS=12\n",
		"loader-ignored alias twice": "CUTLASS_MAX_ITERATIONS=10\nCUTLASS_MAX_ITERATIONS=12\n",
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

// A reader-specific alias is left in place, so it never conflicts with the
// canonical key.
func TestPlanEnvMigrationReaderSpecificAliasNeverConflicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "FLEET_TLS_MODE=off\nCHAT_TLS_MODE=auto\n")
	m, err := PlanEnvMigration(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Conflicts) != 0 || m.Pending() {
		t.Fatalf("conflicts=%v pending=%v, want neither", m.Conflicts, m.Pending())
	}
}

func TestDeprecatedAliasFindings(t *testing.T) {
	unsetForTest(t, "CHAT_SERVER_TOKEN", "CUTLASS_TEMPERATURE", "CHAT_TEMPERATURE", "CHAT_TLS_MODE")
	t.Setenv("CUTLASS_TEMPERATURE", "0.5")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "CHAT_SERVER_TOKEN=placeholder-token\nCUTLASS_MAX_ITERATIONS=3\nCHAT_TLS_MODE=auto\n")
	findings := func(exported bool) string {
		t.Helper()
		var unit *UnitEnv
		if exported {
			unit = unitExporting(t, path)
		}
		got, err := DeprecatedAliasFindings(path, unit)
		if err != nil {
			t.Fatal(err)
		}
		lines := make([]string, 0, len(got))
		for _, f := range got {
			lines = append(lines, f.String())
		}
		return strings.Join(lines, "\n")
	}
	notExported, exported := findings(false), findings(true)
	for _, want := range []string{
		"CHAT_SERVER_TOKEN is a deprecated alias (set in the env file); use FLEET_SERVER_TOKEN",
		"CUTLASS_TEMPERATURE is a deprecated alias (set in the process env); use FLEET_TEMPERATURE",
		"CHAT_TLS_MODE is a deprecated alias (set in the env file); use FLEET_TLS_MODE — only the operator CLI's local health probe",
	} {
		for _, joined := range []string{notExported, exported} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing finding %q in:\n%s", want, joined)
			}
		}
	}
	if !strings.Contains(notExported, "CUTLASS_MAX_ITERATIONS is a deprecated alias (set in the env file); use FLEET_MAX_ITERATIONS — fleet's own loader does not admit this spelling and the env file is not exported") {
		t.Errorf("not-exported wording:\n%s", notExported)
	}
	if !strings.Contains(exported, "CUTLASS_MAX_ITERATIONS is a deprecated alias (set in the env file); use FLEET_MAX_ITERATIONS — it reaches the server only through the systemd unit's EnvironmentFile=") {
		t.Errorf("exported wording:\n%s", exported)
	}
	if strings.Contains(notExported+exported, "placeholder-token") {
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
