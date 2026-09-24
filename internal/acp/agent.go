// Package acp is `fleet acp`: fleet as an Agent Client Protocol agent (#984).
//
// ACP (agentclientprotocol.com — the Zed/JetBrains protocol, not IBM's older
// Agent Communication Protocol) is JSON-RPC 2.0 over stdio, client → agent: an
// ACP client (buzz-acp, Zed, JetBrains, …) launches `fleet acp` as a
// subprocess and drives sessions over its stdin/stdout.
//
// The load-bearing design choice is the same one A2A made (ADR-0051): ACP is a
// protocol-shaped translation of a seam fleet already ships, not a new seam.
// `fleet acp` never builds an agent in-process. Each session/prompt becomes one
// POST /chat turn against the RUNNING fleet server — exactly what `fleet chat`
// sends — so the turn still runs through the one governed loop
// (agentcore.Run), the sandbox, the cost/token ceilings, the audit trail and
// the host-side MCP credential broker. This package only translates frames;
// TestPackageStaysAClient pins that it imports none of the execution packages.
//
// Wire types and JSON-RPC framing come from github.com/coder/acp-go-sdk, a
// stdlib-only module generated from the official ACP schema. Only its types
// and its connection are used; fleet is the executor.
package acp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/chattui"
)

// SpecVersion is the ACP protocol version this adapter speaks, pinned in one
// place. It is the SDK's generated ProtocolVersionNumber; bumping the SDK is a
// deliberate PR that re-checks the mapping below against the new schema.
const SpecVersion = acpsdk.ProtocolVersionNumber

// revisedMarker separates a superseded draft from the final answer. ACP
// updates are append-only, so when fleet's final text does not extend what was
// already streamed (an enforcement round replaced the draft), the client gets
// the final answer again after this line rather than silently diverging from
// the persisted conversation.
const revisedMarker = "\n\n— revised answer —\n\n"

// turnClient is the slice of chattui.Client the adapter needs: stream one
// governed turn, and stop one server-side.
type turnClient interface {
	StreamInput(ctx context.Context, message, convID, inputID string, onEvent func(chattui.Event)) (string, error)
	Cancel(convID, turnID string) error
	CancelInput(convID, inputID string) error
}

// updater sends session/update notifications (the AgentSideConnection in
// production; a recorder in tests).
type updater interface {
	SessionUpdate(ctx context.Context, params acpsdk.SessionNotification) error
}

// Agent implements acpsdk.Agent on top of a running fleet server.
type Agent struct {
	client turnClient
	// cfgErr is a connection-config problem (no email, no token) found at
	// startup. It is reported on session/new and session/prompt as an
	// auth_required error, so the ACP client shows the actual fix instead of
	// "agent exited".
	cfgErr    error
	publicURL string
	timeout   time.Duration
	version   string

	conn updater

	mu       sync.Mutex
	sessions map[acpsdk.SessionId]*session
}

// session maps one ACP session onto one fleet conversation. The conversation
// id is learned from the first turn's `conversation` event; until then the
// session has no server-side state at all.
type session struct {
	mu     sync.Mutex // serializes prompts on this session
	convID string
	cwd    string
	// ns scopes the keys built from a client messageId to this session. fleet
	// looks a first prompt's key up per user (it has no conversation yet), and
	// a client may number messageIds per session, so an unscoped key would
	// match another session's prompt and answer this one with its replay.
	ns string
	// unsettled holds, per prompt text (by its hash), the key of every
	// prompt whose outcome is unknown: the request may have been accepted
	// but the answer was lost (a transport failure). A retry of the same
	// text reuses its idempotency key, so fleet recognises the input it
	// already accepted instead of running it a second time. Nothing is
	// evicted — forgetting a key would let its retry run twice — so the
	// memory is one small entry per prompt whose answer was lost, for the
	// life of the session.
	unsettled map[string]string
	// keyConv maps each unresolved key to the conversation it was first
	// submitted to ("" = it started the session's conversation). A retry
	// goes back there: fleet recognises a key only in the conversation that
	// accepted it (or, sent with no conversation, per user), so a retry
	// posted into a conversation created since would run the input again.
	keyConv map[string]string
}

// target returns the conversation a prompt under key is sent to: the one
// its key was first submitted to while it is unresolved, else the session's.
func (s *session) target(key string) string {
	if c, ok := s.keyConv[key]; ok {
		return c
	}
	return s.convID
}

