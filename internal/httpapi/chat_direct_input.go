// The direct-input claim lifecycle of POST /chat (#785): replaying an
// already-accepted input_id, claiming a direct submission's idempotency row,
// binding it (or a drained queue row) to its turn, and releasing or settling it
// when the turn never launches or ends. Split out of chat.go.

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/google/uuid"
)

// replayAcceptedInput answers a resend of an input_id that was already
// ACCEPTED (queued, running, or terminal) with that input's acknowledgement,
// so it never runs a duplicate turn — even when the retry lands after the
// conversation went idle (#785). A caller declaring user-unique keys
// (input_id_scope "user") is looked up per user, so a resend finds its input
// whichever conversation accepted it; otherwise keys are conversation-scoped.
// A first submission (no conversation yet) is recoverFirstSubmission's.
// handled reports that the response was written.
func (s *Server) replayAcceptedInput(w http.ResponseWriter, r *http.Request, user, convID string, req chatRequest) (handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" {
		return false
	}
	var existing *store.InputQueueRow
	var err error
	if strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		existing, err = s.lookupUserScopedInput(r.Context(), user, clientID)
	} else {
		existing, err = s.store.LookupInput(r.Context(), convID, clientID)
	}
	if err != nil {
		// Fail closed: proceeding without the lookup could run (and bill) a
		// duplicate turn for an input_id that was already accepted. A 500
		// lets the client retry the same input_id safely.
		http.Error(w, "input lookup failed: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	if existing == nil {
		return false
	}
	if existing.UserEmail != "" && !strings.EqualFold(existing.UserEmail, user) {
		http.Error(w, "input_id belongs to another user in this conversation", http.StatusConflict)
		return true
	}
	s.writeQueueAck(w, r, http.StatusOK, existing.ConversationID, *existing)
	return true
}

func (s *Server) lookupUserScopedInput(ctx context.Context, user, clientID string) (*store.InputQueueRow, error) {
	existing, err := s.store.LookupInputForUser(ctx, user, clientID)
	if err != nil || existing != nil {
		return existing, err
	}
	ext := externalClientInputID(clientID)
	if ext != clientID {
		existing, err = s.store.LookupInputForUser(ctx, user, ext)
		if err != nil || existing != nil {
			return existing, err
		}
	}
	if pref := effectiveMultiplayerInputID(user, ext); pref != clientID {
		return s.store.LookupInputForUser(ctx, user, pref)
	}
	return nil, nil
}

// lockUserScopedKey takes the per-(user, key) lock for a submission that
// declares user-unique keys and returns its (idempotent) unlock; for any
// other submission it locks nothing. The lock is in-process: fleet's control
// plane is single-replica by design (the inflight registry the Stop gate
// relies on is too), and it holds no database connection while the
// protected work asks the pool for one.
func (s *Server) lockUserScopedKey(user string, req chatRequest) (unlock func()) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" || !strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		return func() {}
	}
	return sync.OnceFunc(s.inputKeyLocks.lock(user + "\x00" + externalClientInputID(clientID)))
}

// recoverFirstSubmission answers a first submission (no conversation yet)
// whose user-unique key (input_id_scope "user") was already accepted, with
// that input's acknowledgement. It takes the key's lock first, held until
// the key is claimed (the returned unlock; a no-op when nothing was
// locked), so a concurrent resend waits for the first request's claim
// instead of both creating a conversation. The lock is in-process: fleet's
// control plane is single-replica by design (the inflight registry the Stop
// gate relies on is too), and an in-process lock holds no database
// connection while the protected work asks the pool for one. handled
// reports that the response was written.
func (s *Server) recoverFirstSubmission(w http.ResponseWriter, r *http.Request, user string, req chatRequest) (unlock func(), handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" || !strings.EqualFold(strings.TrimSpace(req.InputIDScope), "user") {
		return func() {}, false
	}
	unlock = s.lockUserScopedKey(user, req)
	existing, err := s.lookupUserScopedInput(r.Context(), user, clientID)
	if err != nil {
		unlock()
		http.Error(w, "input lookup failed: "+err.Error(), http.StatusInternalServerError)
		return func() {}, true
	}
	if existing != nil {
		unlock()
		s.writeQueueAck(w, r, http.StatusOK, existing.ConversationID, *existing)
		return func() {}, true
	}
	return unlock, false
}

