package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func TestApprovalOutcomeFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a    store.Approval
		want map[string]any
	}{
		{
			name: "rejected has no execution flags",
			a:    store.Approval{Status: "rejected", ResultText: "User declined this action."},
			want: nil,
		},
		{
			name: "pending has no execution flags",
			a:    store.Approval{Status: "pending"},
			want: nil,
		},
		{
			name: "failed execution echoes is_err true",
			a: store.Approval{
				Status:     "approved",
				ResultText: "send failed: boom",
				IsErr:      sql.NullBool{Valid: true, Bool: true},
			},
			want: map[string]any{"is_err": true},
		},
		{
			name: "successful execution echoes is_err false",
			a: store.Approval{
				Status:     "approved",
				ResultText: `{"status_code":202}`,
				IsErr:      sql.NullBool{Valid: true, Bool: false},
			},
			want: map[string]any{"is_err": false},
		},
		{
			name: "in-flight claim sentinel is executing, not success",
			a: store.Approval{
				Status:     "approved",
				ResultText: approvalExecutingSentinel,
			},
			want: map[string]any{"executing": true},
		},
		{
			name: "legacy approved row without is_err is execution_unknown",
			a: store.Approval{
				Status:     "approved",
				ResultText: `{"status_code":202}`,
			},
			want: map[string]any{"execution_unknown": true},
		},
		{
			name: "known is_err wins over sentinel text",
			a: store.Approval{
				Status:     "approved",
				ResultText: approvalExecutingSentinel,
				IsErr:      sql.NullBool{Valid: true, Bool: false},
			},
			want: map[string]any{"is_err": false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := approvalOutcomeFlags(&tc.a)
			if len(got) != len(tc.want) {
				t.Fatalf("flags = %#v, want %#v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("flags[%q] = %#v, want %#v", k, got[k], v)
				}
			}
		})
	}
}

func TestHandleApproval_IdempotentEchoesExecutionOutcome(t *testing.T) {
	cases := []struct {
		name       string
		approval   store.Approval
		wantErr    *bool
		executing  bool
		unknown    bool
		wantStatus string
	}{
		{
			name: "failed tool retry includes is_err",
			approval: store.Approval{
				ID: "a", ConversationID: "c", UserEmail: "u@example.com",
				ToolName: "mcp_pages_deploy_page", Status: "approved",
				ResultText: "action failed: boom",
				IsErr:      sql.NullBool{Valid: true, Bool: true},
			},
			wantErr:    boolPtr(true),
			wantStatus: "approved",
		},
		{
			name: "successful tool retry includes is_err false",
			approval: store.Approval{
				ID: "a", ConversationID: "c", UserEmail: "u@example.com",
				ToolName: "mcp_pages_deploy_page", Status: "approved",
				ResultText: "ok",
				IsErr:      sql.NullBool{Valid: true, Bool: false},
			},
			wantErr:    boolPtr(false),
			wantStatus: "approved",
		},
		{
			name: "in-flight sentinel is executing",
			approval: store.Approval{
				ID: "a", ConversationID: "c", UserEmail: "u@example.com",
				ToolName: "mcp_pages_deploy_page", Status: "approved",
				ResultText: approvalExecutingSentinel,
			},
			executing:  true,
			wantStatus: "approved",
		},
		{
			name: "legacy approved row is execution_unknown",
			approval: store.Approval{
				ID: "a", ConversationID: "c", UserEmail: "u@example.com",
				ToolName: "mcp_pages_deploy_page", Status: "approved",
				ResultText: "ok",
			},
			unknown:    true,
			wantStatus: "approved",
		},
		{
			name: "rejected has no execution flags",
			approval: store.Approval{
				ID: "a", ConversationID: "c", UserEmail: "u@example.com",
				ToolName: "mcp_pages_deploy_page", Status: "rejected",
				ResultText: "User declined this action.",
			},
			wantStatus: "rejected",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &expiredClickFakeStore{approval: tc.approval}
			s := &Server{store: fake}
			req := httptest.NewRequest(http.MethodPost, "/conversations/c/approvals/a",
				strings.NewReader(`{"approved":true}`))
			req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@example.com"))
			rec := httptest.NewRecorder()
			s.handleApproval(rec, req, "c", "a")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Status           string `json:"status"`
				ResultText       string `json:"result_text"`
				IsErr            *bool  `json:"is_err"`
				Executing        bool   `json:"executing"`
				ExecutionUnknown bool   `json:"execution_unknown"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", resp.Status, tc.wantStatus)
			}
			if tc.wantErr != nil {
				if resp.IsErr == nil || *resp.IsErr != *tc.wantErr {
					t.Errorf("is_err = %v, want %v", resp.IsErr, *tc.wantErr)
				}
			} else if resp.IsErr != nil {
				t.Errorf("is_err = %v, want omitted", *resp.IsErr)
			}
			if resp.Executing != tc.executing {
				t.Errorf("executing = %v, want %v", resp.Executing, tc.executing)
			}
			if resp.ExecutionUnknown != tc.unknown {
				t.Errorf("execution_unknown = %v, want %v", resp.ExecutionUnknown, tc.unknown)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }
