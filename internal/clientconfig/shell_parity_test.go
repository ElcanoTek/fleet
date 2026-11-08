package clientconfig

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellResolverMatchesGo (Codex P2 on #2081) — the shell resolver the
// install/update/doctor scripts use for a manifest sandbox scalar
// (fleet_sandbox_resolve_scalar, scripts/lib/sandbox-bundle.sh) and the Go
// loader agree on every interpolation form: ${X}, ${X:-d}, ${X:?m}, for a live
// and a removed (ADR-0106) name, set, set-empty and unset. Where the loader
// refuses the bundle, the shell resolver fails (non-zero exit) — and for a
// removed name both name the FLEET_ replacement; a removed name is unset on
// both sides whatever its value.
func TestShellResolverMatchesGo(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	lib, err := filepath.Abs(filepath.Join("..", "..", "scripts", "lib", "sandbox-bundle.sh"))
	if err != nil {
		t.Fatal(err)
	}
	const live, retired = "PARITY_SANDBOX_IMG", "CHAT_SANDBOX_IMAGE"
	exprs := []string{
		"${" + live + "}", "${" + live + ":-registry.example/def:1}", "${" + live + ":?set the image}",
		"${" + retired + "}", "${" + retired + ":-registry.example/def:1}", "${" + retired + ":?set the image}",
		"registry.example/literal:2",
	}
	states := map[string]*string{"unset": nil, "empty": new(string), "set": func() *string { s := "  registry.example/live:3  "; return &s }()}
	for _, expr := range exprs {
		for state, val := range states {
			t.Run(expr+"/"+state, func(t *testing.T) {
				unsetTracked(t, live, retired)
				armEnvFile(t, "")
				if val != nil {
					t.Setenv(live, *val)
					t.Setenv(retired, *val)
				}
				dir := writeManifest(t, "sandbox:\n  image: \""+expr+"\"\n")

				var goOut string
				b, goErr := Load(dir)
				if goErr == nil {
					goOut = b.Sandbox().Image
				}

				cmd := exec.Command(bash, "-c", `set -u; . "$1"; fleet_sandbox_resolve_scalar image "$2" fleet_shell_env_value`, "bash", lib, filepath.Join(dir, "manifest.yaml"))
				var stderr strings.Builder
				cmd.Stderr = &stderr
				out, shErr := cmd.Output()
				shOut := strings.TrimSpace(string(out))

				if (goErr != nil) != (shErr != nil) {
					t.Fatalf("go err=%v, shell err=%v (shell out %q)", goErr, shErr, shOut)
				}
				if goErr == nil && goOut != shOut {
					t.Fatalf("go %q, shell %q", goOut, shOut)
				}
				const rename = "rename it to FLEET_SANDBOX_IMAGE"
				if goErr != nil && strings.Contains(goErr.Error(), rename) != strings.Contains(stderr.String(), rename) {
					t.Fatalf("rename message: go %v, shell %q", goErr, stderr.String())
				}
			})
		}
	}
}
