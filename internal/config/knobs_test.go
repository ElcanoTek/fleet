package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── #1119: the loader fails LOUD on malformed numeric/bool/duration values ──
//
// Before #1119 every typed getenv* helper swallowed parse errors and silently
// returned the default — fail-OPEN for security knobs (FLEET_LOCKDOWN_ONLY=
// enabled left lockdown off) and silently wrong for spend ceilings
// (FLEET_MAX_COST_USD=5O booted with the $50 default). These tests pin the new
// contract: unset (or blank) → default; set-but-malformed → Load refuses,
// naming the variable, the offending value, and the expected format.

// TestLoad_MalformedKnobFailsLoud covers one representative knob per kind plus
// the two motivating cases from the issue.
func TestLoad_MalformedKnobFailsLoud(t *testing.T) {
	cases := []struct {
		envKey, value string
		// wantMention must all appear in the Load error.
		wantMention []string
	}{
		// The fail-open security knob: an unrecognized token must refuse to
		// boot, never silently leave lockdown OFF.
		{"FLEET_LOCKDOWN_ONLY", "enabled", []string{"FLEET_LOCKDOWN_ONLY", `"enabled"`, "boolean"}},
		// The typo'd spend ceiling (letter O, not zero).
		{"FLEET_MAX_COST_USD", "5O", []string{"FLEET_MAX_COST_USD", `"5O"`, "number"}},
		// int / int64 / duration / bool via direct keys and the alias chain.
		{"CONVERSATION_TTL_DAYS", "two weeks", []string{"CONVERSATION_TTL_DAYS", "integer"}},
		{"FLEET_UPLOAD_MAX_BYTES", "1G", []string{"FLEET_UPLOAD_MAX_BYTES", `"1G"`, "integer"}},
		{"FLEET_CHAT_DB_CONNECT_TIMEOUT", "30", []string{"FLEET_CHAT_DB_CONNECT_TIMEOUT", `"30"`, "duration"}},
		{"FLEET_SEARCH_ENABLED", "enabled", []string{"FLEET_SEARCH_ENABLED", "boolean"}},
		// A legacy alias spelling is parsed just as strictly; the error names
		// the canonical spelling (matching what the reload path reports).
		{"CUTLASS_MAX_COST_USD", "free", []string{"FLEET_MAX_COST_USD", `"free"`}},
		{"CHAT_MAX_ITERATIONS", "many", []string{"FLEET_MAX_ITERATIONS", `"many"`}},
		// Out-of-range values on the bounded knobs (the same bounds the
		// hot-reload path has always enforced).
		{"FLEET_MAX_ITERATIONS", "20000", []string{"FLEET_MAX_ITERATIONS", "between 1 and 10000"}},
		{"FLEET_MAX_ITERATIONS", "0", []string{"FLEET_MAX_ITERATIONS", "between 1 and 10000"}},
		{"FLEET_MAX_TOTAL_TOKENS", "-5", []string{"FLEET_MAX_TOTAL_TOKENS", ">= 0"}},
		{"FLEET_TEMPERATURE", "-0.1", []string{"FLEET_TEMPERATURE", ">= 0"}},
		{"FLEET_MAX_COST_USD", "-1", []string{"FLEET_MAX_COST_USD", ">= 0"}},
		{"FLEET_MAX_CONCURRENT_AGENTS", "-2", []string{"FLEET_MAX_CONCURRENT_AGENTS", ">= 1"}},
		// 0 is NOT "use a default" here: `fleet serve` would feed it to
		// admission.New, which floors it to a box-wide cap of ONE turn.
		{"FLEET_MAX_CONCURRENT_AGENTS", "0", []string{"FLEET_MAX_CONCURRENT_AGENTS", ">= 1"}},
		{"FLEET_INPUT_QUEUE_RETENTION_DAYS", "-1", []string{"FLEET_INPUT_QUEUE_RETENTION_DAYS", ">= 0"}},
	}
	for _, tc := range cases {
		t.Run(tc.envKey+"="+tc.value, func(t *testing.T) {
			isolateEnv(t)
			chdir(t, t.TempDir())
			t.Setenv(tc.envKey, tc.value)
			_, err := Load("")
			if err == nil {
				t.Fatalf("Load with %s=%q: want error, got nil", tc.envKey, tc.value)
			}
			for _, want := range tc.wantMention {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error should mention %q, got: %v", want, err)
				}
			}
		})
	}
}