// settle records whether key is still unresolved after a prompt: retained,
// it keeps the conversation it was first sent to; resolved, it is forgotten.
func (s *session) settle(key, conv string, retain bool) {
	if !retain {
		delete(s.keyConv, key)
		return
	}
	if s.keyConv == nil {
		s.keyConv = map[string]string{}
	}
	s.keyConv[key] = conv
}

// textKey is the unsettled map's key for a prompt's text: a fixed-size hash,
// so the entry stays small however long the prompt was.
func textKey(message string) string {
	sum := sha256.Sum256([]byte(message))
	return hex.EncodeToString(sum[:])
}

// clearUnsettled forgets message's retained key only if it is key: a later,
// different prompt with the same text (say, one carrying a messageId, so a
// different key) must not wipe an earlier prompt's pending retry key.
func (s *session) clearUnsettled(message, key string) {
	if s.unsettled[textKey(message)] == key {
		delete(s.unsettled, textKey(message))
	}
}

func (s *session) setUnsettled(message, key string) {
	if message == "" {
		return // not a text-keyed prompt (see promptOnce); prompt text is never empty
	}
	if s.unsettled == nil {
		s.unsettled = map[string]string{}
	}
	s.unsettled[textKey(message)] = key
}

var _ acpsdk.Agent = (*Agent)(nil)

// NewAgent builds the adapter. timeout bounds one turn (0 = no bound).
func NewAgent(client turnClient, cfgErr error, publicURL string, timeout time.Duration, version string) *Agent {
	return &Agent{
		client:    client,
		cfgErr:    cfgErr,
		publicURL: strings.TrimRight(publicURL, "/"),
		timeout:   timeout,
		version:   version,
		sessions:  map[acpsdk.SessionId]*session{},
	}
}

// SetConnection wires the connection session/update notifications go out on.
func (a *Agent) SetConnection(c updater) { a.conn = c }

// Initialize advertises only what is true: text prompts (plus the baseline
// resource_link, and embedded text resources), no session/load, no images or
// audio, no MCP servers from the client, no auth methods (identity is the
// operator-provisioned fleet user, resolved host-side from the environment).
func (a *Agent) Initialize(_ context.Context, _ acpsdk.InitializeRequest) (acpsdk.InitializeResponse, error) {
	title := "fleet"
	return acpsdk.InitializeResponse{
		ProtocolVersion: SpecVersion,
		AgentCapabilities: acpsdk.AgentCapabilities{
			LoadSession:        false,
			PromptCapabilities: acpsdk.PromptCapabilities{EmbeddedContext: true},
		},
		AgentInfo:   &acpsdk.Implementation{Name: "fleet", Title: &title, Version: a.version},
		AuthMethods: []acpsdk.AuthMethod{},
	}, nil
}

// Authenticate is never needed: Initialize advertises no auth methods.
func (a *Agent) Authenticate(context.Context, acpsdk.AuthenticateRequest) (acpsdk.AuthenticateResponse, error) {
	return acpsdk.AuthenticateResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodAuthenticate)
}

// NewSession opens an ACP session. Nothing is created in fleet yet — the
// conversation is born on the first prompt, like a new web chat. The client's
// cwd is recorded but not used: tool calls run in fleet's sandbox workspace,
// never in the client's filesystem. Client-supplied MCP servers are refused
// rather than ignored, because fleet's connectors come from the operator's
// bundle and are credential-brokered host-side.
func (a *Agent) NewSession(_ context.Context, p acpsdk.NewSessionRequest) (acpsdk.NewSessionResponse, error) {
	if a.cfgErr != nil {
		return acpsdk.NewSessionResponse{}, acpsdk.NewAuthRequired(map[string]any{"error": a.cfgErr.Error()})
	}
	if len(p.McpServers) > 0 {
		return acpsdk.NewSessionResponse{}, acpsdk.NewInvalidParams(map[string]any{
			"error": "fleet does not accept MCP servers from the ACP client: its connectors come from the operator's bundle and run host-side with brokered credentials",
		})
	}
	id := acpsdk.SessionId("fleet-acp-" + randomID())
	a.mu.Lock()
	a.sessions[id] = &session{cwd: p.Cwd, ns: randomID()}
	a.mu.Unlock()
	return acpsdk.NewSessionResponse{SessionId: id}, nil
}

// Cancel needs no work of its own: the SDK cancels the in-flight Prompt's
// context on session/cancel, and Prompt stops the fleet turn server-side when
// it sees that.
func (a *Agent) Cancel(context.Context, acpsdk.CancelNotification) error { return nil }

