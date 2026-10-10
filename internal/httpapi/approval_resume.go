package httpapi

// Resume after approval (docs/RESUME-AFTER-APPROVAL.md, ADR-0083).
//
// An approval card resolves outside any turn, so its outcome lands in history
// and the model reads it only when the user types again. For a tool the bundle
// lists in agent_policy.critical_tool_resume, the chat server starts ONE new
// turn once the conversation's cards are settled, so the agent can verify what
// was approved and carry on.
//
// The pieces, and why each is where it is:
//
//   - Staging arms the card (approvalStager.Stage → ArmApprovalResume). The
//     intent is durable from the moment the card exists, so no settlement path
//     can forget it and a restart cannot lose it silently.
//   - Every settlement (approve with its outcome recorded, decline, expired
//     click, expiry sweep, a suggestion card) calls noteApprovalSettled, which
//     (re)arms a short per-conversation debounce. Several cards settled close
//     together (one per SSP group, say) therefore produce one turn, not one
//     each.
//   - When the debounce fires, store.ClaimApprovalResume decides in one
//     transaction: wait (another card is still pending or executing — its own
//     settlement re-kicks), skip with a note in the conversation (the hourly
//     cap, a full queue), or claim the settled armed cards and enqueue one
//     input-queue row of mode 'resume'. The claim and the enqueue commit
//     together, so a card resumes at most once.
//   - The row then drains through the ordinary queue (maybeDrainQueue →
//     launchQueuedTurn → startTurn → runTurnAsync → agentcore.Run): same
//     governance, persona, model, connector selection, seat, per-user
//     concurrency cap and ceilings as a turn the user typed, and when a turn
//     is already running it waits behind it rather than running beside it.
//     Its input is marked as fleet's (agent.InputKindApprovalResume), so it
//     is persisted and rendered as a notice, not as the user's words.
//
// A resume never approves anything: the new turn's critical calls stage new
// cards like any turn's.

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/store"
)

// approvalResumeDebounce is how long the server waits after the last
// settlement in a conversation before it decides on a resume. A var so tests
// can shorten it.
var approvalResumeDebounce = 2 * time.Second

// approvalResumeRetryDelay / approvalResumeRetries bound the retry of a claim
// that failed on a store error. The cards stay armed throughout, so a retry
// that runs out leaves them for the next settlement in the conversation, or for
// the boot sweep, which drops them with a note.
var approvalResumeRetryDelay = 5 * time.Second

const approvalResumeRetries = 3

// approvalResumeScheduler holds the per-conversation debounce. The zero value
// is ready (tests build Server as a struct literal). gen is process-wide, so a
// timer superseded by a later kick can never match a future generation even
// after the conversation's entry is deleted.
type approvalResumeScheduler struct {
	mu     sync.Mutex
	gen    uint64
	latest map[string]uint64
}

// kick records a new settlement and returns its generation.
func (r *approvalResumeScheduler) kick(convID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest == nil {
		r.latest = make(map[string]uint64)
	}
	r.gen++
	r.latest[convID] = r.gen
	return r.gen
}

// take reports whether gen is still the conversation's latest kick, and if so
// clears it: exactly one timer of a burst decides.
func (r *approvalResumeScheduler) take(convID string, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latest[convID] != gen {
		return false
	}
	delete(r.latest, convID)
	return true
}

// noteApprovalSettled is called after ANY approval in convID reaches a
// terminal, recorded outcome. A settled card that did not opt in still kicks:
// it may be the card a deferred resume was waiting for. With no tool opted in
// it does nothing at all, so a bundle without critical_tool_resume behaves
// exactly as before — except for a card that was armed under an earlier
// policy (the bundle dropped its tool across a restart), which still settles
// into a decision rather than staying armed for good.
func (s *Server) noteApprovalSettled(convID string, approval *store.Approval) {
	armed := approval != nil && approval.ResumeState == store.ApprovalResumeArmed
	if convID == "" || s.store == nil || (!armed && !agentcore.ResumeAfterApprovalEnabled()) {
		return
	}
	s.scheduleApprovalResume(convID, approvalResumeDebounce, 0)
}

func (s *Server) scheduleApprovalResume(convID string, after time.Duration, attempt int) {
	gen := s.approvalResumes.kick(convID)
	s.background.After("httpapi.approval_resume", after, func() {
		if !s.approvalResumes.take(convID, gen) {
			return // a later settlement re-armed the debounce; its timer decides
		}
		s.decideApprovalResume(convID, attempt)
	})
}