// TestLoad_MalformedKnobsAllReportedAtOnce: boot reports EVERY offending
// variable in one error so the operator fixes the file in one pass.
func TestLoad_MalformedKnobsAllReportedAtOnce(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_MAX_COST_USD", "5O")
	t.Setenv("FLEET_LOCKDOWN_ONLY", "enabled")
	_, err := Load("")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "FLEET_MAX_COST_USD") || !strings.Contains(err.Error(), "FLEET_LOCKDOWN_ONLY") {
		t.Errorf("Load error should name both offenders, got: %v", err)
	}
}

// TestLoad_KnobValueNormalization pins the unified value cleaning: valid
// values parse; a quoted value (podman/docker --env-file keeps quotes) parses
// identically; whitespace-only and quoted-empty values count as UNSET (the
// default applies, no error); the same holds when the value arrives via the
// env file.
func TestLoad_KnobValueNormalization(t *testing.T) {
	t.Run("valid and quoted values parse the same", func(t *testing.T) {
		for _, val := range []string{"12.5", `"12.5"`, `'12.5'`, "  12.5  "} {
			isolateEnv(t)
			chdir(t, t.TempDir())
			t.Setenv("FLEET_MAX_COST_USD", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load with FLEET_MAX_COST_USD=%s: %v", val, err)
			}
			if cfg.MaxCostUSD != 12.5 {
				t.Errorf("FLEET_MAX_COST_USD=%s parsed to %v, want 12.5", val, cfg.MaxCostUSD)
			}
		}
	})

	t.Run("blank counts as unset", func(t *testing.T) {
		for _, val := range []string{"   ", `""`, `''`} {
			isolateEnv(t)
			chdir(t, t.TempDir())
			t.Setenv("FLEET_MAX_COST_USD", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load with blank FLEET_MAX_COST_USD=%q: %v", val, err)
			}
			if cfg.MaxCostUSD != 50.0 {
				t.Errorf("blank value %q should keep the default 50, got %v", val, cfg.MaxCostUSD)
			}
		}
	})

	t.Run("env-file values follow the same rules", func(t *testing.T) {
		isolateEnv(t)
		chdir(t, t.TempDir())
		envPath := filepath.Join(t.TempDir(), ".env")
		writeEnv(t, envPath, "FLEET_MAX_COST_USD=\"12.5\"\nFLEET_MAX_ITERATIONS=250 # inline comment\n")
		cfg, err := Load(envPath)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MaxCostUSD != 12.5 || cfg.MaxIterations != 250 {
			t.Errorf("got cost=%v iters=%d, want 12.5/250", cfg.MaxCostUSD, cfg.MaxIterations)
		}

		// The first Load exported the file values into the process env; drop
		// them so the malformed re-load sees only the file (in a real boot the
		// process is fresh and "process env wins" cannot mask the file).
		os.Unsetenv("FLEET_MAX_COST_USD")
		os.Unsetenv("FLEET_MAX_ITERATIONS")
		writeEnv(t, envPath, "FLEET_MAX_COST_USD=5O\n")
		if _, err := Load(envPath); err == nil || !strings.Contains(err.Error(), "FLEET_MAX_COST_USD") {
			t.Errorf("malformed file value must fail Load naming the var, got %v", err)
		}
	})
}