// CloseSession forgets the session. The fleet conversation stays, like any
// other chat, visible in the web UI.
func (a *Agent) CloseSession(_ context.Context, p acpsdk.CloseSessionRequest) (acpsdk.CloseSessionResponse, error) {
	a.mu.Lock()
	delete(a.sessions, p.SessionId)
	a.mu.Unlock()
	return acpsdk.CloseSessionResponse{}, nil
}

// Prompt runs one governed fleet turn and streams it back as session/update
// notifications.
func (a *Agent) Prompt(ctx context.Context, p acpsdk.PromptRequest) (acpsdk.PromptResponse, error) {
	if a.cfgErr != nil {
		return acpsdk.PromptResponse{}, acpsdk.NewAuthRequired(map[string]any{"error": a.cfgErr.Error()})
	}
	a.mu.Lock()
	sess := a.sessions[p.SessionId]
	a.mu.Unlock()
	if sess == nil {
		return acpsdk.PromptResponse{}, &acpsdk.RequestError{Code: -32002, Message: "Resource not found", Data: map[string]any{"sessionId": string(p.SessionId)}}
	}
	message, err := promptText(p.Prompt)
	if err != nil {
		return acpsdk.PromptResponse{}, acpsdk.NewInvalidParams(map[string]any{"error": err.Error()})
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	// A prompt cancelled while it waited for this session (an earlier prompt
	// still running) must never be submitted: once POSTed, fleet may start
	// the turn or dispatch a tool before any Stop could land.
	if ctx.Err() != nil {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled}, nil
	}
	key := idempotencyKey(p.MessageId, sess, message)
	return a.promptOnce(ctx, p, sess, message, key)
}

