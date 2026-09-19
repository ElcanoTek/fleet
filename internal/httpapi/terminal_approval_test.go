package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func TestHandlerOnlyPatternArgumentsAreFrozenStrings(t *testing.T) {
	args := handlerApprovalPatternArgs("schedule_task", `{"name":"nightly","prompt":"full prompt","cron":"0 9 * * *","run_immediately":false,"max_iterations":8}`)
	if len(args) != 3 || args["prompt"] != "full prompt" || args["cron"] != "0 9 * * *" {
		t.Fatalf("wrong argument key space: %v", args)
	}
	email := handlerApprovalPatternArgs("preview_email", `{"to_email":"u@example.com","cc_emails":["c@example.com"]}`)
	if len(email) != 1 || email["to_email"] != "u@example.com" {
		t.Fatalf("email alias or non-string argument leaked into pattern matching: %v", email)
	}
	if got := handlerApprovalPatternArgs("mcp_demo_tool", `{"key":"not-client-matched"}`); got != nil {
		t.Fatalf("executable tool arguments exposed: %v", got)
	}
}

func TestHandlerOnlyScopePreservesIdempotentOutcome(t *testing.T) {
	for _, status := range []string{"pending", "approved", "rejected"} {
		t.Run(status, func(t *testing.T) {
			fake := &expiredClickFakeStore{approval: store.Approval{ID: "a", ConversationID: "c", UserEmail: "u@example.com", ToolName: "schedule_task", Status: status}}
			s := &Server{store: fake}
			req := httptest.NewRequest(http.MethodPost, "/conversations/c/approvals/a", strings.NewReader(`{"approved":true,"scope":"session"}`))
			req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
			rec := httptest.NewRecorder()
			s.handleApproval(rec, req, "c", "a")
			want := http.StatusOK
			if status == "pending" {
				want = http.StatusBadRequest
			}
			if rec.Code != want {
				t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
			}
			if status != "pending" && !strings.Contains(rec.Body.String(), `"status":"`+status+`"`) {
				t.Fatal("lost authoritative settled outcome", rec.Body.String())
			}
		})
	}
}
