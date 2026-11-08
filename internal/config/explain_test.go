package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	got, err := explainRun(path, []string{"FLEET_MAX_COST_USD", "FLEET_TEMPERATURE", "OPENROUTER_API_KEY", "FLEET_MAX_ITERATIONS"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Explanation{}
	for _, x := range got {
		by[x.Key] = x
	}

	// A removed CHAT_ alias (ADR-0106) is not read: the setting keeps its
	// default, and a note names the stale spelling.
	cost := by["FLEET_MAX_COST_USD"]
	if cost.Set || cost.Value == "12" || cost.Source != SourceDefault {
		t.Errorf("FLEET_MAX_COST_USD = %+v, want its default: CHAT_MAX_COST_USD is no longer read", cost)
	}
	if !strings.Contains(strings.Join(cost.Notes, "\n"), "CHAT_MAX_COST_USD is set (env file), but it is a removed alias (ADR-0106) and nothing reads it: rename it to FLEET_MAX_COST_USD") {
		t.Errorf("want a removed-alias note, got %v", cost.Notes)
	}
	temp := by["FLEET_TEMPERATURE"]
	if temp.Set || temp.Source != SourceDefault || temp.Value != "0.3" {
		t.Errorf("FLEET_TEMPERATURE = %+v, want the default 0.3", temp)
	}
	key := by["OPENROUTER_API_KEY"]
	if key.Value != Redacted || !key.Secret || key.Source != SourceEnvFile {
		t.Errorf("OPENROUTER_API_KEY = %+v, want redacted", key)
	}
	// The process env wins over the file, and a removed alias in the file is
	// reported, never used.
	it := by["FLEET_MAX_ITERATIONS"]
	if it.Value != "40" || it.Source != SourceProcessEnv {
		t.Errorf("FLEET_MAX_ITERATIONS = %+v, want 40 from the process env", it)
	}
	notes := strings.Join(it.Notes, "\n")
	if !strings.Contains(notes, "CUTLASS_MAX_ITERATIONS is set (env file), but it is a removed alias") ||
		!strings.Contains(notes, "FLEET_MAX_ITERATIONS is also in the env file, but the process env value wins") {
		t.Errorf("want removed-alias and masked-file notes, got %v", it.Notes)
	}
	// A removed alias as an argument is an error naming the canonical key.
	if _, err := explainRun(path, []string{"CHAT_MAX_ITERATIONS"}, false, nil); err == nil || !strings.Contains(err.Error(), "a removed alias of FLEET_MAX_ITERATIONS") {
		t.Errorf("explain CHAT_MAX_ITERATIONS: err = %v, want one naming FLEET_MAX_ITERATIONS", err)
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