// promptOnce submits the prompt under one idempotency key and translates the
// outcome. A key fleet reports accepted and cancelled is never resubmitted on
// its own: fleet does not record why an input was cancelled, so a Stop (from
// any surface) and a launch that failed look the same, and resubmitting could
// run a message after its Stop succeeded. The user is told to send it again
// as a new message instead.
func (a *Agent) promptOnce(ctx context.Context, p acpsdk.PromptRequest, sess *session, message, key string) (acpsdk.PromptResponse, error) {
	// stopCtx ends when the client cancels (session/cancel, or a newer prompt
	// superseding this one) or the timeout fires. The stream itself runs on a
	// context that ignores both: a fleet turn is detached from its HTTP request
	// by design, so dropping the stream would not stop it. Instead stopTurn
	// stops the turn server-side first and only then ends the stream.
	stopCtx, cancelStop := ctx, context.CancelFunc(func() {})
	if a.timeout > 0 {
		stopCtx, cancelStop = context.WithTimeout(ctx, a.timeout)
	}
	defer cancelStop()
	streamCtx, cancelStream := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelStream()

	// Notifications must still go out while the turn is being cancelled, so
	// they are sent on a context that ignores the turn's cancellation.
	sendCtx := context.WithoutCancel(ctx)
	// The stop watcher and the submission must name the same conversation:
	// a retry of an unresolved key goes back to the one it was first sent to
	// (none, for a session's first prompt), not the session's newer one.
	target := sess.target(key)
	// Whether key was already unresolved (an earlier attempt's answer was
	// lost): a refusal of THIS attempt says nothing about that one.
	_, wasUnresolved := sess.keyConv[key]
	tr := newTranslator(p.SessionId, target, func(u acpsdk.SessionUpdate) {
		if a.conn != nil {
			_ = a.conn.SessionUpdate(sendCtx, acpsdk.SessionNotification{SessionId: p.SessionId, Update: u})
		}
	})

	streamDone := make(chan struct{})
	stopped := make(chan stopOutcome, 1)
	go a.stopTurn(stopCtx, tr, key, streamDone, stopped, cancelStream)
	convID, streamErr := a.client.StreamInput(streamCtx, message, target, key, tr.handle)
	close(streamDone)
	stop := <-stopped
	stopErr := stop.err
	if convID == "" {
		convID = tr.conversationID()
	}
	if convID == "" {
		convID = target
	}
	// The session's conversation is set by its first answered prompt only:
	// a retry sent back to an earlier conversation (keyConv) does not move
	// the session there.
	if sess.convID == "" {
		sess.convID = convID
	}
	// Only THIS prompt's entry is reconciled here; other unresolved prompts
	// keep their keys until they are retried and answered.
	// keep: the key must survive for a retry. An unknown outcome keeps it;
	// so does a retry of an already-unresolved key refused before fleet
	// looked it up (a 429 from the rate limiter, an auth hiccup), since that
	// refusal is definite only for this attempt, not the earlier one. A 409
	// is the exception: it is fleet's answer about this very input.
	keep := keepKey(streamErr, wasUnresolved)
	// Only a prompt whose key came from its text is remembered by its text:
	// a messageId prompt recovers through its own deterministic key, and
	// filing it under the text would hand that key to a later, different
	// text-only prompt with the same words (answering it as a replay).
	textMsg := message
	if p.MessageId != nil && strings.TrimSpace(*p.MessageId) != "" {
		textMsg = ""
	}
	sess.clearUnsettled(textMsg, key)
	if keep {
		sess.setUnsettled(textMsg, key)
	}

	meta := map[string]any{"fleet.conversationId": convID}
	var queued *chattui.QueuedError
	isQueued := errors.As(streamErr, &queued)
	if isQueued && (ctx.Err() != nil || stopCtx.Err() != nil) {
		stop.intervened = true
		// The watcher may already know the turn had ended; an acceptance that
		// needed no stop (queued.State cancelled) must not erase that.
		if accErr := a.stopAccepted(convID, key, queued); accErr != nil || !errors.Is(stopErr, chattui.ErrTurnNotRunning) {
			stopErr = accErr
		}
	}
	if outcomeUnknown(streamErr) && (ctx.Err() != nil || stopCtx.Err() != nil) && !stop.intervened {
		// Cancelled (or timed out) and the answer was lost before fleet said
		// what it did with this prompt: find it by key and stop it, so it
		// cannot run on after ACP reports it cancelled.
		stop.intervened = true
		stopErr = a.reconcileLost(convID, key)
	}
	if errors.Is(stopErr, chattui.ErrTurnNotRunning) {
		// The Stop by key found the input already finished: nothing was
		// stopped, and it is not an unconfirmed stop either.
		stopErr, stop.alreadyEnded = nil, true
	}
	sess.retainKey(textMsg, key, target, convID, keep, stopErr, queued)
	// A staged approval stays pending in fleet whatever ended the turn —
	// cancelled, timed out or errored included — so its pointer goes out
	// before any outcome.
	tr.flushApprovals(a.approvalPointer)
	switch {
	case ctx.Err() != nil:
		// ACP requires a cancelled prompt to answer with the cancelled stop
		// reason, not an error — so when fleet did not accept the Stop, the
		// client is told in the transcript that the turn may still be running,
		// rather than left believing it stopped.
		if stopErr != nil {
			tr.send(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
				"\n\nfleet could not confirm this turn stopped (%v). It may still be running: stop it at %s", stopErr, a.conversationPointer(convID))))
		} else if stop.alreadyEnded {
			// Still "cancelled", as ACP requires of a cancelled prompt, but
			// not a claim that anything was stopped: the turn had finished.
			tr.send(acpsdk.UpdateAgentMessageText(fmt.Sprintf(
				"\n\nThe turn had already finished before the Stop reached fleet, so nothing was stopped: what it did (tool calls included) stands. See %s", a.conversationPointer(convID))))
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Meta: meta}, nil
	case stop.intervened && stopCtx.Err() != nil && !stop.alreadyEnded:
		// A timeout whose Stop found the turn already complete falls through:
		// the turn finished, so its outcome is reported, not a timeout.
		if stopErr != nil {
			return acpsdk.PromptResponse{}, acpsdk.NewInternalError(map[string]any{
				"error": fmt.Sprintf("the fleet turn did not finish within %s, and stopping it failed (%v): it may still be running — stop it at %s", a.timeout, stopErr, a.conversationPointer(convID)),
			})
		}
		return acpsdk.PromptResponse{}, acpsdk.NewInternalError(map[string]any{
			"error": fmt.Sprintf("the fleet turn did not finish within %s and was stopped (raise it with fleet acp --timeout)", a.timeout),
		})
	}
	if tr.policyBlocked {
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonRefusal, Meta: meta}, nil
	}
	if isQueued {
		// An accepted prompt, not a failure — either fleet queued it behind a
		// turn already running in this conversation, or this is a resend of a
		// message fleet accepted earlier under the same key (the original is
		// not run again). Say which, and where to follow it, instead of
		// inviting a retry.
		tr.send(acpsdk.UpdateAgentMessageText(acceptedNote(queued, a.conversationPointer(convID))))
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: meta, UserMessageId: p.MessageId}, nil
	}
	if errors.Is(streamErr, context.Canceled) {
		// Stopped from another fleet surface (the web chat's Stop, where these
		// conversations are visible): the server's turn.cancelled is a normal
		// cancellation, not a failure.
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Meta: meta}, nil
	}
	var se *chattui.StatusError
	if errors.As(streamErr, &se) && se.Code == http.StatusConflict {
		// POST /chat answers 409 only when a Stop (from another surface)
		// cancelled this input before its turn started: nothing ran, and the
		// Stop succeeded.
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Meta: meta}, nil
	}
	if streamErr != nil {
		return acpsdk.PromptResponse{}, requestError(streamErr)
	}
	resp := acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: meta, UserMessageId: p.MessageId}
	if tr.usage != nil {
		resp.Usage = tr.usage
	}
	return resp, nil
}

