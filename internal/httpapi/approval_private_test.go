package httpapi

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// These tests cover an outbound email card's Private box (approval_private.go):
// the person's choice is written with the claim and sent with the call as the
// request `_meta` fleet/private_emails, so it wins over the model's argument.

const privateStagedArgs = `{"to_email":"alice@client.example","subject":"Weekly numbers","content":"<p>hi</p>","private":true}`

// privateStore is narrowStore plus the Private claim.
type privateStore struct {
	narrowStore
	privateClaims int
}

func (p *privateStore) ClaimApprovalPrivate(_ context.Context, _, _, text string, private bool) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.approval.Status != "pending" {
		return false, nil
	}
	p.privateClaims++
	p.approval.Status, p.approval.ResultText, p.approval.PrivateChoice = "approved", text, &private
	return true, nil
}

func privateEmailRow(tool string) store.Approval {
	return store.Approval{
		ID: "ap1", ConversationID: "c1", UserEmail: "u@example.com",
		ToolName: tool, ArgsJSON: privateStagedArgs, Status: "pending",
	}
}

func privateServer(t *testing.T, st chatStore, child *narrowChild, server, tool string) *Server {
	t.Helper()
	s := narrowServer(t, st, child)
	s.agent.(*approvalEngine).catalog = []mcp.ServerTool{{ServerName: server, Tool: mcp.Tool{Name: tool}}}
	return s
}

// Ticked or unticked, the choice is claimed with the card and reaches the
// sender as `_meta`; the staged arguments are sent unchanged.
func TestHandleApproval_PrivateChoiceReachesTheSender(t *testing.T) {
	for _, private := range []bool{true, false} {
		st := &privateStore{narrowStore: narrowStore{approval: privateEmailRow("mcp_ses_outbound_send_email")}}
		child := &narrowChild{}
		s := privateServer(t, st, child, "ses_outbound", "send_email")

		body := `{"approved":true,"scope":"once","private":` + map[bool]string{true: "true", false: "false"}[private] + `}`
		rec := postNarrowApproval(t, s, body)
		if rec.Code != 200 {
			t.Fatalf("private=%v: POST = %d %s", private, rec.Code, rec.Body.String())
		}
		if st.privateClaims != 1 || st.wholeClaims != 0 {
			t.Fatalf("private=%v: claims private %d whole %d, want one private claim", private, st.privateClaims, st.wholeClaims)
		}
		if want := map[string]any{mcp.PrivateEmailsMetaKey: private}; !reflect.DeepEqual(child.meta, want) {
			t.Fatalf("private=%v: _meta = %#v, want %#v", private, child.meta, want)
		}
		if child.args["private"] != true || child.args["to_email"] != "alice@client.example" {
			t.Fatalf("private=%v: args changed: %v", private, child.args)
		}
		var reply map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &reply)
		if reply["private_choice"] != private {
			t.Fatalf("private=%v: reply private_choice = %v", private, reply["private_choice"])
		}
	}
}

// No choice (the CLI, an older client): the plain claim, no `_meta`, so the
// sender follows the model's own argument.
func TestHandleApproval_NoPrivateChoiceKeepsTheArgument(t *testing.T) {
	st := &privateStore{narrowStore: narrowStore{approval: privateEmailRow("mcp_ses_outbound_send_email")}}
	child := &narrowChild{}
	s := privateServer(t, st, child, "ses_outbound", "send_email")
	rec := postNarrowApproval(t, s, `{"approved":true,"scope":"once"}`)
	if rec.Code != 200 {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body.String())
	}
	if st.wholeClaims != 1 || st.privateClaims != 0 || child.meta != nil || st.approval.PrivateChoice != nil {
		t.Fatalf("claims whole %d private %d meta %v choice %v", st.wholeClaims, st.privateClaims, child.meta, st.approval.PrivateChoice)
	}
}

// A choice the card could not have offered is refused before a claim: the
// card stays pending and nothing runs.
func TestHandleApproval_RefusesAPrivateChoiceOffAnEmailCard(t *testing.T) {
	cases := []struct{ name, tool, server, short, body, want string }{
		{"with a decline", "mcp_ses_outbound_send_email", "ses_outbound", "send_email", `{"approved":false,"private":true}`, "private goes with an approval"},
		{"not an email tool", "mcp_ix_deploy_page", "ix", "deploy_page", `{"approved":true,"private":true}`, "only to an outbound email card"},
		{"a template email card", "mcp_ses_outbound_send_template_email", "ses_outbound", "send_template_email", `{"approved":true,"private":false}`, "only to an outbound email card"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			narrowPolicy(t)
			st := &privateStore{narrowStore: narrowStore{approval: privateEmailRow(tc.tool)}}
			child := &narrowChild{}
			s := privateServer(t, st, child, tc.server, tc.short)
			rec := postNarrowApproval(t, s, tc.body)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("POST = %d %q, want 400 with %q", rec.Code, rec.Body.String(), tc.want)
			}
			if st.approval.Status != "pending" || child.tool != "" {
				t.Fatalf("status %q, call %q: want the card pending and nothing run", st.approval.Status, child.tool)
			}
		})
	}
}

// The card offers the box on outbound email tools only, ticked when the
// model passed private.
func TestApprovalClientFields_PrivateOption(t *testing.T) {
	email := approvalClientFields("mcp_ses_outbound_send_email", privateStagedArgs, "")
	if email["private_option"] != true {
		t.Fatalf("email card private_option = %v", email["private_option"])
	}
	if summary, _ := email["summary"].(map[string]any); summary["private"] != true {
		t.Fatalf("email summary private = %v", summary["private"])
	}
	plain := approvalClientFields("mcp_ses_outbound_send_email", `{"to_email":"a@b.example","subject":"s","content":"c"}`, "")
	if summary, _ := plain["summary"].(map[string]any); summary["private"] != nil {
		t.Fatalf("unprivate email summary private = %v", summary["private"])
	}
	for _, tool := range []string{"mcp_ix_deploy_page", "mcp_ses_outbound_send_template_email", "preview_email"} {
		if other := approvalClientFields(tool, `{}`, ""); other["private_option"] != nil {
			t.Fatalf("%s card private_option = %v, want none: it renders no Private box", tool, other["private_option"])
		}
	}
}
