package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"charm.land/fantasy"
)

// TestParseReviewResult covers the reviewer-verdict JSON parsing, including the
// fenced-code-block and surrounding-prose cases the model commonly emits and the
// blank-issue cleanup, mirroring parseVerifierResult's handling.
func TestParseReviewResult(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantRev   bool
		wantIssue []string
		wantErr   bool
	}{
		{
			name:    "clean pass",
			raw:     `{"needs_revision": false, "issues": [], "reasoning": "looks good"}`,
			wantRev: false,
		},
		{
			name:      "needs revision with issues",
			raw:       `{"needs_revision": true, "issues": ["recompute Q3 totals", "add the deadline"], "reasoning": "math is off"}`,
			wantRev:   true,
			wantIssue: []string{"recompute Q3 totals", "add the deadline"},
		},
		{
			name:      "fenced json with prose",
			raw:       "Here is my review:\n```json\n{\"needs_revision\": true, \"issues\": [\"fix it\"], \"reasoning\": \"x\"}\n```\nThanks!",
			wantRev:   true,
			wantIssue: []string{"fix it"},
		},
		{
			name:      "blank issues are dropped",
			raw:       `{"needs_revision": true, "issues": ["", "  ", "real issue"], "reasoning": "y"}`,
			wantRev:   true,
			wantIssue: []string{"real issue"},
		},
		{
			name:    "no json object",
			raw:     "I think it's fine, ship it.",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseReviewResult(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseReviewResult(%q) expected error, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReviewResult(%q) unexpected error: %v", tc.raw, err)
			}
			if got.NeedsRevision != tc.wantRev {
				t.Errorf("needs_revision = %v, want %v", got.NeedsRevision, tc.wantRev)
			}
			if len(got.Issues) != len(tc.wantIssue) {
				t.Fatalf("issues = %v, want %v", got.Issues, tc.wantIssue)
			}
			if len(tc.wantIssue) > 0 && !reflect.DeepEqual(got.Issues, tc.wantIssue) {
				t.Errorf("issues = %v, want %v", got.Issues, tc.wantIssue)
			}
		})
	}
}

// TestLatestAssistantText pins that the reviewer critiques the most recent
// non-empty assistant message (skipping later tool/user turns and empty ones).
func TestLatestAssistantText(t *testing.T) {
	if got := latestAssistantText(nil); got != "" {
		t.Fatalf("nil session should yield empty, got %q", got)
	}
	session := NewLogSession()
	toolID := "c1"
	session.Messages = []LogMessage{
		{Role: roleUser, Content: "do the thing"},
		{Role: roleAssistant, Content: "first draft"},
		{Role: roleAssistant, Content: "final answer"},
		{Role: roleAssistant, Content: "   "}, // blank, must be skipped
		{Role: roleTool, ToolCallID: &toolID, Content: "tool output"},
	}
	if got := latestAssistantText(session); got != "final answer" {
		t.Fatalf("latestAssistantText = %q, want %q", got, "final answer")
	}
}

// reviewMockModel drives the reviewer call through the SAME seam the real review
// uses: fantasy.NewAgent(model).Generate calls model.Generate, so a Generate
// stub fully exercises runPhoneAFriendReview without a real key or network.
type reviewMockModel struct {
	text   string
	genErr error
	calls  int
}

func (m *reviewMockModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	if m.genErr != nil {
		return nil, m.genErr
	}
	return &fantasy.Response{
		Content:      []fantasy.Content{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 10, OutputTokens: 5},
	}, nil
}

func (m *reviewMockModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *reviewMockModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *reviewMockModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}
func (m *reviewMockModel) Provider() string { return "mock" }
func (m *reviewMockModel) Model() string    { return "mock-reviewer" }

// TestRunPhoneAFriendReview exercises the end-to-end review call over the mock
// model seam: a "needs revision" verdict surfaces actionable issues, a clean
// verdict surfaces none, and a model error degrades to an error (which the
// caller turns into a fail-open skip).
func TestRunPhoneAFriendReview(t *testing.T) {
	a := &Agent{}
	records := []toolExecRecord{{Name: "run_python", Succeeded: true}}

	t.Run("needs revision returns issues", func(t *testing.T) {
		m := &reviewMockModel{text: `{"needs_revision": true, "issues": ["fix the totals"], "reasoning": "off by refunds"}`}
		issues, err := a.runPhoneAFriendReview(context.Background(), m, "compute Q3 totals", "the total is 100", records)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !reflect.DeepEqual(issues, []string{"fix the totals"}) {
			t.Fatalf("issues = %v, want [fix the totals]", issues)
		}
		if m.calls != 1 {
			t.Fatalf("reviewer should be called exactly once, got %d", m.calls)
		}
	})

	t.Run("clean verdict returns no issues", func(t *testing.T) {
		m := &reviewMockModel{text: `{"needs_revision": false, "issues": [], "reasoning": "good"}`}
		issues, err := a.runPhoneAFriendReview(context.Background(), m, "task", "answer", records)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(issues) != 0 {
			t.Fatalf("expected no issues, got %v", issues)
		}
	})

	t.Run("nil reviewer errors", func(t *testing.T) {
		if _, err := a.runPhoneAFriendReview(context.Background(), nil, "task", "answer", records); err == nil {
			t.Fatal("expected error with nil reviewer model")
		}
	})

	t.Run("model error degrades to error", func(t *testing.T) {
		m := &reviewMockModel{genErr: errors.New("boom")}
		if _, err := a.runPhoneAFriendReview(context.Background(), m, "task", "answer", records); err == nil {
			t.Fatal("expected error when the reviewer model fails")
		}
	})
}