// conversationWait bounds how long a cancelled prompt waits to learn its
// conversation and turn ids. A cancel can arrive before the stream's first frame (the
// `conversation` event) has been read; without the id there is nothing to
// stop, and the turn would run on server-side with no client.
const conversationWait = 5 * time.Second

// stopSettleWait bounds how long an accepted Stop waits for the turn's
// terminal frame, which says whether the Stop stopped it (turn.cancelled) or
// the turn had just completed. A var so tests can shorten it.
var stopSettleWait = 3 * time.Second

// turnGrace bounds the wait for turn.started once the conversation is known.
const turnGrace = time.Second

// stopOutcome is what the stop watcher did: intervened is true when it sent
// (or tried to send) a Stop, and err is that Stop's failure, if any.
// alreadyEnded means the Stop reached fleet after the turn had finished, so
// it stopped nothing and the stream was read to its real end.
type stopOutcome struct {
	intervened   bool
	alreadyEnded bool
	err          error
}

// stopTurn watches one prompt. If stop fires before the stream finishes, it
// waits (bounded) for the conversation id, stops the fleet turn server-side,
// then ends the stream. It reports what it did on stopped, so Prompt never
// reports a stop the server did not confirm.
//
// A turn the server already reported as over is never stopped: the Stop is
// conversation-scoped, so sending it after the watched turn ended could
// cancel a follow-up queued from another surface instead.
func (a *Agent) stopTurn(stop context.Context, tr *translator, key string, streamDone <-chan struct{}, stopped chan<- stopOutcome, cancelStream context.CancelFunc) {
	select {
	case <-streamDone:
		stopped <- stopOutcome{}
		return
	case <-stop.Done():
	}
	// Wait (bounded) to learn which turn to stop. fleet sends the
	// conversation frame and turn.started back to back, so once the
	// conversation is known the turn id follows within turnGrace or not at
	// all (an older server); either way the wait ends as soon as the turn is
	// over or the stream is gone.
	select {
	case <-tr.turnKnown:
	case <-tr.endedCh:
	case <-streamDone:
	case <-tr.convKnown:
		select {
		case <-tr.turnKnown:
		case <-tr.endedCh:
		case <-streamDone:
		case <-time.After(turnGrace):
		}
	case <-time.After(conversationWait):
	}
	if tr.ended() {
		// The turn's terminal frame arrived first: nothing left to stop, and
		// the turn's outcome (tool effects included) stands.
		cancelStream()
		stopped <- stopOutcome{alreadyEnded: true}
		return
	}
	var err error
	if id, turn := tr.conversationID(), tr.turnID(); id != "" && turn != "" {
		// Always targeted at the watched turn: the server refuses to cancel
		// any other turn, so the Stop cannot hit a successor that started
		// after the watched turn ended.
		err = a.client.Cancel(id, turn)
		if err == nil || errors.Is(err, chattui.ErrStopUnconfirmed) {
			// Accepted — but the turn may have finished in the instant
			// between fleet's check and its cancel, which then stops
			// nothing. Read on to the terminal frame (bounded) and trust it:
			// turn.cancelled is a real stop; any other end (completed,
			// failed, model-required) is the turn's own, so what it did
			// stands.
			select {
			case <-tr.endedCh:
			case <-streamDone:
			case <-time.After(stopSettleWait):
			}
			if errors.Is(err, chattui.ErrStopUnconfirmed) && tr.cancelledTurn() {
				err = nil // the turn's own terminal frame confirms the stop
			}
			if tr.endedOnItsOwn() {
				// The terminal frame is in; let the stream finish on its own
				// (briefly), so the prompt reports the turn's own outcome
				// rather than a stream it cut short.
				select {
				case <-streamDone:
				case <-time.After(stopSettleWait):
					cancelStream()
				}
				stopped <- stopOutcome{intervened: true, alreadyEnded: true}
				return
			}
		}
		if errors.Is(err, chattui.ErrTurnNotRunning) {
			// The turn ended between the check above and the Stop landing:
			// nothing was stopped. Keep reading, so the prompt reports how
			// the turn actually ended rather than calling it cancelled.
			select {
			case <-streamDone:
				stopped <- stopOutcome{alreadyEnded: true}
				return
			case <-tr.endedCh:
				// The terminal frame is the answer; the stream can stay open
				// a while longer for post-turn work (auto-titling), so let
				// it finish only briefly, as for a completed turn above.
				select {
				case <-streamDone:
				case <-time.After(stopSettleWait):
					cancelStream()
				}
				stopped <- stopOutcome{alreadyEnded: true}
				return
			case <-time.After(conversationWait):
				err = errors.New("the turn had already ended, but its final answer did not arrive")
			}
		}
	} else if id != "" {
		// Never an untargeted Stop: "whichever turn is running" could be a
		// successor by the time it lands. The answer has not named a turn
		// (it may be slow, lost, or a queue acknowledgement), so stop reading
		// and find this prompt by its key instead: a turn started for this
		// submission gets a targeted Stop, a queue row with this key is
		// withdrawn.
		cancelStream()
		err = a.reconcileLost(id, key) // ErrTurnNotRunning: already finished (promptOnce says so)
	} else {
		select {
		case <-streamDone:
			// The response ended before naming a conversation. That proves
			// only that the answer stopped, not that fleet ran nothing: what
			// the answer was decides (promptOnce reconciles an unknown
			// outcome, which with no conversation to name is unconfirmed; an
			// acceptance is stopped by its key; a refusal needs nothing).
			stopped <- stopOutcome{}
			return
		default:
			err = errors.New("fleet never reported the conversation id, so there was nothing to address the Stop to")
		}
	}
	cancelStream()
	stopped <- stopOutcome{intervened: true, err: err}
}

