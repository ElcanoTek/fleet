package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCredentialAllowlistJSONRoundTrip guards the nil-vs-empty distinction (#184)
// across JSON marshaling — the boundary an API response or a node TaskAssignment
// crosses. `omitempty` on the field would drop a non-nil empty (deny-all) slice
// and decode it back as nil (inherit global), silently inverting the most
// restrictive setting into the least restrictive.
func TestCredentialAllowlistJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		al          CredentialAllowlist
		wantInJSON  string
		wantNilBack bool
		wantLenBack int
	}{
		{"nil → null (inherit global)", nil, `"credential_allowlist":null`, true, 0},
		{"empty → [] (deny all, NOT omitted)", CredentialAllowlist{}, `"credential_allowlist":[]`, false, 0},
		{"populated", CredentialAllowlist{{Server: "github", Account: "client_a"}}, `"credential_allowlist":[{"server":"github","account":"client_a"}]`, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := Task{CredentialAllowlist: tt.al}
			b, err := json.Marshal(task)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), tt.wantInJSON) {
				t.Errorf("JSON missing %q in %s", tt.wantInJSON, b)
			}
			var back Task
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tt.wantNilBack && back.CredentialAllowlist != nil {
				t.Errorf("nil must round-trip as nil, got %#v", back.CredentialAllowlist)
			}
			if !tt.wantNilBack {
				if back.CredentialAllowlist == nil {
					t.Errorf("non-nil must round-trip as non-nil (deny-all/populated preserved)")
				}
				if len(back.CredentialAllowlist) != tt.wantLenBack {
					t.Errorf("len = %d, want %d", len(back.CredentialAllowlist), tt.wantLenBack)
				}
			}
		})
	}
}
