package scheduledrun

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// A task set to send privately tags every MCP call of its runs; any other
// task tags none.
func TestPrivateEmailsRequestMeta(t *testing.T) {
	if got := privateEmailsRequestMeta(false); got != nil {
		t.Fatalf("not private: %v, want nil", got)
	}
	want := map[string]any{mcp.PrivateEmailsMetaKey: true}
	if got := privateEmailsRequestMeta(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("private: %v, want %v", got, want)
	}
	if mcp.PrivateEmailsMetaKey != "fleet/private_emails" {
		t.Fatalf("the key the bundle's sender reads changed: %q", mcp.PrivateEmailsMetaKey)
	}
}