// idempotencyKey is the input_id for one prompt. The ACP client's own message
// id (stable across its retries) wins; otherwise a retry of the prompt whose
// outcome was lost reuses that prompt's key; otherwise a fresh key.
func idempotencyKey(messageID *string, sess *session, message string) string {
	if messageID != nil && strings.TrimSpace(*messageID) != "" {
		// Opaque: trimming decides only whether an id was sent. " job-1 "
		// and "job-1" are different messages.
		// Hashed: a client may send a messageId of any length, and the key
		// lands in a btree index with a size limit. The hash keeps it
		// deterministic, so a resend of the same messageId finds the run.
		sum := sha256.Sum256([]byte(*messageID))
		return "acp-msg-" + sess.ns + "-" + hex.EncodeToString(sum[:])
	}
	if k, ok := sess.unsettled[textKey(message)]; ok {
		return k
	}
	return "fleet-acp-" + randomID()
}

// outcomeUnknown reports whether a failed submission may still have been
// accepted by fleet: a transport failure (the POST may have landed), as
// opposed to a definite answer — a refusal status, a queue acknowledgement,
// a terminal turn error, or success.
func outcomeUnknown(err error) bool {
	if err == nil {
		return false
	}
	var se *chattui.StatusError
	var qe *chattui.QueuedError
	if errors.As(err, &se) {
		// A 4xx is a definite refusal. A 5xx is not: the server may have
		// committed the input and then failed to say so (a commit whose
		// acknowledgement was lost), so the key is kept for a retry.
		return se.Code >= http.StatusInternalServerError
	}
	if errors.As(err, &qe) || errors.Is(err, context.Canceled) {
		return false
	}
	msg := err.Error()
	return !strings.HasPrefix(msg, "turn failed:") && !strings.HasPrefix(msg, "turn requires another model:")
}

