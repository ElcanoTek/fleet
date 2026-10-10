package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Grouped approvals (docs/GROUPED-APPROVALS.md).
//
// A bundle lists critical tools in agent_policy.critical_tool_group_approval.
// Every card the stager creates for such a tool records the staging turn's id
// as its group id (approvals.group_id), so the web can render the two or more
// cards one turn stages as ONE card: a checkbox per call, Approve all (the
// checked calls, the unchecked ones declined), One at a time (today's
// individual cards) and Cancel all.
//
// The group decision is one POST that names, per card, approve or decline.
// It adds no execution path: each card is decided by decideApproval, the same
// function behind the single-card POST, so each keeps its own claim (the only
// gate against a double run), its own budget and early "executing" reply, its
// own outcome write and history breadcrumb, and its own resume bookkeeping.
// A server endpoint was chosen over a client loop of single-card POSTs because
// a loop can be cut off half way (a closed tab, a dropped connection) and
// leave a person's one decision half applied: some calls approved, the
// unchecked ones never declined. Here the whole decision is checked before
// anything is claimed and then applied on the server, detached from the
// request.

// maxApprovalGroupDecisions bounds one group decision. A group is the cards
// one turn staged for opted-in tools, normally one per external system; the
// bound only keeps a pathological request from fanning out without limit.
const maxApprovalGroupDecisions = 100

// approvalGroupWorkers bounds how many of a group's cards are decided at
// once. An approved MCP call opens its own scope (a fresh server process), so
// the fan-out is bounded; a declined card settles at once.
const approvalGroupWorkers = 8

// approvalGroupRequest is the group decision body: the approval ids to
// approve and the ones to decline. A card of the group named in neither list
// is left as it is (it may have been staged after the person looked).
type approvalGroupRequest struct {
	Approve []string `json:"approve"`
	Decline []string `json:"decline"`
}

// approvalGroupResult is one card's answer inside the group reply: the body
// the single-card POST would have answered, or its error.
type approvalGroupResult struct {
	ApprovalID string `json:"approval_id"`
	// Decision is what was asked for this card: "approve" or "decline".
	Decision string `json:"decision"`
	// StatusCode is the HTTP status the single-card POST would have answered.
	StatusCode int `json:"status_code"`
	// Result is that POST's JSON body (status, result_text, is_err,
	// executing, execution_unknown, resume). Absent when Error is set.
	Result map[string]any `json:"result,omitempty"`
	// Error is that POST's error text; the card was not decided by this
	// request (it stays pending unless something else settled it).
	Error string `json:"error,omitempty"`
}

// withApprovalGroup adds the row's group id to a client payload when it has
// one. Only present for a card that joined a group, so every other card's
// payload is unchanged.
func withApprovalGroup(payload map[string]any, a *store.Approval) map[string]any {
	if a != nil && a.GroupID != "" {
		payload["group_id"] = a.GroupID
	}
	return payload
}

// joinApprovalGroup records the turn's group id on a card whose tool the
// bundle opted in. Best effort: a failed write leaves the card ungrouped, so
// it renders and resolves on its own exactly as before.
func (a *approvalStager) joinApprovalGroup(approval *store.Approval) {
	if a.groupID == "" || handlerOnlyApproval(approval.ToolName) || !agentcore.GroupsApproval(approval.ToolName) {
		return
	}
	ok, err := a.store.SetApprovalGroup(a.ctx, a.userEmail, approval.ID, a.groupID)
	if err != nil || !ok {
		log.Printf("approval group: store group for %s: ok=%t err=%v", approval.ID, ok, err)
		return
	}
	approval.GroupID = a.groupID
}

