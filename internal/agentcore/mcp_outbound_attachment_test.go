package agentcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// TestSpawnEnvAdmitsOutboundAttachmentDir pins the outbound-attachment half of
// a connector's read allowlist: an *ALLOWED_DIRS key that already lists
// ${FLEET_WORKSPACE_ROOT} gets the pinned staging dir appended (once, with the
// path-list separator) by SpawnEnv, the one function every spawn path and the
// broker scope binder go through; nothing else changes. Without it the email
// connector refused every snapshot copy fleet handed it ("Inline attachment
// file not found" on a scheduled send with an inline logo).
func TestSpawnEnvAdmitsOutboundAttachmentDir(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(t.TempDir(), "data", "attachments", "outbound")
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	t.Setenv(OutboundAttachmentDirEnv, "")
	PinOutboundAttachmentDir(staging)
	sep := string(os.PathListSeparator)

	env := map[string]string{
		"CUTLASS_ALLOWED_DIRS":   "/srv/drops" + sep + WorkspaceRootEnvToken,
		"ROOT_ONLY_ALLOWED_DIRS": WorkspaceRootEnvToken,
		"OTHER_ALLOWED_DIRS":     "/srv/drops",
		"REPORT_ALLOWED_DIRS":    WorkspaceRootEnvToken + "/reports",
		"CUTLASS_REPORT_DIR":     WorkspaceRootEnvToken,
		"CUTLASS_RUN_WORKDIR":    WorkspaceEnvToken,
	}
	out := SpawnEnv(env, "/work/task-1", "", MCPUserIdentity{})
	if got, want := out["CUTLASS_ALLOWED_DIRS"], "/srv/drops"+sep+root+sep+staging; got != want {
		t.Errorf("CUTLASS_ALLOWED_DIRS = %q, want %q", got, want)
	}
	if got, want := out["ROOT_ONLY_ALLOWED_DIRS"], root+sep+staging; got != want {
		t.Errorf("ROOT_ONLY_ALLOWED_DIRS = %q, want %q", got, want)
	}
	if got := out["OTHER_ALLOWED_DIRS"]; got != "/srv/drops" {
		t.Errorf("an allowlist that does not admit the root gained the staging dir: %q", got)
	}
	if got := out["REPORT_ALLOWED_DIRS"]; got != filepath.Join(root, "reports") {
		t.Errorf("a subpath of the root does not admit the root: got %q", got)
	}
	if got := out["CUTLASS_REPORT_DIR"]; got != root {
		t.Errorf("a single-directory key must stay the root, got %q", got)
	}
	if got := out["CUTLASS_RUN_WORKDIR"]; got != "/work/task-1" {
		t.Errorf("CUTLASS_RUN_WORKDIR = %q", got)
	}
	if env["CUTLASS_ALLOWED_DIRS"] != "/srv/drops"+sep+WorkspaceRootEnvToken {
		t.Error("input map was mutated")
	}
	// Idempotent: a value already listing the dir is left alone.
	again := AdmitOutboundAttachmentDir(map[string]string{"CUTLASS_ALLOWED_DIRS": staging + sep + WorkspaceRootEnvToken})
	if strings.Count(again["CUTLASS_ALLOWED_DIRS"], staging) != 1 {
		t.Errorf("staging dir appended twice: %q", again["CUTLASS_ALLOWED_DIRS"])
	}

	// No staging dir pinned: the allowlist is exactly the bundle's.
	PinOutboundAttachmentDir("")
	if got := SpawnEnv(env, "", "", MCPUserIdentity{})["CUTLASS_ALLOWED_DIRS"]; got != "/srv/drops"+sep+root {
		t.Errorf("with no staging dir pinned the allowlist must be unchanged, got %q", got)
	}
	// A hand-set relative value is ignored rather than admitted.
	t.Setenv(OutboundAttachmentDirEnv, "data/attachments/outbound")
	if got := AdmitOutboundAttachmentDir(env); got["CUTLASS_ALLOWED_DIRS"] != env["CUTLASS_ALLOWED_DIRS"] {
		t.Errorf("a relative staging dir must not be admitted, got %q", got["CUTLASS_ALLOWED_DIRS"])
	}
	// A relative pin is made absolute against the cwd.
	cwd := t.TempDir()
	t.Chdir(cwd)
	PinOutboundAttachmentDir("data/attachments/outbound")
	if got := envreg.Get(OutboundAttachmentDirEnv); !filepath.IsAbs(got) || !strings.HasSuffix(got, filepath.Join("data", "attachments", "outbound")) {
		t.Errorf("pinned dir must be absolute, got %q", got)
	}
}