// retainKey records, after a prompt, what a later retry of key needs. An
// unconfirmed stop keeps the key for the text (the original may still run,
// so a retry must reuse the key and be answered with that run, never start
// a second one); keep or an unconfirmed stop keeps its conversation; and an
// input accepted and still live (queued may be nil) stays routed to the
// conversation that holds it — a replay may name one other than the
// session's — so a later resend or a Stop by key goes where fleet can find
// the input.
func (s *session) retainKey(message, key, target, convID string, keep bool, stopErr error, queued *chattui.QueuedError) {
	if stopErr != nil {
		s.setUnsettled(message, key)
	}
	s.settle(key, target, keep || stopErr != nil)
	if queued != nil && convID != "" && (queued.State == "running" || queued.State == "queued" || queued.State == "injected") {
		s.settle(key, convID, true)
	}
}

// keepKey reports whether key must survive for a retry: an unknown outcome
// keeps it, and so does a retry of an already-unresolved key refused before
// fleet looked it up (refusedAttempt).
func keepKey(streamErr error, wasUnresolved bool) bool {
	return outcomeUnknown(streamErr) || (wasUnresolved && refusedAttempt(streamErr))
}

// refusedAttempt reports a 4xx other than 409: this attempt was refused,
// which proves nothing about an earlier attempt of the same key. A 409 is
// fleet's answer about the input itself (a Stop cancelled it).
func refusedAttempt(err error) bool {
	var se *chattui.StatusError
	return errors.As(err, &se) && se.Code >= http.StatusBadRequest && se.Code < http.StatusInternalServerError && se.Code != http.StatusConflict
}

// stopAccepted handles a cancel (or timeout) whose answer was an acceptance
// rather than a stream: a fresh queue item, or a resend of a message fleet
// accepted earlier under the same key. Anything not yet over (queued, running,
// injected into a running turn) is stopped by its key, which fleet resolves to
// wherever the input is; a completed or never-run input needs nothing.
func (a *Agent) stopAccepted(convID, key string, q *chattui.QueuedError) error {
	switch q.State {
	case "completed":
		return chattui.ErrTurnNotRunning // it had already run: nothing to stop
	case "cancelled":
		return nil // it never ran, and will not
	}
	if err := a.client.CancelInput(convID, key); err != nil {
		if errors.Is(err, chattui.ErrTurnNotRunning) || errors.Is(err, chattui.ErrStopUnconfirmed) {
			return err // already finished, or not yet confirmed: the caller says which
		}
		return fmt.Errorf("the message was accepted and could not be stopped: %w", err)
	}
	return nil
}

// reconcileLost stops a prompt whose answer was lost, found by its key
// without resubmitting it. fleet does the finding (a Stop naming the input_id):
// a still-queued row is withdrawn, a running turn for the key is cancelled,
// and a turn that has not registered yet (a direct claim still being
// prepared, a row just claimed, a request still in transit) is refused when
// it tries — atomically with registration, so there is no gap between
// looking and stopping. With no conversation known there is nothing to name,
// and the stop is reported as unconfirmed.
func (a *Agent) reconcileLost(convID, key string) error {
	if convID == "" {
		return errors.New("fleet never reported the conversation, so the lost prompt cannot be found to stop")
	}
	return a.client.CancelInput(convID, key)
}

// acceptedNote tells the ACP user what became of a prompt fleet accepted
// without streaming it: queued behind a running turn, or a resend of a message
// accepted earlier under the same key (never run twice).
func acceptedNote(q *chattui.QueuedError, where string) string {
	switch {
	case q.Replayed() && (q.State == "running" || q.State == "injected"):
		return "fleet is already running this message from an earlier attempt (it is not run twice). Follow it at " + where
	case q.Replayed() && q.State == "completed":
		return "fleet already ran this message from an earlier attempt (it is not run twice). Its reply is in " + where
	case q.Replayed() && q.State == "cancelled":
		return "an earlier attempt of this message was cancelled (stopped, or it failed before it started), so it did not run. To run it, send it again as a new message."
	case q.Replayed():
		return fmt.Sprintf("this message is already queued from an earlier attempt (position %d). Follow it at %s", q.Position, where)
	default:
		return fmt.Sprintf("fleet is already running a turn in this conversation, so your message was queued (position %d) and will run after it. Follow it at %s", q.Position, where)
	}
}

// conversationPointer says where a person can see (and stop) a conversation.
func (a *Agent) conversationPointer(convID string) string {
	switch {
	case convID == "":
		return "the fleet web chat"
	case a.publicURL != "":
		return a.publicURL + "/chat?c=" + url.QueryEscape(convID)
	default:
		return "the fleet web chat (conversation " + convID + ")"
	}
}

