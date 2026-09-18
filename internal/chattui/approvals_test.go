package chattui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClientDefaultModel pins the /client-config adoption path: the workspace
// default slug is parsed out of the same envelope the web picker reads, with
// the shared-secret + identity headers attached.
func TestClientDefaultModel(t *testing.T) {
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		if r.URL.Path != "/client-config" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"branding":{},"models":{"default_model":"acme/frontier-1","advanced_model":"acme/frontier-1-pro"}}`)
	}))
	defer srv.Close()

	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"})
	slug, err := c.DefaultModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if slug != "acme/frontier-1" {
		t.Errorf("slug = %q, want acme/frontier-1", slug)
	}
	if hdr.Get("X-Chat-Server-Token") != "sekret" || hdr.Get("X-User-Email") != "u@x.co" {
		t.Errorf("auth headers not sent: %v", hdr)
	}
}

func TestClientDefaultModelErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"})
	if _, err := c.DefaultModel(context.Background()); err == nil {
		t.Fatal("want error on 401")
	}
}

// TestTurnModelPrecedence pins the model-selection contract: an explicit
// --model always wins; the adopted workspace default rides ONLY new
// conversations so a resumed thread keeps its stored model.
func TestTurnModelPrecedence(t *testing.T) {
	c := NewClient(Config{Model: "explicit/model"})
	c.AdoptDefaultModel("workspace/default")
	if got := c.turnModel(""); got != "explicit/model" {
		t.Errorf("explicit+new = %q", got)
	}
	if got := c.turnModel("conv-1"); got != "explicit/model" {
		t.Errorf("explicit+resume = %q", got)
	}

	c = NewClient(Config{})
	c.AdoptDefaultModel("workspace/default")
	if got := c.turnModel(""); got != "workspace/default" {
		t.Errorf("default+new = %q, want workspace/default", got)
	}
	if got := c.turnModel("conv-1"); got != "" {
		t.Errorf("default+resume = %q, want empty (keep stored model)", got)
	}
	if got := c.EffectiveModel(); got != "workspace/default" {
		t.Errorf("EffectiveModel = %q", got)
	}
}

// TestClientResolveApproval pins the wire call the web card's buttons make:
// POST /conversations/{id}/approvals/{approvalId} with {"approved": bool}, and
// the status/result_text response mapping.
func TestClientResolveApproval(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"status":"approved","result_text":"Scheduled task created.\nTask id: abc-123"}`)
	}))
	defer srv.Close()

	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"})
	status, resultText, err := c.ResolveApproval(context.Background(), "conv-9", "appr-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/conversations/conv-9/approvals/appr-1" {
		t.Errorf("%s %s — wrong endpoint", gotMethod, gotPath)
	}
	if gotBody["approved"] != true {
		t.Errorf("body = %v, want approved:true", gotBody)
	}
	if status != "approved" || !strings.Contains(resultText, "abc-123") {
		t.Errorf("status=%q result=%q", status, resultText)
	}
}

func TestClientResolveApprovalErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "approval not found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "super-secret-token"})
	_, _, err := c.ResolveApproval(context.Background(), "conv-9", "appr-1", true)
	if err == nil {
		t.Fatal("want error on 404")
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Errorf("error must NOT leak the token: %v", err)
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "approval not found") {
		t.Errorf("error should carry status + body excerpt: %v", err)
	}
}

// TestRunOneShotSurfacesApprovals: a staged critical tool must be VISIBLE in
// one-shot mode, with the exact follow-up command to settle it — otherwise a
// script sees a successful turn whose real action silently never happened.
func TestRunOneShotSurfacesApprovals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(s string) { _, _ = io.WriteString(w, s); fl.Flush() }
		write("event: conversation\ndata: {\"id\":\"conv-77\"}\n\n")
		write("event: tool.call\ndata: {\"name\":\"schedule_task\",\"id\":\"c1\"}\n\n")
		write("event: tool.approval_required\ndata: {\"approval_id\":\"appr-5\",\"tool\":\"schedule_task\",\"summary\":{\"tool\":\"schedule_task\",\"name\":\"nightly\",\"prompt_preview\":\"do the thing\",\"run_immediately\":true,\"recurring\":false}}\n\n")
		write("event: text.delta\ndata: {\"text\":\"staged for your approval\"}\n\n")
		write("event: turn.completed\ndata: {}\n\n")
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := runOneShot(NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"}), "", "schedule it", strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errOut.String())
	}
	for _, want := range []string{
		"approval required: schedule_task",
		"appr-5",
		"--conversation conv-77 --approve appr-5",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut.String())
		}
	}
}

// TestRunResolveApprovalCLI drives the --approve/--deny one-shot path.
func TestRunResolveApprovalCLI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"approved","result_text":"Email sent."}`)
	}))
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "u@x.co", Token: "sekret"})

	var out, errOut bytes.Buffer
	if code := runResolveApproval(c, "conv-1", "appr-1", true, &out, &errOut); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "approved") || !strings.Contains(out.String(), "Email sent.") {
		t.Errorf("stdout = %q", out.String())
	}

	// Missing --conversation is a usage error (exit 2), never a server call.
	out.Reset()
	errOut.Reset()
	if code := runResolveApproval(c, "", "appr-1", true, &out, &errOut); code != 2 {
		t.Errorf("exit = %d, want 2 for missing conversation", code)
	}
}

// TestApprovalSummaryLine pins the one-line renderings for the tailored tools
// and the generic fallback.
func TestApprovalSummaryLine(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		summary map[string]any
		want    string
	}{
		{
			name:    "schedule_task immediate",
			tool:    "schedule_task",
			summary: map[string]any{"name": "nightly sync", "prompt_preview": "sync the things", "run_immediately": true},
			want:    `"nightly sync" — runs as soon as a worker is free — sync the things`,
		},
		{
			name:    "schedule_task cron",
			tool:    "schedule_task",
			summary: map[string]any{"name": "weekday report", "cron": "0 9 * * MON-FRI", "runs_per_month": float64(22)},
			want:    `"weekday report" — cron 0 9 * * MON-FRI (≈22 runs/month)`,
		},
		{
			name:    "send_email",
			tool:    "send_email",
			summary: map[string]any{"to": "crew@x.co", "subject": "weekly numbers"},
			want:    `to crew@x.co — "weekly numbers"`,
		},
		{
			name:    "bash",
			tool:    "bash",
			summary: map[string]any{"command": "rm -rf /tmp/scratch"},
			want:    "rm -rf /tmp/scratch",
		},
		{
			name:    "suggest_advanced_model",
			tool:    "suggest_advanced_model",
			summary: map[string]any{"recommend_model": "acme/frontier-1-pro"},
			want:    "switch to acme/frontier-1-pro",
		},
		{
			name:    "generic sorted kv",
			tool:    "mcp_pages_deploy",
			summary: map[string]any{"tool": "mcp_pages_deploy", "zeta": "last", "alpha": "first"},
			want:    "alpha=first tool=mcp_pages_deploy zeta=last",
		},
		{
			name:    "nil summary",
			tool:    "whatever",
			summary: nil,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var summary any
			if tt.summary != nil {
				summary = tt.summary
			}
			if got := approvalSummaryLine(tt.tool, summary); got != tt.want {
				t.Errorf("approvalSummaryLine = %q, want %q", got, tt.want)
			}
		})
	}
}
