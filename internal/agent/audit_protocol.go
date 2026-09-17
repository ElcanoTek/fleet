package agent

import (
	"os"
	"path/filepath"
	"strings"
)

// AuditProtocolFile is the bundle file the self-audit ritual reads before
// confirm_audit, resolved under the bundle's protocols directory.
const AuditProtocolFile = "self-audit.md"

// auditProtocolWorkspaceRef is how the model reaches that file from its
// working directory: every run workspace carries a `protocols` symlink to the
// bundle's protocols directory (tools.SeedSupportingDocSymlinks /
// EnsureWorkspaceDir), so the workspace-relative path is what the guidance
// texts should name.
const auditProtocolWorkspaceRef = "protocols/" + AuditProtocolFile

// AuditProtocolRef reports the workspace-relative path the audit guidance
// should send the model to, or "" when the bundle's protocols directory has no
// self-audit.md — then the finish nudge and the BLOCKED text ask for the audit
// without naming a file. A bundle that shipped none (Reklaim, 2026-09-17) had
// its fallback model take "read protocols/self-audit.md" literally, find
// nothing, and abort a finished report as FAILED / INCOMPLETE.
func AuditProtocolRef(protocolsDir string) string {
	dir := strings.TrimSpace(protocolsDir)
	if dir == "" {
		return ""
	}
	info, err := os.Stat(filepath.Join(dir, AuditProtocolFile))
	if err != nil || info.IsDir() {
		return ""
	}
	return auditProtocolWorkspaceRef
}