// approvalPointer tells the ACP user where to settle a staged approval.
// Approvals stay in fleet (the default-deny card is the control); ACP's
// session/request_permission is deliberately not used to reimplement it.
func (a *Agent) approvalPointer(convID, approvalID, tool string) string {
	if tool == previewEmailTool {
		// A display-only card: there is nothing to allow or deny, only a draft
		// to look at (its one action is Dismiss).
		return fmt.Sprintf("\n\nA draft email preview is open in fleet (nothing was sent, and no approval is needed): %s", a.conversationPointer(convID))
	}
	where := "fleet chat --conversation " + convID + " --approve " + approvalID + "   (or --deny)"
	if a.publicURL != "" {
		where = a.publicURL + "/chat?c=" + url.QueryEscape(convID)
	}
	return fmt.Sprintf("\n\nApproval needed in fleet: %s is waiting for a person to allow or deny it. Review it at %s", orDefault(tool, "a tool call"), where)
}

// Unsupported methods. Initialize does not advertise them, so a conforming
// client never calls them; if one does, it gets a clear method-not-found.

func (a *Agent) Logout(context.Context, acpsdk.LogoutRequest) (acpsdk.LogoutResponse, error) {
	return acpsdk.LogoutResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodLogout)
}

func (a *Agent) ListSessions(context.Context, acpsdk.ListSessionsRequest) (acpsdk.ListSessionsResponse, error) {
	return acpsdk.ListSessionsResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionList)
}

func (a *Agent) ResumeSession(context.Context, acpsdk.ResumeSessionRequest) (acpsdk.ResumeSessionResponse, error) {
	return acpsdk.ResumeSessionResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionResume)
}

func (a *Agent) SetSessionConfigOption(context.Context, acpsdk.SetSessionConfigOptionRequest) (acpsdk.SetSessionConfigOptionResponse, error) {
	return acpsdk.SetSessionConfigOptionResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionSetConfigOption)
}

func (a *Agent) SetSessionMode(context.Context, acpsdk.SetSessionModeRequest) (acpsdk.SetSessionModeResponse, error) {
	return acpsdk.SetSessionModeResponse{}, acpsdk.NewMethodNotFound(acpsdk.AgentMethodSessionSetMode)
}

// requestError maps a failed turn onto a JSON-RPC error the client can show.
// An auth failure (401/403 from POST /chat) is ACP's auth_required; anything
// else — server unreachable, turn.error, turn.model_required — is an internal
// error whose message is the same actionable text `fleet chat` prints.
func requestError(err error) error {
	var se *chattui.StatusError
	if errors.As(err, &se) && (se.Code == 401 || se.Code == 403) {
		return acpsdk.NewAuthRequired(map[string]any{"error": se.Error()})
	}
	return acpsdk.NewInternalError(map[string]any{"error": err.Error()})
}

// promptText flattens an ACP prompt into the one message a fleet turn takes.
// Text blocks are joined; a resource_link becomes a Markdown link and an
// embedded text resource is inlined under its URI, so the model sees what the
// user attached. Images, audio and binary blobs are refused (not advertised).
func promptText(blocks []acpsdk.ContentBlock) (string, error) {
	var parts []string
	for _, b := range blocks {
		switch {
		case b.Text != nil:
			parts = append(parts, b.Text.Text)
		case b.ResourceLink != nil:
			name := orDefault(b.ResourceLink.Name, b.ResourceLink.Uri)
			parts = append(parts, fmt.Sprintf("[%s](%s)", name, b.ResourceLink.Uri))
		case b.Resource != nil && b.Resource.Resource.TextResourceContents != nil:
			r := b.Resource.Resource.TextResourceContents
			parts = append(parts, fmt.Sprintf("Contents of %s:\n```\n%s\n```", r.Uri, r.Text))
		case b.Image != nil:
			return "", errors.New("fleet acp does not accept image content (promptCapabilities.image is false)")
		case b.Audio != nil:
			return "", errors.New("fleet acp does not accept audio content (promptCapabilities.audio is false)")
		default:
			return "", errors.New("fleet acp accepts text, resource_link and embedded text resources only")
		}
	}
	msg := strings.Join(parts, "\n\n")
	if strings.TrimSpace(msg) == "" { // trimmed only to test for emptiness: the text is sent exactly
		return "", errors.New("empty prompt")
	}
	return msg, nil
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
