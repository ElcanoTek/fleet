package config

import (
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// TestAllowlistMatchesPreRegistrySnapshot — the env-file allowlist is derived
// from the registry (#2053); it must admit EXACTLY the keys the hand-kept map
// it replaced admitted. A key that disappears is silently dropped from every
// env file that sets it; a key that appears changes what a file can set.
func TestAllowlistMatchesPreRegistrySnapshot(t *testing.T) {
	var missing, extra []string
	for k := range preRegistryAllowedEnvVars {
		if !allowedEnvVars[k] {
			missing = append(missing, k)
		}
	}
	for k := range allowedEnvVars {
		if !preRegistryAllowedEnvVars[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, k := range missing {
		t.Errorf("%s was admitted from the env file before the registry and no longer is", k)
	}
	for _, k := range extra {
		t.Errorf("%s is admitted from the env file now but was not before the registry", k)
	}
	if len(allowedEnvVars) != len(preRegistryAllowedEnvVars) {
		t.Errorf("allowlist has %d keys, the pre-registry map had %d", len(allowedEnvVars), len(preRegistryAllowedEnvVars))
	}
}

// TestAllowlistAdmitsEveryOldKeyFromAFile is the table test over the old
// allowlist the brief asks for: for every key the pre-registry map held, a
// value set ONLY in the env file reaches the process environment through the
// real loader path (applyEnvFile), and a process-env value still wins.
func TestAllowlistAdmitsEveryOldKeyFromAFile(t *testing.T) {
	keys := make([]string, 0, len(preRegistryAllowedEnvVars))
	for k := range preRegistryAllowedEnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		t.Setenv(k, "")
		b.WriteString(k + "=file-" + k + "\n")
	}
	path := t.TempDir() + "/env"
	writeEnv(t, path, b.String())
	// One key pinned in the process env must win over the file.
	t.Setenv("FLEET_MAX_ITERATIONS", "7")
	for _, k := range keys {
		if k != "FLEET_MAX_ITERATIONS" {
			_ = os.Unsetenv(k)
		}
	}
	if _, err := applyEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		want := "file-" + k
		if k == "FLEET_MAX_ITERATIONS" {
			want = "7"
		}
		if got := envreg.Get(k); got != want {
			t.Errorf("%s = %q after applyEnvFile, want %q", k, got, want)
		}
	}
}

// TestKnobsMatchPreRegistrySnapshot — the typed-knob table is derived from
// the registry; every row must match the hand-kept table it replaced, field
// for field (kind, alias chain, bounds, scope, consumer, leniency).
func TestKnobsMatchPreRegistrySnapshot(t *testing.T) {
	old := map[string]envKnob{}
	for _, k := range preRegistryEnvKnobs {
		old[k.key] = k
	}
	got := map[string]envKnob{}
	for _, k := range envKnobs {
		got[k.key] = k
	}
	for key, o := range old {
		n, ok := got[key]
		if !ok {
			t.Errorf("knob %s disappeared from the derived table", key)
			continue
		}
		if n.fleet != o.fleet || n.kind != o.kind || n.scope != o.scope || n.readBy != o.readBy ||
			n.lenient != o.lenient || n.why != o.why || !sameBound(n.min, o.min) || !sameBound(n.max, o.max) {
			t.Errorf("knob %s changed:\n  was %+v\n  now %+v", key, describeKnob(o), describeKnob(n))
		}
	}
	for key := range got {
		if _, ok := old[key]; !ok {
			t.Errorf("knob %s is new in the derived table (a boot-validation change)", key)
		}
	}
}

func sameBound(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func describeKnob(k envKnob) string {
	f := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return formatBound(*p)
	}
	return strings.Join([]string{k.key, k.kind.label(), "fleet=" + boolStr(k.fleet), "min=" + f(k.min), "max=" + f(k.max), "readBy=" + k.readBy, "lenient=" + boolStr(k.lenient)}, " ")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestLookupFleetSpellingsMatchPrefixFamily — lookupFleet now takes its
// spellings from the registry; for every chain entry they must be exactly the
// FLEET_/CHAT_/CUTLASS_ family the hard-coded loop produced, in that order.
func TestLookupFleetSpellingsMatchPrefixFamily(t *testing.T) {
	for _, e := range envreg.All() {
		if !e.Chain {
			continue
		}
		suffix := strings.TrimPrefix(e.Name, "FLEET_")
		want := []string{"FLEET_" + suffix, "CHAT_" + suffix, "CUTLASS_" + suffix}
		got := envreg.ChainSpellings(e.Name)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s chain = %v, want %v", e.Name, got, want)
		}
		if strings.Join(EnvAliases(e.Name), ",") != strings.Join(want, ",") {
			t.Errorf("EnvAliases(%s) = %v, want %v", e.Name, EnvAliases(e.Name), want)
		}
	}
}

// TestEveryDeprecatedFileAliasHasAFileSettableCanonical — `fleet config
// migrate-env` rewrites an alias line to its canonical key; that is only
// behaviour-preserving when the env file admits the canonical spelling too.
//
// LEGACY-ENV-ALIAS: remove after migrate-env
func TestEveryDeprecatedFileAliasHasAFileSettableCanonical(t *testing.T) {
	for _, e := range envreg.All() {
		for _, a := range e.DeprecatedAliases() {
			if a.EnvFile && !e.FileSettable() {
				t.Errorf("%s is admitted from the env file but its canonical %s is not — migrate-env would drop the setting", a.Name, e.Name)
			}
		}
	}
}
