package db

import (
	"database/sql"
	"testing"
)

// TestUnmarshalCredentialAllowlistFailsClosed pins the failure direction of the
// allowlist decoder. nil means "no allowlist → every global seat" (#184), so a
// row whose JSON cannot be read must NOT decode to nil: it decodes to the
// empty, non-nil list — deny all — and the task fails loudly on its first
// credentialed call instead of running with seats nobody granted it.
func TestUnmarshalCredentialAllowlistFailsClosed(t *testing.T) {
	if got := unmarshalCredentialAllowlist(sql.NullString{}); got != nil {
		t.Fatalf("NULL column must stay nil (inherit), got %#v", got)
	}
	if got := unmarshalCredentialAllowlist(sql.NullString{Valid: true, String: `[]`}); got == nil || len(got) != 0 {
		t.Fatalf("empty list must round-trip as empty non-nil, got %#v", got)
	}
	got := unmarshalCredentialAllowlist(sql.NullString{Valid: true, String: `{"server":"github"}`})
	if got == nil {
		t.Fatal("unreadable allowlist decoded to nil — that grants every seat")
	}
	if len(got) != 0 {
		t.Fatalf("unreadable allowlist must be deny-all (empty), got %#v", got)
	}
}
