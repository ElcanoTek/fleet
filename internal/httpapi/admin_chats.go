package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Chat administration (ADR-0115): the admin-only /admin/chats routes behind
// Settings -> Chats, and the executor that carries out the admin requests
// (send a message, stop, transfer) that `fleet chats` on the host records in
// chat.admin_requests. Both reach the chat through the same code the owner's
// own routes use: a message is a queue row drained by startTurn, a stop is the
// owner's Stop, a transfer is the owner's transfer.

// adminChatStore is the store surface chat administration needs. The real
// store implements it; fakes that do not are told the feature is unavailable.
type adminChatStore interface {
	AdminListChats(ctx context.Context, f store.AdminChatFilter) ([]store.AdminChat, error)
	AdminGetChat(ctx context.Context, idOrPrefix string) (store.AdminChat, error)
	AdminGetChatAny(ctx context.Context, idOrPrefix string) (store.AdminChat, error)
	AdminMoveChatToBin(ctx context.Context, convID, notice string) (int, error)
	AdminRestoreChat(ctx context.Context, convID string) error
	AdminPurgeChat(ctx context.Context, convID string) error
	AdminChatTranscript(ctx context.Context, convID string, o store.AdminTranscriptOptions) ([]store.AdminTranscriptEntry, error)
	AdminAppendNotice(ctx context.Context, a store.AdminActor, convID, text string) error
	AdminUpdateChat(ctx context.Context, a store.AdminActor, convID string, c store.AdminChatChange, reason string) (store.AdminChat, error)
	AdminSetChatScheduleCron(ctx context.Context, a store.AdminActor, in store.AdminChatScheduleCronEdit) (store.AdminChatScheduleCronResult, error)
	ListAdminActions(ctx context.Context, convID string, limit int) ([]store.AdminAction, error)
	CreateAdminRequest(ctx context.Context, in store.AdminRequestInput) (store.AdminRequest, error)
	GetAdminRequest(ctx context.Context, id string) (store.AdminRequest, error)
	ListPendingAdminRequests(ctx context.Context, limit int) ([]store.AdminRequest, error)
	FinishAdminRequest(ctx context.Context, id, state, result string) (bool, error)
	ClaimAdminRequest(ctx context.Context, id string) (bool, error)
	FailStaleAdminRequests(ctx context.Context, olderThan time.Duration) (int, error)
	AppendAdminRequestNotice(ctx context.Context, convID, text string) error
	QueueAdminMessage(ctx context.Context, requestID string) (store.InputQueueRow, bool, error)
	AdminListChatSchedules(ctx context.Context, f store.AdminChatScheduleFilter) ([]store.AdminChatSchedule, error)
	AdminGetChatSchedule(ctx context.Context, idOrPrefix string) (store.AdminChatSchedule, error)
	AdminSetChatSchedulePrompt(ctx context.Context, in store.AdminChatSchedulePromptEdit) (before, after store.AdminChatSchedule, err error)
	AdminSetChatScheduleStatus(ctx context.Context, a store.AdminActor, idOrPrefix, status, reason string) (store.AdminChatSchedule, error)
}

func (s *Server) adminChats() (adminChatStore, bool) {
	st, ok := s.store.(adminChatStore)
	return st, ok
}

// unattendedTurn reports a turn nobody is at the owner's keyboard for that
// fleet must not treat as the owner's own: a patrol run, or a message an
// administrator sent (ADR-0115). Such a turn arms no show_ui auto-submit
// countdown, offers no optional connectors, and reads the Autonomous note
// without them.
func unattendedTurn(inputKind string) bool {
	return inputKind == agent.InputKindChatPatrol || inputKind == agent.InputKindAdminMessage
}

// adminSenderKey carries the administrator who wrote an admin message from
// the queue drain to the turn's execution context.
type adminSenderKey struct{}