// claimDirectInput claims a direct submission's input_id before its turn
// launches. handled reports that the response was already written (a replay
// of the key, a claim failure, or a Stop that covered the claim).
func (s *Server) claimDirectInput(w http.ResponseWriter, r *http.Request, user string, conv *store.Conversation, req chatRequest, releaseSlot func()) (claim *directClaim, handled bool) {
	clientID := strings.TrimSpace(req.InputID)
	if clientID == "" {
		return nil, false
	}
	claimID := uuid.NewString()
	row, created, err := s.store.ClaimDirectInput(r.Context(), store.InputQueueRow{
		ID: claimID, ConversationID: conv.ID, UserEmail: user,
		ClientInputID: clientID, SubmissionID: strings.TrimSpace(req.SubmissionID),
		Message: req.Message, Attachments: inputAttachmentsJSON(req),
	})
	if err != nil {
		// The claim may have committed with its acknowledgement lost. No
		// turn will run under it, so it is released (by its own id — never
		// another submission's claim) rather than left 'running' to answer
		// every resend "already running".
		releaseSlot()
		s.settleDirectInput(claimID, "") // settled "did not run", never left running
		http.Error(w, "input claim failed: "+err.Error(), http.StatusInternalServerError)
		return nil, true
	}
	if !created {
		releaseSlot()
		s.writeQueueAck(w, r, http.StatusOK, conv.ID, row)
		return nil, true
	}
	// The claim is an accepted input, so a Stop scope=all that begins
	// from here on covers it the way it covers a queued row — but the
	// Stop's queue sweep skips it (a claim is already 'running') and no
	// turn exists yet to cancel. So it carries the same gate a drained
	// row does: refused here if a Stop already began after acceptance,
	// and at registration if one begins while the turn is prepared.
	gen, stopped := s.stopGateForRow(conv.ID, row.AcceptedSeq)
	if stopped {
		releaseSlot()
		s.cancelStoppedDirectInput(w, row.ID)
		return nil, true
	}
	return &directClaim{id: row.ID, key: clientID, sweepGen: gen}, false
}

// maxInputIDLen bounds a caller's input_id / submission_id.
const maxInputIDLen = 256

// directClaim is a direct submission's idempotency claim as startTurn needs
// it: the claim row, and the Stop generation read when it was accepted (the
// registration gate refuses the launch if a Stop scope=all begins after).
type directClaim struct {
	id, key  string
	sweepGen uint64
}

// cancelStoppedDirectInput settles a claim that a Stop scope=all covered
// before its turn launched as cancelled (nothing ran), and tells the caller.
func (s *Server) cancelStoppedDirectInput(w http.ResponseWriter, id string) {
	s.settleDirectInput(id, "")
	http.Error(w, "a Stop in this conversation cancelled this message before it started", http.StatusConflict)
}

// directReleaseRetries bounds the background retries of a failed release,
// with delays doubling from directReleaseBackoff (about a minute in total).
const directReleaseRetries = 6

var directReleaseBackoff = time.Second // a var so tests can shorten it

// releaseDirectInput frees a direct claim whose turn never launched, so its
// key can be retried, and reports whether the release landed. It is tried a
// few times in place; one that still fails is retried in the background,
// because a claim left behind with no turn reads as "already running" to
// every resend of its key, and nothing would settle it before the next boot
// recovery.
func (s *Server) releaseDirectInput(user, convID string, c *directClaim) bool {
	if c == nil || c.id == "" {
		return true
	}
	for attempt := range directReleaseAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * directReleasePause)
		}
		if s.tryReleaseDirectInput(c.id) {
			return true
		}
	}
	// Unconfirmed: the delete may have committed with its acknowledgement
	// lost, and a concurrent resend may already have been told the claim is
	// running. Deleting again in the background could leave that key with no
	// record at all, so the key is settled "cancelled" (nothing ran) instead:
	// the claim if it is still there, a cancelled row if it is gone.
	s.settleUnreleasedInput(user, convID, c)
	return false
}

