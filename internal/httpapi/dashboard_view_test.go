package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// A turn sent from dashboard view carries one note telling the agent to keep
// the board's panels current; every other view value adds nothing, so a
// client that sends an unknown view cannot steer the model.
func TestAppendDashboardViewBlock(t *testing.T) {
	got := appendDashboardViewBlock("prior", chatViewDashboard)
	if !strings.HasPrefix(got, "prior\n\n[Dashboard view:") || !strings.Contains(got, `"panel" key`) {
		t.Fatalf("dashboard block = %q", got)
	}
	for _, view := range []string{"", "Dashboard", "chat", "dashboard "} {
		if out := appendDashboardViewBlock("prior", view); out != "prior" {
			t.Errorf("view %q added %q", view, out)
		}
	}
}

// The web client sends the view as "view" on POST /chat.
func TestChatRequestDecodesView(t *testing.T) {
	var req chatRequest
	if err := json.Unmarshal([]byte(`{"message":"hi","view":"dashboard"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.View != chatViewDashboard {
		t.Fatalf("view = %q", req.View)
	}
}