// TestLoad_BoolTokens pins the accepted boolean vocabulary on a security-
// relevant knob, including case-insensitivity.
func TestLoad_BoolTokens(t *testing.T) {
	accept := map[string]bool{
		"1": true, "true": true, "yes": true, "on": true, "TRUE": true, "On": true,
		"0": false, "false": false, "no": false, "off": false, "FALSE": false,
	}
	for val, want := range accept {
		isolateEnv(t)
		chdir(t, t.TempDir())
		t.Setenv("FLEET_LOCKDOWN_ONLY", val)
		t.Setenv("FLEET_SANDBOX_IMAGE", "img:latest") // so lockdown is enforceable
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("Load with FLEET_LOCKDOWN_ONLY=%s: %v", val, err)
		}
		if cfg.LockdownOnly != want {
			t.Errorf("FLEET_LOCKDOWN_ONLY=%s → %v, want %v", val, cfg.LockdownOnly, want)
		}
	}
	for _, val := range []string{"enabled", "2", "yep", "tru"} {
		isolateEnv(t)
		chdir(t, t.TempDir())
		t.Setenv("FLEET_LOCKDOWN_ONLY", val)
		if _, err := Load(""); err == nil {
			t.Errorf("FLEET_LOCKDOWN_ONLY=%s must refuse to boot (fail-open otherwise)", val)
		}
	}
}

// TestBootAndReloadAgreeOnKnobValues: the same env value produces the same
// accept/reject outcome at boot (Load) and on hot-reload (Reload) — the #1119
// acceptance criterion that closes the old disagreement where boot silently
// defaulted a value reload loudly rejected.
func TestBootAndReloadAgreeOnKnobValues(t *testing.T) {
	// Every ACCEPT value below must differ from the knob's built-in default
	// (cost 50, iterations 300, tokens 10000000, temperature 0.3): the test
	// asserts the reload actually APPLIED the value (res.Changed names the
	// key), which a value equal to the running default would never show.
	cases := []struct {
		suffix, value string
		wantReject    bool
	}{
		{"MAX_COST_USD", "22.5", false},
		// 0 = no cost ceiling (the documented agentcore budget convention) —
		// accepted, not confused with "unset".
		{"MAX_COST_USD", "0", false},
		{"MAX_COST_USD", "5O", true},
		{"MAX_COST_USD", "-1", true},
		{"MAX_ITERATIONS", "250", false},
		{"MAX_ITERATIONS", "20000", true},
		{"MAX_ITERATIONS", "abc", true},
		{"MAX_TOTAL_TOKENS", "1000000", false},
		{"MAX_TOTAL_TOKENS", "-5", true},
		{"TEMPERATURE", "0.9", false},
		{"TEMPERATURE", "hot", true},
		{"TEMPERATURE", "-0.1", true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("FLEET_%s=%s", tc.suffix, tc.value), func(t *testing.T) {
			// Reload side: boot clean, then feed the value via the env file.
			isolateEnv(t)
			chdir(t, t.TempDir())
			envPath := filepath.Join(t.TempDir(), ".env")
			writeEnv(t, envPath, "")
			cfg, err := Load(envPath)
			if err != nil {
				t.Fatalf("clean Load: %v", err)
			}
			writeEnv(t, envPath, "FLEET_"+tc.suffix+"="+tc.value+"\n")
			res, err := cfg.Reload(envPath)
			if err != nil {
				t.Fatalf("Reload: %v", err)
			}
			reloadRejected := len(res.Errors) > 0

			// Boot side: the same value in the process env.
			t.Setenv("FLEET_"+tc.suffix, tc.value)
			_, bootErr := Load(envPath)
			bootRejected := bootErr != nil

			if bootRejected != tc.wantReject {
				t.Errorf("boot: rejected=%v, want %v (err=%v)", bootRejected, tc.wantReject, bootErr)
			}
			if reloadRejected != tc.wantReject {
				t.Errorf("reload: rejected=%v, want %v (errors=%+v)", reloadRejected, tc.wantReject, res.Errors)
			}
			if !tc.wantReject {
				// "No errors" is not enough — an accept case would pass
				// vacuously if the reload never saw the value. Assert it was
				// actually applied.
				applied := false
				for _, ch := range res.Changed {
					if ch.Key == "FLEET_"+tc.suffix {
						applied = true
						break
					}
				}
				if !applied {
					t.Errorf("reload accepted FLEET_%s=%s but did not apply it (Changed=%+v)", tc.suffix, tc.value, res.Changed)
				}
			}
			if bootRejected != reloadRejected {
				t.Errorf("boot and reload DISAGREE: boot rejected=%v, reload rejected=%v", bootRejected, reloadRejected)
			}
		})
	}
}