// settleUnreleasedInput gives a claim whose release could not be confirmed a
// durable outcome — cancelled, nothing ran — retried in the background.
func (s *Server) settleUnreleasedInput(user, convID string, c *directClaim) {
	try := func() bool {
		ctx, cancel := context.WithTimeout(detachedCtx(user, convID, ""), 5*time.Second)
		defer cancel()
		if err := s.store.SettleDirectInput(ctx, c.id, ""); err != nil {
			logging.From(ctx).Error("settle unreleased input failed", "input_id", safe.LogSafe(c.id), "err", err)
			return false
		}
		if _, _, err := s.store.CancelInputKey(ctx, store.InputQueueRow{
			ID: uuid.NewString(), ConversationID: convID, UserEmail: user, ClientInputID: c.key,
		}); err != nil {
			logging.From(ctx).Error("settle unreleased input failed", "input_id", safe.LogSafe(c.id), "err", err)
			return false
		}
		return true
	}
	if !try() {
		s.retryDirectInput(detachedCtx(user, convID, ""), "settle_unreleased", c.id, 1, directReleaseBackoff, try)
	}
}

// directReleaseAttempts and directReleasePause bound releaseDirectInput's
// in-place attempts (and directBindAttempts bindDirectInput's).
const (
	directReleaseAttempts = 3
	directBindAttempts    = 3
)

var directReleasePause = 100 * time.Millisecond // a var so tests can shorten it

// bindQueuedInputIf stamps the real turn ID on a drained queue row when
// queued is non-nil. Returns false only when the row is no longer running
// (cancelled by a concurrent Stop).
func (s *Server) bindQueuedInputIf(queued *queuedLaunch, turnID string) bool {
	if queued == nil || queued.rowID == "" {
		return true
	}
	bctx, bcancel := context.WithTimeout(detachedCtx("", "", turnID), 5*time.Second)
	bound, err := s.store.BindInputTurn(bctx, queued.rowID, turnID)
	bcancel()
	if err != nil {
		logging.From(bctx).Error("bind input turn failed", "input_id", safe.LogSafe(queued.rowID), "turn_id", safe.LogSafe(turnID), "err", err)
		return true
	}
	if bound {
		queued.claimTurnID = turnID
	}
	return bound
}

// bindDirectInput stamps a direct claim with its turn id, retrying a few
// times. bound false with stopped true means the claim is no longer running
// (a Stop cancelled it durably); bound false alone means the bind failed.
// With no direct claim (id == "") it reports bound (nothing to bind).
func (s *Server) bindDirectInput(id, turnID string) (bound, stopped bool) {
	if id == "" {
		return true, false
	}
	for attempt := range directBindAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * directReleasePause)
		}
		ctx, cancel := context.WithTimeout(detachedCtx("", "", turnID), 5*time.Second)
		bound, err := s.store.BindInputTurn(ctx, id, turnID)
		cancel()
		if err == nil {
			return bound, !bound // not bound: the claim is no longer running, so do not launch
		}
		logging.From(ctx).Error("bind direct input turn failed", "input_id", safe.LogSafe(id), "turn_id", safe.LogSafe(turnID), "err", err)
	}
	return false, false
}

func (s *Server) tryReleaseDirectInput(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.ReleaseDirectInput(ctx, id); err != nil {
		logging.From(ctx).Error("release direct input failed", "input_id", safe.LogSafe(id), "err", err)
		return false
	}
	return true
}

