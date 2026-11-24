package store

import (
	"reflect"
	"testing"
)

func TestMCPAccountListRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      []string
		encoded string
		decoded []string
	}{
		{"nothing", nil, "", nil},
		{"default only stores no override", []string{""}, "", nil},
		{"one named account is stored as before", []string{"twc"}, "twc", []string{"twc"}},
		{"default first, then named in given order", []string{"twc", "", "arena"}, ",twc,arena", []string{"", "twc", "arena"}},
		{"named accounts keep their order", []string{"twc", "arena"}, "twc,arena", []string{"twc", "arena"}},
		{"repeats and blanks collapse", []string{" twc ", "twc", "", " "}, ",twc", []string{"", "twc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JoinMCPAccounts(tc.in)
			if got != tc.encoded {
				t.Fatalf("JoinMCPAccounts(%q) = %q, want %q", tc.in, got, tc.encoded)
			}
			if back := SplitMCPAccounts(got); !reflect.DeepEqual(back, tc.decoded) {
				t.Fatalf("SplitMCPAccounts(%q) = %q, want %q", got, back, tc.decoded)
			}
		})
	}
}

func TestIsMCPAccountList(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{"": false, "twc": false, ",twc": true, "twc,arena": true} {
		if got := IsMCPAccountList(value); got != want {
			t.Errorf("IsMCPAccountList(%q) = %v, want %v", value, got, want)
		}
	}
}
