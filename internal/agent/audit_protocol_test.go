package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditProtocolRef(t *testing.T) {
	dir := t.TempDir()
	if got := AuditProtocolRef(""); got != "" {
		t.Fatalf("no protocols dir → %q, want empty", got)
	}
	if got := AuditProtocolRef(dir); got != "" {
		t.Fatalf("protocols dir without the file → %q, want empty", got)
	}
	if err := os.Mkdir(filepath.Join(dir, AuditProtocolFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := AuditProtocolRef(dir); got != "" {
		t.Fatalf("a directory named like the file is not the protocol → %q", got)
	}
	if err := os.Remove(filepath.Join(dir, AuditProtocolFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AuditProtocolFile), []byte("# Self-Audit Protocol\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := AuditProtocolRef(" " + dir + " "); got != "protocols/self-audit.md" {
		t.Fatalf("shipped protocol → %q, want the workspace-relative path", got)
	}
}
