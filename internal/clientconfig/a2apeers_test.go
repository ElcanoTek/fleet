package clientconfig

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// a2aPeerFixture is the a2a_peers[] manifest both parse tests load. The header
// carries a ${HELPDESK_A2A_KEY} reference so env resolution and the allowlist
// behavior can be exercised by toggling whether the var is set.
const a2aPeerFixture = `branding: {}
a2a_peers:
  - name: helpdesk
    rpc_url: "https://support.example.com/v1/a2a"
    description: "The helpdesk agent: password resets, access requests."
    headers:
      X-API-Key: "${HELPDESK_A2A_KEY}"
    critical: true
`

func TestA2APeersParse(t *testing.T) {
	t.Setenv("HELPDESK_A2A_KEY", "shh-secret")
	b, err := Load(writeManifest(t, a2aPeerFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.A2APeers) != 1 || b.A2APeers[0].Name != "helpdesk" || !b.A2APeers[0].Critical {
		t.Fatalf("A2APeers = %+v", b.A2APeers)
	}
	cfgs := b.A2APeerConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("A2APeerConfigs len = %d", len(cfgs))
	}
	if got := cfgs[0].Headers["X-API-Key"]; got != "shh-secret" {
		t.Errorf("resolved header = %q, want the host-side secret", got)
	}
	if cfgs[0].RPCURL != "https://support.example.com/v1/a2a" || !strings.Contains(cfgs[0].Description, "password resets") {
		t.Errorf("runtime shape: %+v", cfgs[0])
	}
}

func TestA2APeersEnvVarNamesSurvivesAllowlist(t *testing.T) {
	t.Setenv("HELPDESK_A2A_KEY", "")
	b, err := Load(writeManifest(t, a2aPeerFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !slices.Contains(b.EnvVarNames(), "HELPDESK_A2A_KEY") {
		t.Errorf("EnvVarNames = %v, want HELPDESK_A2A_KEY (deferred header credential)", b.EnvVarNames())
	}
	// Unresolved at load: the raw reference is kept for call-time resolution,
	// never a literal empty header.
	if got := b.A2APeers[0].Headers["X-API-Key"]; got != "${HELPDESK_A2A_KEY}" {
		t.Errorf("header at load = %q, want the raw reference kept", got)
	}
}

func TestA2APeersCriticalFold(t *testing.T) {
	b, err := Load(writeManifest(t, a2aPeerFixture))
	if err != nil {
		t.Fatal(err)
	}
	suffixes := b.AgentPolicy().CriticalToolSuffixes
	for _, want := range []string{"helpdesk_send", "helpdesk_cancel"} {
		if !slices.Contains(suffixes, want) {
			t.Errorf("CriticalToolSuffixes = %v, want %s", suffixes, want)
		}
	}
	for _, notWant := range []string{"helpdesk_status", "helpdesk_wait", "helpdesk"} {
		if slices.Contains(suffixes, notWant) {
			t.Errorf("read-only operation %s must not be critical: %v", notWant, suffixes)
		}
	}
}

func TestA2APeersValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad name charset", "a2a_peers:\n  - name: Help-Desk\n    rpc_url: https://x.example/a2a\n    description: d\n", "must match"},
		{"missing rpc_url", "a2a_peers:\n  - name: hd\n    description: d\n", "rpc_url is required"},
		{"file scheme", "a2a_peers:\n  - name: hd\n    rpc_url: file:///etc/passwd\n    description: d\n", "scheme must be http or https"},
		{"userinfo", "a2a_peers:\n  - name: hd\n    rpc_url: https://user:pw@x.example/a2a\n    description: d\n", "userinfo"},
		{"missing description", "a2a_peers:\n  - name: hd\n    rpc_url: https://x.example/a2a\n", "description is required"},
		{"duplicate", "a2a_peers:\n  - name: hd\n    rpc_url: https://x.example/a2a\n    description: d\n  - name: hd\n    rpc_url: https://y.example/a2a\n    description: d\n", "duplicate"},
		{"reserved mcp server name", "mcp_servers:\n  - name: _a2a\n    command: python3\n", "reserved for a2a_peers"},
		{"http_tool shadows derived name", "http_tools:\n  - name: hd_send\n    method: GET\n    url: https://x.example\na2a_peers:\n  - name: hd\n    rpc_url: https://x.example/a2a\n    description: d\n", "derived tool name"},
		{"unknown key fails strict decode", "a2a_peers:\n  - name: hd\n    rpc_url: https://x.example/a2a\n    description: d\n    agent_card: https://x.example/card\n", "agent_card"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, "branding: {}\n"+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestA2APeersScrubbed(t *testing.T) {
	t.Setenv("HELPDESK_A2A_KEY", "shh")
	b, err := Load(writeManifest(t, a2aPeerFixture))
	if err != nil {
		t.Fatal(err)
	}
	b.ScrubConnectorRuntimeDefinitions()
	if b.A2APeers != nil || b.A2APeerConfigs() != nil {
		t.Fatalf("scrub must drop the peer definitions: %+v", b.A2APeers)
	}
}

// TestDefaultBundleHasNoA2APeers asserts the generic bundle ships none, so the
// feature is opt-in and the default behavior is unchanged.
func TestDefaultBundleHasNoA2APeers(t *testing.T) {
	b, err := Load(filepath.Join(repoRoot(t), "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if len(b.A2APeers) != 0 || b.A2APeerConfigs() != nil {
		t.Errorf("default bundle must declare no a2a_peers (commented-out example only)")
	}
}
