package config

import (
	"reflect"
	"testing"
)

// An ADMIN_EMAILS entry is checked as written before it is lowercased. One
// pgtext.CheckedEmail refuses is kept as written, so it matches no principal:
// a KELVIN SIGN entry does not become kate@x.com, the address lowercasing
// would turn it into.
func TestSplitEmailsKeepsRefusedEntriesAsWritten(t *testing.T) {
	got := splitEmails(" Ops@Example.com ,\u212aate@x.com,, bob\x01@x.com ")
	want := []string{"ops@example.com", "\u212aate@x.com", "bob\x01@x.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitEmails = %q, want %q", got, want)
	}
}