// withAdminSender returns ctx unchanged for every turn but an admin message.
func withAdminSender(ctx context.Context, email string) context.Context {
	if email == "" {
		return ctx
	}
	return context.WithValue(ctx, adminSenderKey{}, email)
}

func adminSenderFrom(ctx context.Context) string {
	v, _ := ctx.Value(adminSenderKey{}).(string)
	return v
}

// adminEmailForTurn is the administrator to record on an admin-message
// turn's input; "" for every other turn.
func adminEmailForTurn(ctx context.Context, inputKind string) string {
	if inputKind != agent.InputKindAdminMessage {
		return ""
	}
	return adminSenderFrom(ctx)
}

// ---------------------------------------------------------------------------
// Executing admin requests

// adminRequestBatch bounds how many pending requests one pass carries out.
const adminRequestBatch = 20

// adminRequestStaleAfter is how long a claimed stop / transfer / archive may
// stay running before the sweep settles it as interrupted. Generous on
// purpose: a transfer that moves a participant's attachments across volumes
// can take many minutes, and settling a live one as failed would be wrong.
const adminRequestStaleAfter = time.Hour

// ExecuteAdminRequests carries out pending admin requests. The scheduler's
// chat pass calls it (through KickQueuedChatScheduleTurns) every tick, which
// is how a request `fleet chats` wrote on the host reaches the running
// server; a request made in the browser is executed at once.
func (s *Server) ExecuteAdminRequests(ctx context.Context) {
	st, ok := s.adminChats()
	if !ok || s.shuttingDown.Load() || UpdateDrainActive() {
		return
	}
	if n, err := st.FailStaleAdminRequests(ctx, adminRequestStaleAfter); err != nil {
		logging.From(ctx).Error("admin requests: settle stale failed", "err", safe.LogSafe(err.Error()))
	} else if n > 0 {
		logging.From(ctx).Warn("admin requests: settled interrupted requests as failed", "count", n)
	}
	reqs, err := st.ListPendingAdminRequests(ctx, adminRequestBatch)
	if err != nil {
		logging.From(ctx).Error("admin requests: list failed", "err", safe.LogSafe(err.Error()))
		return
	}
	for _, r := range reqs {
		s.executeAdminRequest(ctx, st, r)
	}
}

func (s *Server) executeAdminRequest(ctx context.Context, st adminChatStore, r store.AdminRequest) {
	log := logging.From(ctx).With("admin_request", r.ID, "kind", r.Kind, "conv_id", safe.LogSafe(r.ConversationID), "actor", safe.LogSafe(r.Actor))
	switch r.Kind {
	case store.AdminRequestMessage:
		row, ok, err := st.QueueAdminMessage(ctx, r.ID)
		if err != nil {
			log.Error("admin requests: queue message failed", "err", safe.LogSafe(err.Error()))
			return // stays pending; the next pass retries
		}
		if ok && row.ID != "" {
			s.emitQueueUpdate(ctx, "", r.ConversationID)
			s.maybeDrainQueue(r.ConversationID)
			log.Info("admin requests: message queued", "input_id", row.ID)
		}
	case store.AdminRequestStop, store.AdminRequestTransfer, store.AdminRequestArchive,
		store.AdminRequestDelete, store.AdminRequestRestore, store.AdminRequestPurge:
		// Claimed first: the browser path and the scheduler sweep can both
		// reach a pending request, and these act exactly once.
		ok, err := st.ClaimAdminRequest(ctx, r.ID)
		if err != nil {
			log.Error("admin requests: claim failed", "err", safe.LogSafe(err.Error()))
			return
		}
		if !ok {
			return
		}
		s.executeClaimedAdminRequest(ctx, st, r)
	default:
		s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "unknown request kind", "")
	}
}

