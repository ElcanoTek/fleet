package config

import (
	"path/filepath"
	"testing"
)

// TestEmailAttachmentDirIsAbsolute: the default "./data/attachments" and a
// relative operator value both load as ABSOLUTE paths under the cwd. Every
// consumer derives paths from it, and the outbound attachment copies in
// particular are read by MCP subprocesses running in a different cwd.
func TestEmailAttachmentDirIsAbsolute(t *testing.T) {
	isolateEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "data", "attachments"); cfg.EmailAttachmentDir != want {
		t.Fatalf("default EmailAttachmentDir = %q, want %q", cfg.EmailAttachmentDir, want)
	}

	t.Setenv("EMAIL_ATTACHMENT_DIR", "state/att")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "state", "att"); cfg.EmailAttachmentDir != want {
		t.Fatalf("relative EmailAttachmentDir = %q, want %q", cfg.EmailAttachmentDir, want)
	}
}
