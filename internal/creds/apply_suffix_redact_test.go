package creds

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// A brokered account's suffixed value comes from the host, so it is
// registered with the log redactor whatever the variable's name (#2074).
func TestApplyClientSuffixRegistersTheAccountValue(t *testing.T) {
	const v = "acctSessionOpaque-7Rw2Lp"
	t.Setenv("VENDOR_SESSION_CLIENTA", v)
	out, n := ApplyClientSuffix(map[string]string{"VENDOR_SESSION": "default"}, "clienta")
	if n != 1 || out["VENDOR_SESSION"] != v {
		t.Fatalf("overlay = %v (%d)", out, n)
	}
	if got := logging.Redact("echo " + v); strings.Contains(got, v) {
		t.Fatalf("account value not registered: %q", got)
	}
}