// executeClaimedAdminRequest carries out a stop, transfer or archive this
// process has claimed.
func (s *Server) executeClaimedAdminRequest(ctx context.Context, st adminChatStore, r store.AdminRequest) {
	log := logging.From(ctx).With("admin_request", r.ID, "kind", r.Kind, "conv_id", safe.LogSafe(r.ConversationID))
	switch r.Kind {
	case store.AdminRequestStop:
		running := s.adminStopConversation(ctx, r.ConversationID)
		result := "nothing was running; queued inputs cancelled"
		if running {
			result = "stopped the running turn; queued inputs cancelled"
		}
		s.finishAdminRequest(ctx, st, r, store.AdminRequestDone, result,
			fmt.Sprintf("An administrator stopped this chat's work. Reason: %s", r.Reason))
	case store.AdminRequestTransfer:
		conv, err := s.store.GetConversationOwnerAndMultiplayer(ctx, r.ConversationID)
		if err != nil {
			log.Error("admin requests: transfer read failed", "err", safe.LogSafe(err.Error()))
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "could not read the chat; ask again", "")
			return
		}
		if conv == nil {
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "the chat no longer exists", "")
			return
		}
		res, err := s.transferConversation(ctx, conv, r.Payload)
		if err != nil {
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, err.Error(), "")
			return
		}
		s.finishAdminRequest(ctx, st, r, store.AdminRequestDone,
			fmt.Sprintf("transferred from %s to %s (%d patrols moved, %d paused)", res.FromEmail, res.ToEmail, res.PatrolsMoved, res.PatrolsPaused),
			fmt.Sprintf("An administrator transferred this chat from %s to %s. Reason: %s", res.FromEmail, res.ToEmail, r.Reason))
	case store.AdminRequestArchive:
		archive := r.Payload == "archive"
		conv, err := s.store.GetConversationOwnerAndMultiplayer(ctx, r.ConversationID)
		if err != nil {
			log.Error("admin requests: archive read failed", "err", safe.LogSafe(err.Error()))
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "could not read the chat; ask again", "")
			return
		}
		if conv == nil {
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "the chat no longer exists", "")
			return
		}
		// The owner's archive path (handleConversationArchive): archiving
		// unshares the chat, so its participants lose their turns, queued
		// inputs, approvals and live streams with it.
		if err := s.store.SetArchived(ctx, conv.UserEmail, conv.ID, archive); err != nil {
			log.Error("admin requests: archive failed", "err", safe.LogSafe(err.Error()))
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "archive failed", "")
			return
		}
		what := "restored this chat from the archive"
		if archive {
			s.enforceMultiplayerRevocation(ctx, conv.ID, &store.Conversation{UserEmail: conv.UserEmail}, nil)
			what = "archived this chat"
		}
		s.finishAdminRequest(ctx, st, r, store.AdminRequestDone, strings.TrimSuffix(what, " this chat")+" the chat",
			fmt.Sprintf("An administrator %s. Reason: %s", what, r.Reason))
	case store.AdminRequestDelete:
		s.adminDeleteToBin(ctx, st, r)
	case store.AdminRequestRestore:
		if err := st.AdminRestoreChat(ctx, r.ConversationID); err != nil {
			s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, err.Error(), "")
			return
		}
		s.finishAdminRequest(ctx, st, r, store.AdminRequestDone, "restored from the recycle bin; its patrols stay paused",
			fmt.Sprintf("An administrator restored this chat from the recycle bin. Its patrols are paused until someone resumes them. Reason: %s", r.Reason))
	case store.AdminRequestPurge:
		s.adminPurge(ctx, st, r)
	}
}

