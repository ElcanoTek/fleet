package httpapi

import "testing"

func TestLogSafeStripsCRLF(t *testing.T) {
	got := logSafe("ok\nforged\rline")
	if got != "okforgedline" {
		t.Fatalf("logSafe = %q, want CR/LF stripped", got)
	}
	if logSafeSlug("a\nb") != "ab" {
		t.Fatal("logSafeSlug must use the same sanitizer")
	}
}
