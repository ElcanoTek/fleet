package clientconfig

import (
	"reflect"
	"strings"
	"testing"
)

// TestProbePropagationAndValidation pins the manifest probe: contract — the
// declared canary call for `fleet mcp test --deep`: a valid block flows into
// MCPServerConfigs verbatim, and the fail-loud validations reject a blank
// tool and a tool outside the server's tools: allowlist (the probe must never
// exercise a call the runtime itself would not expose).
func TestProbePropagationAndValidation(t *testing.T) {
	t.Run("propagates to runtime config", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: mail
    command: python3
    args: ["mcp/mail.py"]
    always: true
    tools: ["list_messages", "get_message"]
    probe:
      tool: list_messages
      args: {maxResults: 1}
      contains: messages
`)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		p := b.MCPServerConfigs()["mail"].Probe
		if p == nil {
			t.Fatal("Probe = nil, want the declared probe carried through")
		}
		if p.Tool != "list_messages" || p.Contains != "messages" {
			t.Errorf("probe = %+v", p)
		}
		if !reflect.DeepEqual(p.Args, map[string]interface{}{"maxResults": uint64(1)}) &&
			!reflect.DeepEqual(p.Args, map[string]interface{}{"maxResults": 1}) {
			t.Errorf("probe args = %#v, want maxResults=1", p.Args)
		}
	})

	t.Run("absent probe stays nil", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: mail
    command: python3
    args: ["mcp/mail.py"]
    always: true
`)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if p := b.MCPServerConfigs()["mail"].Probe; p != nil {
			t.Errorf("Probe = %+v, want nil when undeclared", p)
		}
	})

	t.Run("blank tool rejected", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: mail
    command: python3
    args: ["mcp/mail.py"]
    always: true
    probe:
      tool: ""
`)
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "probe.tool is required") {
			t.Fatalf("blank probe.tool must fail the load, got: %v", err)
		}
	})

	t.Run("tool outside allowlist rejected", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: mail
    command: python3
    args: ["mcp/mail.py"]
    always: true
    tools: ["get_message"]
    probe:
      tool: delete_message
`)
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "delete_message") {
			t.Fatalf("out-of-allowlist probe.tool must fail the load naming the tool, got: %v", err)
		}
	})
}