// decideApprovalResume runs the claim and acts on what it decided.
func (s *Server) decideApprovalResume(convID string, attempt int) {
	if s.shuttingDown.Load() {
		// No new turns while draining. The cards stay armed; the next boot
		// drops them with a note (DropApprovalResumesAtBoot), never runs them.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	maxPerHour := agentcore.ResumeMaxPerHour()
	res, err := s.store.ClaimApprovalResume(ctx, store.ApprovalResumeRequest{
		ConversationID: convID,
		MaxPerHour:     maxPerHour,
		MaxPending:     maxPendingInputs,
		IgnoreOutstanding: func(id string) bool {
			// A card whose executing sentinel stands only because this
			// process failed to write its outcome is not still running.
			_, failed := s.approvalPersistenceFailures.Load(id)
			return failed
		},
	})
	if s.approvalResumeObserver != nil {
		defer s.approvalResumeObserver(convID, res, err)
	}
	if err != nil {
		log.Printf("approval resume (conv=%s): claim failed: %s", logSafe(convID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF; convID is a server-generated UUID.
		if attempt < approvalResumeRetries {
			s.scheduleApprovalResume(convID, approvalResumeRetryDelay, attempt+1)
		}
		return
	}
	switch {
	case len(res.Outstanding) > 0:
		// Another card is still pending or executing; its settlement kicks
		// again. Nothing was claimed.
		return
	case len(res.Claimed) == 0:
		return
	case res.Input == nil:
		metrics.RecordApprovalResume(res.Skipped)
		//nolint:gosec // G706: every value is %q-quoted (CR/LF escaped) and server-generated.
		log.Printf("audit: approval resume skipped for conversation %q (%s; %d in the last hour, cap %d) after %q",
			convID, res.Skipped, res.RecentResumes, maxPerHour, approvalResumeIDs(res.Claimed))
		return
	}
	metrics.RecordApprovalResume("queued")
	//nolint:gosec // G706: every value is %q-quoted (CR/LF escaped) and server-generated.
	log.Printf("audit: approval resume queued for conversation %q as input %q after %q",
		convID, res.Input.ID, approvalResumeIDs(res.Claimed))
	s.emitQueueUpdate(ctx, res.Input.UserEmail, convID)
	// Idle → the drain launches it now; a running turn → it waits in the
	// queue and that turn's completion tail drains it.
	s.maybeDrainQueue(convID)
}

func approvalResumeIDs(items []store.ApprovalResumeItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ToolName+"/"+it.ID+"="+it.Outcome)
	}
	return out
}

// armApprovalResume arms a freshly staged card of an opted-in tool. Best
// effort: a failed write leaves the card unarmed, which is exactly the
// behaviour without the feature (no turn starts on its own).
func (a *approvalStager) armApprovalResume(approval *store.Approval) {
	if approval == nil || !agentcore.ResumeAfterApproval(approval.ToolName) || handlerOnlyApproval(approval.ToolName) {
		return
	}
	ok, err := a.store.ArmApprovalResume(a.ctx, a.userEmail, approval.ID)
	if err != nil || !ok {
		log.Printf("approval resume: arm %s: ok=%t err=%v", approval.ID, ok, err)
		return
	}
	approval.ResumeState = store.ApprovalResumeArmed
}

// resumeReplyFlag is approvalResumeReplyFlag plus the case where this card did
// not opt in but its settlement may release a resume that was waiting on it:
// another card of the conversation is still armed.
func (s *Server) resumeReplyFlag(ctx context.Context, out map[string]any, approval *store.Approval) map[string]any {
	out = approvalResumeReplyFlag(out, approval)
	if out["resume"] == true || approval == nil || s.store == nil || !agentcore.ResumeAfterApprovalEnabled() {
		return out
	}
	if armed, err := s.store.HasArmedApprovals(ctx, approval.ConversationID); err == nil && armed {
		out["resume"] = true
	}
	return out
}

// approvalResumeReplyFlag adds "resume": true to an approval POST answer when
// the card is armed, or already claimed into a resume, so the client knows to
// look for the turn fleet is about to start, has started, or (while the call
// is still executing) will start once it lands. Idempotent replays (a lost
// answer, a second tab, Check result) carry it too: by then the card is often
// already claimed.
func approvalResumeReplyFlag(out map[string]any, approval *store.Approval) map[string]any {
	if approval != nil && (approval.ResumeState == store.ApprovalResumeArmed || approval.ResumeState == store.ApprovalResumeClaimed) {
		out["resume"] = true
	}
	return out
}

// resumeRowFromEarlierProcess reports a 'resume' queue row created before
// this process started.
func (s *Server) resumeRowFromEarlierProcess(row *store.InputQueueRow) bool {
	return row != nil && row.Mode == store.InputModeResume &&
		!s.processStart.IsZero() && row.CreatedAt <= s.processStart.Unix()
}

// noteDroppedResume records, in the conversation, a resume that the launch
// guard refused because it predates this process.
func (s *Server) noteDroppedResume(convID, rowID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.AppendApprovalResumeDroppedNotice(ctx, convID); err != nil {
		log.Printf("approval resume (conv=%s): note the dropped resume %s: %v", logSafe(convID), logSafe(rowID), err) //nolint:gosec // G706: logSafe strips CR/LF; both ids are server-generated.
	}
	//nolint:gosec // G706: every value is %q-quoted (CR/LF escaped) and server-generated.
	log.Printf("audit: approval resume %q in conversation %q predates this process; cancelled, not run", rowID, convID)
}
