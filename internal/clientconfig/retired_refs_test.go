package clientconfig

import (
	"strings"
	"testing"
)

// ADR-0106: a manifest reference to a removed CHAT_/CUTLASS_ spelling is
// reported with its FLEET_ name; the canonical connector wire key
// CUTLASS_ALLOWED_DIRS (every client bundle interpolates it) is not.
func TestRetiredManifestRefWarnings(t *testing.T) {
	got := retiredManifestRefWarnings([]string{"CUTLASS_ALLOWED_DIRS", "FLEET_WORKSPACE_ROOT", "CHAT_WORKSPACE_ROOT", "PROVIDER_API_KEY"})
	if len(got) != 1 || !strings.Contains(got[0], "${CHAT_WORKSPACE_ROOT}, a removed alias of FLEET_WORKSPACE_ROOT") {
		t.Fatalf("warnings = %q, want exactly one for CHAT_WORKSPACE_ROOT", got)
	}
}