// TestLoad_ZeroCostCeilingAccepted: FLEET_MAX_COST_USD=0 means "no cost
// ceiling" (the agentcore budget convention: 0 = unlimited) — boot must accept
// it and carry the 0 through, never confuse it with unset or reject it.
func TestLoad_ZeroCostCeilingAccepted(t *testing.T) {
	isolateEnv(t)
	chdir(t, t.TempDir())
	t.Setenv("FLEET_MAX_COST_USD", "0")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with FLEET_MAX_COST_USD=0: %v", err)
	}
	if cfg.MaxCostUSD != 0 {
		t.Errorf("MaxCostUSD = %v, want 0 (no cost ceiling)", cfg.MaxCostUSD)
	}
}

// TestGetenvDefaultHelpersStripQuotes pins the #1119 quote unification on the
// two string helpers that historically did NOT strip: a value stored with a
// surrounding quote layer (podman/docker --env-file keep quotes in place)
// resolves to the bare interior — identical to getenvFleet/getenvFleetOrBare —
// and an unset var still yields the default verbatim.
func TestGetenvDefaultHelpersStripQuotes(t *testing.T) {
	isolateEnv(t)
	t.Setenv("FLEET_DATA_DIR", `"json"`)
	if got := getenvFleetDefault("DATA_DIR", "fallback"); got != "json" {
		t.Errorf(`getenvFleetDefault with FLEET_DATA_DIR="json" (quoted) = %q, want %q`, got, "json")
	}
	t.Setenv("FLEET_TLS_MODE", `'auto'`)
	if got := getenvDefault("FLEET_TLS_MODE", "off"); got != "auto" {
		t.Errorf(`getenvDefault with FLEET_TLS_MODE='auto' (quoted) = %q, want %q`, got, "auto")
	}
	// Unset resolves to the default, returned verbatim (no quote stripping).
	if got := getenvFleetDefault("DEFINITELY_UNSET_KNOB", `"keep"`); got != `"keep"` {
		t.Errorf("unset default = %q, want it returned verbatim", got)
	}
}

// TestValidateEnvKnobs_FlagsEveryRegisteredKnob drives the registry itself:
// with EVERY registered knob set to a value no kind accepts, ValidateEnvKnobs
// must flag every single one, naming the canonical env var — proving the
// `fleet validate-config` preflight covers the whole table, not a hand-picked
// subset.
func TestValidateEnvKnobs_FlagsEveryRegisteredKnob(t *testing.T) {
	isolateEnv(t)
	for i := range envKnobs {
		// "zzz" parses under no kind: not a number, not a duration, not a
		// recognized boolean token.
		t.Setenv(envKnobs[i].key, "zzz")
	}
	problems := ValidateEnvKnobs()
	if len(problems) != len(envKnobs) {
		t.Errorf("ValidateEnvKnobs flagged %d problems, want %d (one per registered knob)", len(problems), len(envKnobs))
	}
	flagged := map[string]bool{}
	for _, p := range problems {
		key, _, ok := strings.Cut(p, ":")
		if !ok {
			t.Errorf("problem %q does not lead with the env var name", p)
			continue
		}
		flagged[key] = true
	}
	for i := range envKnobs {
		if !flagged[envKnobs[i].key] {
			t.Errorf("knob %s not flagged by ValidateEnvKnobs", envKnobs[i].key)
		}
	}

	// And with a clean environment it reports nothing.
	isolateEnv(t)
	if problems := ValidateEnvKnobs(); len(problems) != 0 {
		t.Errorf("clean env should yield no problems, got %v", problems)
	}
}
