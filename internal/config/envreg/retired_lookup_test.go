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
	script := `. "$1"; for n in CHAT_SANDBOX_IMAGE CUTLASS_TEMPERATURE DATA_DIR CUTLASS_ALLOWED_DIRS CHAT_RATE_PER_MIN FLEET_SANDBOX_IMAGE; do if fleet_env_name_retired "$n"; then echo "$n retired"; else echo "$n live"; fi; done`
	out, err := exec.Command(bash, "-c", script, "bash", lib).CombinedOutput()
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