// retryDirectInput retries one write to a direct claim in the background,
// doubling the delay, until it lands or directReleaseRetries run out (boot
// recovery then settles the claim). A claim left 'running' with no live turn
// would answer every resend of its key "already running".
func (s *Server) retryDirectInput(ctx context.Context, what, id string, attempt int, delay time.Duration, try func() bool) {
	if attempt > directReleaseRetries {
		logging.From(ctx).Warn("direct input: giving up after retries; boot recovery settles it", "op", safe.LogSafe(what), "input_id", safe.LogSafe(id), "retries", directReleaseRetries)
		return
	}
	s.background.After("httpapi.direct_"+what, delay, func() {
		if !try() {
			s.retryDirectInput(ctx, what, id, attempt+1, 2*delay, try)
		}
	})
}

// settleDirectInput resolves a direct claim (completed when turnID's user
// entry committed, otherwise cancelled) on its own bounded context, retrying
// a failure in the background.
func (s *Server) settleDirectInput(id, turnID string) {
	try := func() bool {
		ctx, cancel := context.WithTimeout(detachedCtx("", "", turnID), 5*time.Second)
		defer cancel()
		if err := s.store.SettleDirectInput(ctx, id, turnID); err != nil {
			logging.From(ctx).Error("settle direct input failed", "input_id", safe.LogSafe(id), "turn_id", safe.LogSafe(turnID), "err", err)
			return false
		}
		return true
	}
	if !try() {
		s.retryDirectInput(detachedCtx("", "", turnID), "settle", id, 1, directReleaseBackoff, try)
	}
}

// settleTurnInputs settles a finished turn's queue rows (SettleTurnInputs),
// first cancelling what a Stop by key named while the turn ran — an injected
// steer, or the drained row itself — so an uncommitted one is not returned to
// the queue for a drain to run after its Stop. The Stop may have been
// answered before the turn's terminal frame (202, unconfirmed), so the Stop's
// own row writes cannot be what keeps the input from running again. The
// cancels are guarded: an input whose user entry (or steered text) committed
// ran, and the settlement records it completed.
//
// A cancel that fails leaves the turn's rows unsettled rather than settle
// them without it (a re-queue is exactly what it prevents): a background
// retry runs the cancels and then the settlement, and boot recovery covers a
// retry that runs out.
func (s *Server) settleTurnInputs(buf *turnBuffer, turnID, queueRowID string) (requeued, cancelledSteers int, err error) {
	steers := buf.stoppedSteerIDs()
	drained := ""
	if queueRowID != "" && buf.stoppedByKey.Load() {
		drained = queueRowID
	}
	cancelStopped := func(ctx context.Context) bool {
		for _, id := range steers {
			if _, err := s.store.CancelStoppedSteer(ctx, id); err != nil {
				logging.From(ctx).Error("cancel stopped steer failed", "input_id", safe.LogSafe(id), "turn_id", safe.LogSafe(turnID), "err", err)
				return false
			}
		}
		if drained != "" {
			if _, err := s.store.CancelStoppedDrain(ctx, drained, turnID); err != nil {
				logging.From(ctx).Error("cancel stopped drain failed", "input_id", safe.LogSafe(drained), "turn_id", safe.LogSafe(turnID), "err", err)
				return false
			}
		}
		return true
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if !cancelStopped(sctx) {
		s.retryDirectInput(detachedCtx("", buf.convID, turnID), "cancel_stopped_inputs", turnID, 1, directReleaseBackoff, func() bool {
			ctx, cancel := context.WithTimeout(detachedCtx("", buf.convID, turnID), 5*time.Second)
			defer cancel()
			if !cancelStopped(ctx) {
				return false
			}
			if _, _, err := s.store.SettleTurnInputs(ctx, turnID, queueRowID); err != nil {
				logging.From(ctx).Error("settle turn inputs failed", "turn_id", safe.LogSafe(turnID), "err", err)
				return false
			}
			s.maybeDrainQueue(buf.convID)
			return true
		})
		return 0, 0, nil
	}
	return s.store.SettleTurnInputs(sctx, turnID, queueRowID)
}

// inputAttachmentsJSON is the attachments column for an input row.
func inputAttachmentsJSON(req chatRequest) string {
	if len(req.Attachments) > 0 {
		if raw, err := json.Marshal(req.Attachments); err == nil {
			return string(raw)
		}
	}
	return "[]"
}
