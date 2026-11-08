package envreg

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0106 (Codex P2 on #2081): every exported lookup in this package reads a
// removed spelling as unset, even when the process env holds it — the shipped
// systemd unit exports every env-file line, so a stale CHAT_SERVER_TOKEN can
// sit in os.Environ. Covers names fleet's code passes (Get/Lookup) and names
// the client bundle supplies (Unregistered/LookupUnregistered). A canonical
// CUTLASS_ wire key next to it still reads normally.
func TestEveryLookupReadsARetiredNameAsUnset(t *testing.T) {
	for _, name := range []string{"CHAT_SERVER_TOKEN", "CUTLASS_TEMPERATURE", "DATA_DIR"} {
		t.Setenv(name, "stale-value")
		lookups := map[string]func(string) (string, bool){
			"Get":                func(n string) (string, bool) { v := Get(n); return v, v != "" },
			"Lookup":             Lookup,
			"Unregistered":       func(n string) (string, bool) { v := Unregistered(n); return v, v != "" },
			"LookupUnregistered": LookupUnregistered,
		}
		for fn, lookup := range lookups {
			if v, ok := lookup(name); ok || v != "" {
				t.Errorf("%s(%s) = %q, %v; a removed spelling must read as unset", fn, name, v, ok)
			}
		}
	}
	t.Setenv("CUTLASS_ALLOWED_DIRS", "/srv/drops")
	if Get("CUTLASS_ALLOWED_DIRS") != "/srv/drops" || Unregistered("CUTLASS_ALLOWED_DIRS") != "/srv/drops" {
		t.Error("the canonical CUTLASS_ALLOWED_DIRS must still read")
	}
}

// The shell copy (scripts/lib/retired-env.sh, generated) agrees with
// RetiredAlias: doctor.sh and bootstrap.sh source it so a stale exported
// CHAT_SANDBOX_IMAGE cannot change the image they resolve from a manifest.
func TestRetiredEnvShellAgreesWithTheRegistry(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(moduleRoot(t), "scripts", "lib", "retired-env.sh")
	// Every registered name (all live), every removed spelling, their
	// account-suffixed variants, and the bundle wire keys.
	names := []string{"CUTLASS_RUN_WORKDIR", "CUTLASS_MOC_TASK_ID", "CUTLASS_REPORT_DIR", "CUTLASS_INPUT_DIR", "CUTLASS_USER_AGENT", "CHAT_NOT_A_SETTING"}
	for _, e := range All() {
		names = append(names, e.Name, e.Name+"_ACME", e.Name+"_CLIENT_A")
		for _, r := range RetiredAliasesOf(e.Name) {
			names = append(names, r, r+"_ACME", r+"_CLIENT_A", r+"_lower")
		}
	}
	script := `. "$1"; shift; for n in "$@"; do if fleet_env_name_retired "$n"; then echo "$n retired"; else echo "$n live"; fi; done`
	out, err := exec.Command(bash, append([]string{"-c", script, "bash", lib}, names...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, verdict, _ := strings.Cut(line, " ")
		_, retired := RetiredAlias(name)
		if (verdict == "retired") != retired {
			t.Errorf("shell says %s is %s; RetiredAlias says retired=%v", name, verdict, retired)
		}
	}
}

// Codex P2 on #2081: an account-suffixed removed spelling
// (<BASE>_<ACCOUNT>, creds.ApplyClientSuffix's form) is removed too and maps to
// the suffixed canonical; suffixed variants of live names never are.
func TestRetiredAliasCoversAccountSuffixes(t *testing.T) {
	for name, want := range map[string]string{
		"CHAT_SMTP_PASSWORD_ACME":        "FLEET_SMTP_PASSWORD_ACME",
		"CUTLASS_SMTP_PASSWORD_CLIENT_A": "FLEET_SMTP_PASSWORD_CLIENT_A",
		"DATA_DIR_ACME":                  "FLEET_DATA_DIR_ACME",
	} {
		if got, ok := RetiredAlias(name); !ok || got != want {
			t.Errorf("RetiredAlias(%s) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"FLEET_SMTP_PASSWORD_ACME", "CUTLASS_ALLOWED_DIRS_ACME", "CUTLASS_IMAGE_MODEL_ACME",
		"CHAT_RATE_PER_MIN_ACME", "CHAT_SMTP_PASSWORD_acme", "CHAT_SMTP_PASSWORD_",
		"CUTLASS_RUN_WORKDIR", "CUTLASS_MOC_TASK_ID", "CUTLASS_REPORT_DIR", "CUTLASS_INPUT_DIR",
	} {
		if c, ok := RetiredAlias(name); ok {
			t.Errorf("RetiredAlias(%s) = %q; it is live", name, c)
		}
	}
	t.Setenv("CHAT_SMTP_PASSWORD_ACME", "stale")
	t.Setenv("FLEET_SMTP_PASSWORD_ACME", "live")
	if v := Unregistered("CHAT_SMTP_PASSWORD_ACME"); v != "" {
		t.Errorf("a suffixed removed spelling was read: %q", v)
	}
	if v := Unregistered("FLEET_SMTP_PASSWORD_ACME"); v != "live" {
		t.Errorf("a live suffixed name did not resolve: %q", v)
	}
}

// ADR-0108: the internal service token is a setting removed outright. Its
// names (and the CHAT_/CUTLASS_ spellings ADR-0106 had already removed) read
// as unset, are reported with no canonical name, and RetiredFix says what
// replaced them.
func TestRemovedServiceTokenSettings(t *testing.T) {
	for _, name := range []string{"FLEET_SERVER_TOKEN", "ORCHESTRATOR_SERVER_TOKEN", "CHAT_SERVER_TOKEN", "CUTLASS_SERVER_TOKEN"} {
		if IsRegistered(name) {
			t.Errorf("%s is still registered", name)
		}
		c, ok := RetiredAlias(name)
		if !ok || c != "" {
			t.Errorf("RetiredAlias(%s) = %q, %v; want \"\", true", name, c, ok)
		}
		if fix := RetiredFix(name); !strings.Contains(fix, "ADR-0108") || !strings.Contains(fix, "--type user") {
			t.Errorf("RetiredFix(%s) = %q", name, fix)
		}
		t.Setenv(name, "stale-secret")
		if v := Unregistered(name); v != "" {
			t.Errorf("%s was read: %q", name, v)
		}
	}
	if fix := RetiredFix("CHAT_MAX_COST_USD"); fix != "rename it to FLEET_MAX_COST_USD (ADR-0106)" {
		t.Errorf("alias fix = %q", fix)
	}
	// A per-account suffix of a removed setting is removed with it.
	if c, ok := RetiredAlias("FLEET_SERVER_TOKEN_ACME"); !ok || c != "" || !strings.Contains(RetiredFix("FLEET_SERVER_TOKEN_ACME"), "ADR-0108") {
		t.Errorf("FLEET_SERVER_TOKEN_ACME: %q, %v", c, ok)
	}
	if RetiredFix("FLEET_MAX_COST_USD") != "" {
		t.Error("a live setting has a fix")
	}
}