// adminDeleteToBin is an administrator's delete: note it in the chat (seen if
// it is restored), move it to the recycle bin (schedules paused, queued
// inputs cancelled and the chat tombstoned, in one transaction), then stop
// its running work. Nothing
// else is taken apart — sharing, participants, their paused patrols and
// pending cards, private attachments — so a restore gives the chat back
// whole; a tombstoned chat is already unreachable to everyone. Only what
// lives in this process is released: the sandbox and session approvals.
func (s *Server) adminDeleteToBin(ctx context.Context, st adminChatStore, r store.AdminRequest) {
	// Nothing is stopped until the move has succeeded: a failed delete must
	// leave the chat exactly as it was.
	paused, err := st.AdminMoveChatToBin(ctx, r.ConversationID,
		fmt.Sprintf("An administrator moved this chat to the recycle bin; it is deleted for good after %d days unless restored. Reason: %s", store.AdminRecycleBinDays, r.Reason))
	if err != nil {
		s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "delete failed: "+err.Error(), "")
		return
	}
	// The chat is in the bin: stop whatever was running in it.
	s.adminStopConversation(ctx, r.ConversationID)
	s.releasePersistentSandbox(r.ConversationID)
	s.sessionApprovals.Forget(r.ConversationID)
	s.finishAdminRequest(ctx, st, r, store.AdminRequestDone,
		fmt.Sprintf("moved to the recycle bin (%d patrol(s) paused); deleted for good after %d days unless restored", paused, store.AdminRecycleBinDays), "")
}

// adminPurge deletes a chat permanently at once — the emergency path for a
// full disk. The row is deleted FIRST: if that fails, nothing else has been
// taken away. Then the owner's delete clean-up runs (handleConversationDelete:
// participants' turns and streams, the private stash, the sandbox, session
// approvals), and the orphan sweep reclaims the workspace right away.
func (s *Server) adminPurge(ctx context.Context, st adminChatStore, r store.AdminRequest) {
	owner := ""
	if c, err := st.AdminGetChatAny(ctx, r.ConversationID); err == nil {
		owner = c.OwnerEmail
	}
	if err := st.AdminPurgeChat(ctx, r.ConversationID); err != nil {
		s.finishAdminRequest(ctx, st, r, store.AdminRequestFailed, "permanent delete failed: "+err.Error(), "")
		return
	}
	s.adminStopConversation(ctx, r.ConversationID)
	s.enforceMultiplayerRevocation(ctx, r.ConversationID, &store.Conversation{UserEmail: owner}, nil)
	s.removeConversationPrivateStash(r.ConversationID)
	s.releasePersistentSandbox(r.ConversationID)
	s.sessionApprovals.Forget(r.ConversationID)
	s.finishAdminRequest(ctx, st, r, store.AdminRequestDone, "deleted permanently; its files are reclaimed by the workspace sweep", "")
	s.background.Go("httpapi.admin_purge_sweep", func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if n, err := s.store.SweepOrphanWorkspaces(sctx, tools.WorkspaceDirForConversation("")); err != nil {
			logging.From(sctx).Error("admin purge: workspace sweep failed", "err", safe.LogSafe(err.Error()))
		} else if n > 0 {
			logging.From(sctx).Info("admin purge: reclaimed workspaces", "count", n)
		}
	})
}

func (s *Server) finishAdminRequest(ctx context.Context, st adminChatStore, r store.AdminRequest, state, result, notice string) {
	// Settled on a fresh, bounded context: the work may have outlived the
	// caller's (the scheduler pass gives a minute; a transfer moving a
	// participant's attachments can take longer), and a request that was
	// carried out must never be reported as interrupted for that.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	ok, err := st.FinishAdminRequest(ctx, r.ID, state, result)
	if err != nil {
		logging.From(ctx).Error("admin requests: settle failed", "admin_request", r.ID, "err", safe.LogSafe(err.Error()))
		return
	}
	if ok && notice != "" {
		if err := st.AppendAdminRequestNotice(ctx, r.ConversationID, notice); err != nil {
			logging.From(ctx).Warn("admin requests: notice failed", "admin_request", r.ID, "err", safe.LogSafe(err.Error()))
		}
	}
}

