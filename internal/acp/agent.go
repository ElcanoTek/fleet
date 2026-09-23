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
	"encoding/hex"
	"errors"
	"fmt"
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
	Stream(ctx context.Context, message, convID string, onEvent func(chattui.Event)) (string, error)
	Cancel(convID string) error
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
	a.sessions[id] = &session{cwd: p.Cwd}
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
	tr := newTranslator(p.SessionId, sess.convID, func(u acpsdk.SessionUpdate) {
		if a.conn != nil {
			_ = a.conn.SessionUpdate(sendCtx, acpsdk.SessionNotification{SessionId: p.SessionId, Update: u})
		}
	})

	streamDone := make(chan struct{})
	stopped := make(chan stopOutcome, 1)
	go a.stopTurn(stopCtx, tr, streamDone, stopped, cancelStream)
	convID, streamErr := a.client.Stream(streamCtx, message, sess.convID, tr.handle)
	close(streamDone)
	stop := <-stopped
	stopErr := stop.err
	if convID == "" {
		convID = tr.conversationID()
	}
	sess.convID = convID

	meta := map[string]any{"fleet.conversationId": convID}
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
		}
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Meta: meta}, nil
	case stop.intervened && stopCtx.Err() != nil:
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
	if errors.Is(streamErr, context.Canceled) {
		// Stopped from another fleet surface (the web chat's Stop, where these
		// conversations are visible): the server's turn.cancelled is a normal
		// cancellation, not a failure.
		return acpsdk.PromptResponse{StopReason: acpsdk.StopReasonCancelled, Meta: meta}, nil
	}
	if streamErr != nil {
		return acpsdk.PromptResponse{}, requestError(streamErr)
	}
	resp := acpsdk.PromptResponse{StopReason: acpsdk.StopReasonEndTurn, Meta: meta}
	if tr.usage != nil {
		resp.Usage = tr.usage
	}
	return resp, nil
}

// conversationWait bounds how long a cancelled prompt waits to learn its
// conversation id. A cancel can arrive before the stream's first frame (the
// `conversation` event) has been read; without the id there is nothing to
// stop, and the turn would run on server-side with no client.
const conversationWait = 5 * time.Second

// stopOutcome is what the stop watcher did: intervened is true when it sent
// (or tried to send) a Stop, and err is that Stop's failure, if any.
type stopOutcome struct {
	intervened bool
	err        error
}

// stopTurn watches one prompt. If stop fires before the stream finishes, it
// waits (bounded) for the conversation id, stops the fleet turn server-side,
// then ends the stream. It reports what it did on stopped, so Prompt never
// reports a stop the server did not confirm.
//
// A turn the server already reported as over is never stopped: the Stop is
// conversation-scoped, so sending it after the watched turn ended could
// cancel a follow-up queued from another surface instead.
func (a *Agent) stopTurn(stop context.Context, tr *translator, streamDone <-chan struct{}, stopped chan<- stopOutcome, cancelStream context.CancelFunc) {
	select {
	case <-streamDone:
		stopped <- stopOutcome{}
		return
	case <-stop.Done():
	}
	select {
	case <-tr.convKnown:
	case <-streamDone:
	case <-time.After(conversationWait):
	}
	if tr.ended() {
		cancelStream() // nothing left to stop; just stop reading
		stopped <- stopOutcome{}
		return
	}
	var err error
	if id := tr.conversationID(); id != "" {
		err = a.client.Cancel(id)
	} else {
		select {
		case <-streamDone: // the request ended before fleet started a turn
		default:
			err = errors.New("fleet never reported the conversation id, so there was nothing to address the Stop to")
		}
	}
	cancelStream()
	stopped <- stopOutcome{intervened: true, err: err}
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
	msg := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if msg == "" {
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
