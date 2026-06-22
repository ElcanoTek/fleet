package acpingress

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// TestIngressApproverMaterializesEmail proves the ACP ingress approver now runs
// the SAME host-side email materialization the web stager runs (issue #42): a
// staged send_email with a workspace content_file + a relative attachment is
// persisted with the file inlined and the path made absolute — byte-identical to
// feeding the same raw args through the shared tools.Materialize* helpers — so
// the post-approval replay carries resolvable args instead of bare filenames.
func TestIngressApproverMaterializesEmail(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-email-1"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "<html><body>quarterly report</body></html>"
	if err := os.WriteFile(filepath.Join(root, convID, "draft.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("write draft: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, convID, "chart.png"), []byte("PNGDATA"), 0o644); err != nil {
		t.Fatalf("write chart: %v", err)
	}

	st := newMemStore()
	ap := &ingressApprover{store: st, conversationID: convID, userEmail: "op@fleet.local"}

	raw := `{"to_email":"x@y.com","subject":"s","content_file":"draft.html","inline_attachments":[{"path":"chart.png","cid":"chart"}]}`
	id, err := ap.Stage("mcp_sendgrid_send_email", "tc-1", raw)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	appr := st.apps[id]
	if appr == nil {
		t.Fatal("approval not staged")
	}

	// Byte-identical to the web path: the same raw fed through the shared helpers.
	inlined, _ := tools.MaterializeContentFile(convID, raw)
	wantArgs, _ := tools.MaterializeAttachmentPaths(convID, inlined)
	if appr.ArgsJSON != wantArgs {
		t.Fatalf("staged ArgsJSON not materialized identically to the web path:\n got=%s\nwant=%s", appr.ArgsJSON, wantArgs)
	}

	// And concretely: content inlined, content_file gone, attachment path absolute.
	var got map[string]any
	if err := json.Unmarshal([]byte(appr.ArgsJSON), &got); err != nil {
		t.Fatalf("unmarshal staged args: %v", err)
	}
	if _, ok := got["content_file"]; ok {
		t.Error("content_file should be removed after materialization")
	}
	if got["content"] != body {
		t.Errorf("content = %v, want the inlined file bytes", got["content"])
	}
	gotPath := got["inline_attachments"].([]any)[0].(map[string]any)["path"].(string)
	if gotPath != filepath.Join(root, convID, "chart.png") {
		t.Errorf("attachment path = %q, want absolute under the workspace", gotPath)
	}
}

// TestIngressApproverEmailFailClosed: a missing content_file fails the Stage
// (fail-closed) and stages nothing — the policy surfaces it as a tool-call
// failure rather than sending the wrong bytes.
func TestIngressApproverEmailFailClosed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	st := newMemStore()
	ap := &ingressApprover{store: st, conversationID: "conv-missing", userEmail: "op@fleet.local"}

	raw := `{"to_email":"x@y.com","subject":"s","content_file":"nope.html"}`
	if _, err := ap.Stage("send_email", "tc-1", raw); err == nil {
		t.Fatal("expected Stage to fail-closed on a missing content_file")
	}
	if len(st.apps) != 0 {
		t.Fatalf("nothing should be staged on a fail-closed email; got %d approvals", len(st.apps))
	}
}

// TestIngressApproverNonEmailUnchanged: a non-email critical tool's args are
// persisted verbatim (no materialization path).
func TestIngressApproverNonEmailUnchanged(t *testing.T) {
	st := newMemStore()
	ap := &ingressApprover{store: st, conversationID: "conv-bash", userEmail: "op@fleet.local"}
	raw := `{"command":"git push origin main"}`
	id, err := ap.Stage("bash", "tc-1", raw)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if st.apps[id].ArgsJSON != raw {
		t.Errorf("non-email args should be verbatim; got %s", st.apps[id].ArgsJSON)
	}
}
