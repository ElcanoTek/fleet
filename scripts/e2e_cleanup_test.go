package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestE2ESandboxCleanupOwnsOnlyUniqueWorkspaceMounts(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "podman")
	stub := `#!/usr/bin/env bash
set -eu
case "$1" in
  ps)
    [[ "$*" == 'ps -aq --filter label=fleet.instance' ]]
    printf '%s\n' own-chat own-probe other-live concurrent-test prefix-collision vanished
    ;;
  inspect)
    case "${!#}" in
      own-chat) printf '%s\n' "$OWN/workspace/conversation" ;;
      own-probe) printf '%s\n' "$OWN/probe-workspace" ;;
      other-live) printf '%s\n' '/var/lib/fleet/workspace/conversation' ;;
      concurrent-test) printf '%s\n' "$OTHER/workspace/conversation" ;;
      prefix-collision) printf '%s\n' "${OWN}-other/workspace" ;;
      vanished) exit 1 ;;
    esac
    ;;
  rm) printf '%s\n' "$3" >> "$REMOVED" ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(dir, "removed")
	cmd := exec.Command("bash", "-c", `
set -eu
source "$1"
OWN="$(e2e_create_workspace "$2")"
OTHER="$(e2e_create_workspace "$2")"
export OWN OTHER
[[ "$OWN" != "$OTHER" ]]
e2e_cleanup_sandboxes "$OWN"
if e2e_cleanup_sandboxes "$2"; then exit 1; fi
`, "test", filepath.Join(repoRoot(t), "scripts", "e2e-sandbox-cleanup.sh"), filepath.Join(dir, "custom-parent"))
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "REMOVED="+removed)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup harness: %v\n%s", err, out)
	}
	got, err := os.ReadFile(removed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "own-chat\nown-probe" {
		t.Fatalf("removed containers = %q; want only this run's chat and probe", got)
	}
}