// adminStopConversation is the owner's Stop (scope all) for an
// administrator: cancel the running turn, then the queued inputs and the
// chat's auto-submits. It reports whether a turn was running.
func (s *Server) adminStopConversation(ctx context.Context, convID string) bool {
	epoch, entry, running := s.beginStopSweep(convID)
	defer s.endStopSweep(convID)
	if running {
		entry.cancel()
	}
	qctx, qcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer qcancel()
	if n, err := s.store.CancelQueuedInputs(qctx, "", convID, epoch); err != nil {
		logging.From(ctx).Error("admin stop: cancel queued inputs failed", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
	} else if n > 0 {
		s.emitQueueUpdate(qctx, "", convID)
	}
	if err := s.cancelConversationAutoSubmitsErr(qctx, convID); err != nil {
		logging.From(ctx).Error("admin stop: cancel auto-submits failed", "conv_id", safe.LogSafe(convID), "err", safe.LogSafe(err.Error()))
	}
	return running
}

// ---------------------------------------------------------------------------
// HTTP: /admin/chats (Settings -> Chats)

func (s *Server) adminActor(r *http.Request) store.AdminActor {
	return store.AdminActor{Email: userFromCtx(r.Context()), Via: store.AdminViaWeb}
}

// adminChatError maps a store error to a status and a message the browser
// can show.
func adminChatError(w http.ResponseWriter, err error) {
	var stale *store.ChatSchedulePromptChangedError
	var staleDef *store.ChatScheduleDefinitionChangedError
	switch {
	case errors.Is(err, store.ErrConversationNotFound), errors.Is(err, store.ErrChatScheduleNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.As(err, &stale):
		http.Error(w, "the patrol's instructions changed since you opened them; reload and edit again", http.StatusConflict)
	case errors.As(err, &staleDef):
		http.Error(w, "the patrol changed since you opened it; reload and edit again", http.StatusConflict)
	case errors.Is(err, store.ErrAdminReasonRequired):
		http.Error(w, "a reason is required: it is recorded and shown in the chat", http.StatusBadRequest)
	case errors.Is(err, store.ErrAdminChatNotAllowed), errors.Is(err, store.ErrInvalidChatSchedule),
		errors.Is(err, store.ErrAdminChatAmbiguous), errors.Is(err, store.ErrChatScheduleAmbiguous):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

// handleAdminChats serves GET /admin/chats: every user's chats.
func (s *Server) handleAdminChats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st, ok := s.adminChats()
	if !ok {
		http.Error(w, "chat administration is not available", http.StatusNotImplemented)
		return
	}
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("active_days"))
	deleted := q.Get("deleted") == "1" || q.Get("deleted") == "true"
	limit, _ := strconv.Atoi(q.Get("limit"))
	chats, err := st.AdminListChats(r.Context(), store.AdminChatFilter{
		OwnerEmail: q.Get("owner"), Search: q.Get("q"), ActiveWithinDays: days,
		IncludeArchived: q.Get("archived") == "1" || q.Get("archived") == "true", Deleted: deleted, Limit: limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{"chats": chats})
}

// handleAdminChatItem serves the /admin/chats/{id}[/...] item routes:
//
//	GET   /admin/chats/{id}                 the chat, its patrols, its queued
//	                                        inputs and its recent admin changes
//	PATCH /admin/chats/{id}                 title, model, approval mode, archive
//	GET   /admin/chats/{id}/history         the transcript's latest entries
//	POST  /admin/chats/{id}/requests        send a message, stop, or transfer
//	POST  /admin/chats/{id}/notice          a notice in the chat
//
// (An admin request's progress is GET /admin/chat-requests/{id}; the record of
// admin changes across every chat is GET /admin/chat-actions.)
func (s *Server) handleAdminChatItem(w http.ResponseWriter, r *http.Request) {
	st, ok := s.adminChats()
	if !ok {
		http.Error(w, "chat administration is not available", http.StatusNotImplemented)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/chats/"), "/")
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Any chat, including one in the recycle bin: it can be read, restored or
	// purged there; everything that changes a live chat refuses it.
	chat, err := st.AdminGetChatAny(r.Context(), parts[0])
	if err != nil {
		adminChatError(w, err)
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = strings.Join(parts[1:], "/")
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.adminChatDetail(w, r, st, chat)
	case sub == "" && r.Method == http.MethodPatch:
		s.adminChatPatch(w, r, st, chat)
	case sub == "history" && r.Method == http.MethodGet:
		q := r.URL.Query()
		tail, _ := strconv.Atoi(q.Get("tail"))
		entries, err := st.AdminChatTranscript(r.Context(), chat.ID, store.AdminTranscriptOptions{
			Tail: tail, IncludeTools: q.Get("tools") == "1" || q.Get("tools") == "true",
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, map[string]any{"entries": entries})
	case sub == "requests" && r.Method == http.MethodPost:
		s.adminChatRequest(w, r, st, chat)
	case sub == "notice" && r.Method == http.MethodPost:
		var body struct {
			Text string `json:"text"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		if err := st.AdminAppendNotice(r.Context(), s.adminActor(r), chat.ID, body.Text); err != nil {
			adminChatError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) adminChatDetail(w http.ResponseWriter, r *http.Request, st adminChatStore, chat store.AdminChat) {
	ctx := r.Context()
	patrols, err := st.AdminListChatSchedules(ctx, store.AdminChatScheduleFilter{ConversationID: chat.ID, PatrolsOnly: false})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	actions, err := st.ListAdminActions(ctx, chat.ID, 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	queue, err := s.store.ListQueuedInputs(ctx, "", chat.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type queued struct {
		ID        string `json:"id"`
		Mode      string `json:"mode"`
		State     string `json:"state"`
		User      string `json:"user_email"`
		Sender    string `json:"sender_email,omitempty"`
		CreatedAt int64  `json:"created_at"`
	}
	qs := make([]queued, 0, len(queue))
	for _, q := range queue {
		qs = append(qs, queued{ID: q.ID, Mode: q.Mode, State: q.State, User: q.UserEmail, Sender: q.SenderEmail, CreatedAt: q.CreatedAt})
	}
	if patrols == nil {
		patrols = []store.AdminChatSchedule{}
	}
	writeJSON(w, r, map[string]any{"chat": chat, "patrols": patrols, "queue": qs, "actions": actions})
}

func (s *Server) adminChatPatch(w http.ResponseWriter, r *http.Request, st adminChatStore, chat store.AdminChat) {
	var body struct {
		Title        *string `json:"title"`
		Model        *string `json:"model"`
		ApprovalMode *string `json:"approval_mode"`
		Archived     *bool   `json:"archived"`
		Reason       string  `json:"reason"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	change := store.AdminChatChange{Title: body.Title, Model: body.Model, ApprovalMode: body.ApprovalMode}
	archive := body.Archived != nil && *body.Archived != chat.Archived
	if archive && strings.TrimSpace(body.Reason) == "" {
		http.Error(w, "a reason is required: it is recorded and shown in the chat", http.StatusBadRequest)
		return
	}
	after := chat
	if change.Title != nil || change.Model != nil || change.ApprovalMode != nil {
		var err error
		if after, err = st.AdminUpdateChat(r.Context(), s.adminActor(r), chat.ID, change, body.Reason); err != nil {
			adminChatError(w, err)
			return
		}
	}
	if archive {
		payload := "unarchive"
		if *body.Archived {
			payload = "archive"
		}
		req, err := st.CreateAdminRequest(r.Context(), store.AdminRequestInput{
			Actor: s.adminActor(r), Kind: store.AdminRequestArchive, ConversationID: chat.ID, Payload: payload, Reason: body.Reason,
		})
		if err != nil {
			adminChatError(w, err)
			return
		}
		ctx := context.WithoutCancel(r.Context())
		s.executeAdminRequest(ctx, st, req)
		if got, gerr := st.GetAdminRequest(ctx, req.ID); gerr == nil && got.State == store.AdminRequestFailed {
			http.Error(w, "archive failed: "+got.Result, http.StatusConflict)
			return
		}
		if fresh, gerr := st.AdminGetChat(ctx, chat.ID); gerr == nil {
			after = fresh
		}
	}
	writeJSON(w, r, map[string]any{"chat": after})
}

func (s *Server) adminChatRequest(w http.ResponseWriter, r *http.Request, st adminChatStore, chat store.AdminChat) {
	var body struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		ToEmail string `json:"to_email"`
		Reason  string `json:"reason"`
		// Confirm must repeat the chat's title (or id) for a permanent
		// delete, so it is never one stray click.
		Confirm string `json:"confirm"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	payload := ""
	switch body.Kind {
	case store.AdminRequestMessage:
		payload = body.Message
	case store.AdminRequestTransfer:
		payload = body.ToEmail
	case store.AdminRequestStop, store.AdminRequestDelete, store.AdminRequestRestore:
	case store.AdminRequestPurge:
		// The title; the id only for a chat that has none.
		want := strings.TrimSpace(chat.Title)
		if want == "" {
			want = chat.ID
		}
		if strings.TrimSpace(body.Confirm) != want {
			http.Error(w, "type the chat's title to confirm a permanent delete", http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "kind must be message, stop, transfer, delete, restore or purge (archive through PATCH)", http.StatusBadRequest)
		return
	}
	req, err := st.CreateAdminRequest(r.Context(), store.AdminRequestInput{
		Actor: s.adminActor(r), Kind: body.Kind, ConversationID: chat.ID, Payload: payload, Reason: body.Reason,
	})
	if err != nil {
		adminChatError(w, err)
		return
	}
	// Carried out now, on a context that outlives the request, rather than
	// on the next scheduler pass.
	ctx := context.WithoutCancel(r.Context())
	s.executeAdminRequest(ctx, st, req)
	if got, gerr := st.GetAdminRequest(ctx, req.ID); gerr == nil {
		req = got
	}
	status := http.StatusAccepted
	if req.State == store.AdminRequestFailed {
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"request": req})
}

// handleAdminPatrolItem serves PATCH /admin/patrols/{id}: an administrator
// edits a patrol's instructions (compare-and-swap on the prompt's sha256),
// changes its schedule (cron and/or timezone, compare-and-swap on its
// def_version; dry_run previews the next runs and changes nothing), or
// pauses / resumes it. One change per request.
func (s *Server) handleAdminPatrolItem(w http.ResponseWriter, r *http.Request) {
	st, ok := s.adminChats()
	if !ok {
		http.Error(w, "chat administration is not available", http.StatusNotImplemented)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/patrols/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		p, err := st.AdminGetChatSchedule(r.Context(), id)
		if err != nil {
			adminChatError(w, err)
			return
		}
		writeJSON(w, r, map[string]any{"patrol": p})
		return
	}
	if r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Prompt           *string `json:"prompt"`
		ExpectSHA256     string  `json:"expect_sha256"`
		Title            *string `json:"title"`
		Status           *string `json:"status"`
		Cron             *string `json:"cron"`
		Timezone         *string `json:"timezone"`
		ExpectDefVersion *int64  `json:"expect_def_version"`
		DryRun           bool    `json:"dry_run"`
		Reason           string  `json:"reason"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	actor := s.adminActor(r)
	var out store.AdminChatSchedule
	schedule := body.Cron != nil || body.Timezone != nil
	changes := 0
	for _, set := range []bool{body.Prompt != nil, body.Status != nil, schedule} {
		if set {
			changes++
		}
	}
	if body.DryRun && !schedule {
		http.Error(w, "dry_run previews a schedule change: send cron and/or timezone", http.StatusBadRequest)
		return
	}
	var nextRuns []int64
	switch {
	case changes > 1:
		http.Error(w, "change the instructions, the schedule or the status, one at a time", http.StatusBadRequest)
		return
	case schedule:
		in := store.AdminChatScheduleCronEdit{ID: id, ExpectDefVersion: body.ExpectDefVersion, Reason: body.Reason, DryRun: body.DryRun}
		if body.Cron != nil {
			in.CronExpr = *body.Cron
		}
		if body.Timezone != nil {
			in.Timezone = *body.Timezone
		}
		res, err := st.AdminSetChatScheduleCron(r.Context(), actor, in)
		if err != nil {
			adminChatError(w, err)
			return
		}
		if res.DryRun {
			writeJSON(w, r, map[string]any{"patrol": res.Before, "preview": res.After, "changed": res.Changed, "next_runs": res.NextRuns})
			return
		}
		if res.Changed && res.After.Status == store.ChatScheduleStatusActive {
			s.armShortScheduleTimer(res.After.NextRunAt)
		}
		out, nextRuns = res.After, res.NextRuns
	case body.Prompt != nil:
		why := strings.Join(strings.Fields(body.Reason), " ")
		if why == "" {
			http.Error(w, "a reason is required: it is recorded and shown in the chat", http.StatusBadRequest)
			return
		}
		cur, err := st.AdminGetChatSchedule(r.Context(), id)
		if err != nil {
			adminChatError(w, err)
			return
		}
		title := cur.Title
		if body.Title != nil && strings.TrimSpace(*body.Title) != "" {
			title = strings.TrimSpace(*body.Title)
		}
		_, after, err := st.AdminSetChatSchedulePrompt(r.Context(), store.AdminChatSchedulePromptEdit{
			ID: cur.ID, ExpectSHA256: body.ExpectSHA256, Prompt: *body.Prompt, Title: body.Title,
			Notice: fmt.Sprintf("An administrator updated the instructions of the patrol %q. Reason: %s", title, why),
			Actor:  &actor, Reason: why,
		})
		if err != nil {
			adminChatError(w, err)
			return
		}
		out = after
	case body.Status != nil:
		after, err := st.AdminSetChatScheduleStatus(r.Context(), actor, id, strings.TrimSpace(*body.Status), body.Reason)
		if err != nil {
			adminChatError(w, err)
			return
		}
		if after.Status == store.ChatScheduleStatusActive {
			s.armShortScheduleTimer(after.NextRunAt)
		}
		out = after
	default:
		http.Error(w, "nothing to change: send prompt (with expect_sha256), cron and/or timezone (with expect_def_version), or status", http.StatusBadRequest)
		return
	}
	if conv, cerr := s.store.Get(r.Context(), out.UserEmail, out.ConversationID); cerr == nil && conv != nil {
		s.emitPatrolUpdate(r.Context(), out.UserEmail, out.ConversationID, nil, conv.AutoApprove, conv.EffectiveApprovalMode())
	}
	if nextRuns != nil {
		writeJSON(w, r, map[string]any{"patrol": out, "next_runs": nextRuns})
		return
	}
	writeJSON(w, r, map[string]any{"patrol": out})
}

// handleAdminChatRequest serves GET /admin/chat-requests/{id}: an admin
// request's progress (pending, done or failed; for a message, its queue row's
// state and turn).
func (s *Server) handleAdminChatRequest(w http.ResponseWriter, r *http.Request) {
	st, ok := s.adminChats()
	if !ok {
		http.Error(w, "chat administration is not available", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/chat-requests/"), "/")
	req, err := st.GetAdminRequest(r.Context(), id)
	if id == "" || err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, r, map[string]any{"request": req})
}

// handleAdminChatActions serves GET /admin/chat-actions: the record of admin
// changes across every chat, newest first.
func (s *Server) handleAdminChatActions(w http.ResponseWriter, r *http.Request) {
	st, ok := s.adminChats()
	if !ok {
		http.Error(w, "chat administration is not available", http.StatusNotImplemented)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	actions, err := st.ListAdminActions(r.Context(), "", limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{"actions": actions})
}