// handleApprovalGroup serves POST /conversations/{id}/approval-groups/{groupID}.
//
// Every named card must belong to the caller, to this conversation and to
// this group, and appear once; otherwise the request is refused and nothing
// is claimed. Then each card is decided as the single-card POST would decide
// it (scope once), and the reply lists every card's answer. The decisions run
// detached from the request, so a client that goes away mid-request does not
// leave the decision half applied.
func (s *Server) handleApprovalGroup(w http.ResponseWriter, r *http.Request, convID, groupID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	if strings.TrimSpace(groupID) == "" {
		http.Error(w, "approval group id required", http.StatusBadRequest)
		return
	}

	var req approvalGroupRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "bad json: trailing data", http.StatusBadRequest)
		return
	}

	type decision struct {
		id      string
		approve bool
	}
	decisions := make([]decision, 0, len(req.Approve)+len(req.Decline))
	seen := make(map[string]bool, len(req.Approve)+len(req.Decline))
	for _, list := range []struct {
		ids     []string
		approve bool
	}{{req.Approve, true}, {req.Decline, false}} {
		for _, id := range list.ids {
			if id == "" {
				http.Error(w, "empty approval id", http.StatusBadRequest)
				return
			}
			if seen[id] {
				http.Error(w, fmt.Sprintf("approval %q is named more than once", id), http.StatusBadRequest)
				return
			}
			seen[id] = true
			decisions = append(decisions, decision{id: id, approve: list.approve})
		}
	}
	if len(decisions) == 0 {
		http.Error(w, "name at least one approval to approve or decline", http.StatusBadRequest)
		return
	}
	if len(decisions) > maxApprovalGroupDecisions {
		http.Error(w, fmt.Sprintf("too many approvals in one group decision (max %d)", maxApprovalGroupDecisions), http.StatusBadRequest)
		return
	}

	// Check every card before deciding any: a decision that names a card of
	// another conversation or group is refused whole, so it can never be
	// half applied.
	for _, d := range decisions {
		a, err := s.store.GetApproval(r.Context(), user, d.id)
		if err != nil {
			http.Error(w, "could not load approval", http.StatusInternalServerError)
			return
		}
		if a == nil || a.ConversationID != convID {
			http.Error(w, fmt.Sprintf("approval %q not found in this conversation", d.id), http.StatusNotFound)
			return
		}
		if a.GroupID != groupID {
			http.Error(w, fmt.Sprintf("approval %q is not in this approval group", d.id), http.StatusConflict)
			return
		}
	}

	// Detached: once the decision is accepted it is applied whole, whatever
	// happens to the connection. The wait for a running call still ends when
	// the client goes away (decideApproval's done), and the call carries on.
	ctx := context.WithoutCancel(r.Context())
	results := make([]approvalGroupResult, len(decisions))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(approvalGroupWorkers, len(decisions)) {
		wg.Go(func() {
			for i := range jobs {
				d := decisions[i]
				res := approvalGroupResult{ApprovalID: d.id, Decision: "decline"}
				if d.approve {
					res.Decision = "approve"
				}
				out := s.decideApproval(ctx, r.Context().Done(), user, convID, d.id, approvalRequest{Approved: d.approve, Scope: "once"})
				switch {
				case out.suggest != nil:
					// Unreachable: a suggestion card is handler-only and never
					// joins a group. Refused rather than resolved off-shape.
					res.StatusCode, res.Error = http.StatusBadRequest, "this card cannot be decided in a group"
				case out.gone:
					res.StatusCode = http.StatusOK
				case out.status != 0:
					res.StatusCode, res.Error = out.status, out.errText
				default:
					res.StatusCode, res.Result = http.StatusOK, out.body
				}
				results[i] = res
			}
		})
	}
	for i := range decisions {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	resume := false
	for _, res := range results {
		if res.Result != nil && res.Result["resume"] == true {
			resume = true
		}
	}
	//nolint:gosec // G706: every interpolated value is rendered with %q, which escapes any CR/LF.
	log.Printf("audit: %q decided approval group %q in conversation %q (%d cards)", user, groupID, convID, len(decisions))
	out := map[string]any{"group_id": groupID, "results": results}
	if resume {
		// At least one card's settlement may start the automatic continue
		// (docs/RESUME-AFTER-APPROVAL.md): the client follows the
		// conversation for it, as after a single card.
		out["resume"] = true
	}
	writeJSON(w, out)
}
