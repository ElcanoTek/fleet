package acpingress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// IngressAgent is fleet's ACP AGENT face (acp.Agent): an external host (Zed /
// Neovim / any ACP editor) launches `fleet acp` and drives fleet's OWN governed,
// sandboxed pipeline over stdio. Each Prompt runs the SAME governed interactive
// turn the web path runs, via agent.Manager.RunTurn — so the turn inherits the
// configured runtime flavor + full governance (policy / sandbox / MCP / notes /
// audit / cost-ceilings) verbatim. See the package doc for the cardinal rule.
type IngressAgent struct {
	engine TurnEngine
	store  ConversationStore

	// approvals + runner back the human-in-the-loop approval surface.
	approvals ApprovalStore
	runner    StagedToolRunner

	cfg Config

	conn *acp.AgentSideConnection

	mu       sync.Mutex
	sessions map[string]*ingressSession
}

// Config tunes an IngressAgent. Zero values are sensible defaults.
type Config struct {
	// Principal is the audit identity ingress sessions bind to (see Principal).
	Principal Principal
	// Persona names the persona ingress turns use. Empty uses the engine's
	// configured default (Manager falls back to config.PersonaDefault).
	Persona string
	// Model is the OpenRouter slug ingress turns run against. Required: the
	// engine holds no default model, so a blank model fails the turn up-front.
	// Operators set this to the model `fleet acp` should drive.
	Model string
	// Runtime selects the execution flavor by clientconfig name ("" = the
	// bundle default; "native-inprocess"; "native-acp"). Inherited by the turn,
	// so "fleet runs in a sandbox" holds for ingress for free.
	Runtime string
	// Lockdown, when true, marks every ingress conversation locked-down and forces
	// each turn into the sealed, no-network per-turn sandbox (mirroring the web
	// path, which ORs req.Lockdown with the server's LockdownOnly and threads
	// conv.Lockdown into the turn). cmd/fleet/acp.go ORs FLEET_ACP_LOCKDOWN with
	// the server's CHAT_LOCKDOWN_ONLY to populate this, so a LockdownOnly server is
	// never silently network-enabled when an editor connects. The model is
	// validated against the lockdown allow-list up-front there.
	Lockdown bool
	// PermissionTimeout caps how long a staged approval waits for the human over
	// ACP before defaulting to DENY. Zero uses DefaultPermissionTimeout.
	PermissionTimeout time.Duration
	// Version is advertised in AgentInfo.
	Version string
}

// ingressSession is the per-ACP-session state: the bound fleet conversation +
// the run's cancel hook so Cancel can stop an in-flight Prompt.
type ingressSession struct {
	conversationID string
	cwd            string

	mu     sync.Mutex
	cancel context.CancelFunc
}

var _ acp.Agent = (*IngressAgent)(nil)

// New builds an IngressAgent. engine + store + approvals + runner are the
// production seams (all satisfied by *agent.Manager / *store.Store). cfg.Model
// must be set.
func New(engine TurnEngine, st ConversationStore, approvals ApprovalStore, runner StagedToolRunner, cfg Config) *IngressAgent {
	if cfg.Principal.Email == "" {
		cfg.Principal.Email = DefaultPrincipalEmail
	}
	if cfg.Version == "" {
		cfg.Version = "p-acp-3"
	}
	return &IngressAgent{
		engine:    engine,
		store:     st,
		approvals: approvals,
		runner:    runner,
		cfg:       cfg,
		sessions:  map[string]*ingressSession{},
	}
}

// SetAgentConnection wires the agent-side connection after construction (the SDK
// AgentConnAware pattern). The streaming sink + the outbound permission surface
// both call back to the client through it.
func (a *IngressAgent) SetAgentConnection(conn *acp.AgentSideConnection) { a.conn = conn }

// Initialize advertises fleet's agent capabilities. Prompt: text only (no image
// prompt blocks yet, no loadSession). AgentInfo names us "fleet".
func (a *IngressAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			// loadSession intentionally false for now — ingress is single-turn
			// streaming; resuming a prior session is a later phase.
			LoadSession:        false,
			PromptCapabilities: acp.PromptCapabilities{},
		},
		AgentInfo: &acp.Implementation{Name: "fleet", Version: a.cfg.Version},
	}, nil
}

// NewSession creates the fleet conversation bound to this ACP session and
// captures the editor's cwd. The host-advertised mcpServers are INTENTIONALLY
// IGNORED — fleet brokers its own client-config MCP catalog host-side (see the
// package doc). The conversation persists through the normal store path so
// audit / notes / history work exactly as the web path's.
func (a *IngressAgent) NewSession(ctx context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if a.cfg.Model == "" {
		return acp.NewSessionResponse{}, errors.New("fleet acp: no model configured (set the ingress model)")
	}
	conv, err := a.store.CreateConversation(ctx, a.cfg.Principal.Email, "", a.cfg.Persona, a.cfg.Model, a.cfg.Lockdown)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("create ingress conversation: %w", err)
	}

	sid := newSessionID()
	a.mu.Lock()
	a.sessions[sid] = &ingressSession{conversationID: conv.ID, cwd: p.Cwd}
	a.mu.Unlock()

	log.Printf("acpingress: new session %s → conversation %s (cwd=%s, model=%s, runtime=%q, lockdown=%t, principal=%s)",
		sid, conv.ID, p.Cwd, a.cfg.Model, a.cfg.Runtime, a.cfg.Lockdown, a.cfg.Principal.Email)
	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

// Prompt runs ONE governed interactive turn for the session. It loads the
// conversation's history, assembles the TurnInput (with the ingress streaming
// sink + the ingress approval surface), drives the SAME engine the web path
// drives, persists the new history, and maps the result onto an ACP StopReason.
func (a *IngressAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(p.SessionId)
	a.mu.Lock()
	sess, ok := a.sessions[sid]
	a.mu.Unlock()
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
	}

	// Per-turn cancellable ctx so Cancel can stop this run mid-flight.
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sess.setCancel(cancel)
	defer sess.clearCancel()

	userText := promptText(p.Prompt)

	history, err := a.store.LoadHistory(turnCtx, sess.conversationID)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("load history: %w", err)
	}

	sink := newIngressSink(a.conn, p.SessionId)
	approver := &ingressApprover{
		conn:           a.conn,
		sessionID:      p.SessionId,
		store:          a.approvals,
		runner:         a.runner,
		conversationID: sess.conversationID,
		userEmail:      a.cfg.Principal.Email,
		permTimeout:    a.cfg.PermissionTimeout,
	}

	in := agent.TurnInput{
		UserMessage:    userText,
		Persona:        a.cfg.Persona,
		Model:          a.cfg.Model,
		History:        history,
		ConversationID: sess.conversationID,
		Runtime:        a.cfg.Runtime,
		// Force the sealed, no-network per-turn sandbox when this session is
		// locked down (mirrors the web path threading conv.Lockdown into the
		// turn). The engine re-checks the model against the lockdown allow-list.
		Lockdown: a.cfg.Lockdown,
		// The human-in-the-loop approval surface: a staged critical tool routes
		// to the editor over OUTBOUND request_permission (default-DENY). The
		// other staging surfaces are handled per the approver's documented
		// choices (propose_memory unwired; propose_note inherited host-side).
		ApprovalStager: approver,
		// PermissionBroker is the EXTERNAL-acp human surface, irrelevant here:
		// ingress drives the native/governed flavors, whose human-in-the-loop is
		// the ApprovalStager above. Left nil.
	}

	res, runErr := a.engine.RunTurn(turnCtx, in, sink)
	if runErr != nil {
		// A model-selection failure (errors.Is ErrModelSelectionRequired) or any
		// other stream failure: surface as a refusal stop so the editor shows the
		// turn ended without a fabricated reply. Cancellation is handled below.
		if turnCtx.Err() != nil {
			a.persist(sess.conversationID, partialHistory(res))
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		if errors.Is(runErr, agent.ErrModelSelectionRequired) {
			log.Printf("acpingress: turn failed (model selection required): %v", runErr)
			return acp.PromptResponse{StopReason: acp.StopReasonRefusal}, nil
		}
		return acp.PromptResponse{}, fmt.Errorf("run turn: %w", runErr)
	}

	// Persist the turn's history (the SAME shape the web path persists) so audit
	// / notes / next-turn replay work. This includes any in-loop "APPROVAL_REQUIRED"
	// tool_result the policy emitted — persisted FIRST so the post-turn resolution
	// row lands after it (mirroring the web path, where the turn persists with the
	// block message and the approval handler appends the real outcome later;
	// replayHistory's dedup then prefers the real outcome on the next turn).
	a.persist(sess.conversationID, res.NewHistory)

	// Resolve any critical-tool approvals the turn staged: ask the human over ACP
	// (request_permission), execute approved tools through the governed staged-tool
	// runner, stream the outcome back, and append the resolution to history. This
	// runs AFTER the run loop (so it holds no orchestration lock) and honors the
	// turn ctx (a session Cancel default-denies an in-flight request).
	approver.ResolvePending(turnCtx, sink)

	if res.Cancelled {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

// Cancel cancels the session's in-flight run, if any. A cancelled run returns
// StopReasonCancelled with the partial transcript persisted (see Prompt).
func (a *IngressAgent) Cancel(_ context.Context, p acp.CancelNotification) error {
	a.mu.Lock()
	sess, ok := a.sessions[string(p.SessionId)]
	a.mu.Unlock()
	if ok {
		sess.cancelRun()
	}
	return nil
}

// persist writes the turn's history with a fresh, bounded context (the prompt
// ctx may already be cancelled). A persistence failure is logged, not fatal.
func (a *IngressAgent) persist(convID string, entries []agent.HistoryEntry) {
	if len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if err := a.store.AppendHistory(ctx, convID, entries); err != nil {
		log.Printf("acpingress: persist history (conv=%s): %v", convID, err)
	}
}

// partialHistory returns the partial transcript from a cancelled/errored result.
func partialHistory(res *agent.TurnResult) []agent.HistoryEntry {
	if res == nil {
		return nil
	}
	return res.NewHistory
}

func (s *ingressSession) setCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
}

func (s *ingressSession) clearCancel() {
	s.mu.Lock()
	s.cancel = nil
	s.mu.Unlock()
}

func (s *ingressSession) cancelRun() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// promptText flattens the ACP prompt content blocks to the user's turn text.
func promptText(blocks []acp.ContentBlock) string {
	var out string
	for _, b := range blocks {
		if b.Text != nil {
			out += b.Text.Text
		}
	}
	return out
}

func newSessionID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "acp-ingress-" + hex.EncodeToString(b[:])
}

// --- unsupported acp.Agent methods (ingress is single-turn streaming) ---

func (a *IngressAgent) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	// Local-process trust: launching `fleet acp` already implies box-user trust,
	// so there is no auth method to run. Return success (no-op).
	return acp.AuthenticateResponse{}, nil
}

func (a *IngressAgent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (a *IngressAgent) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}

func (a *IngressAgent) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (a *IngressAgent) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *IngressAgent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (a *IngressAgent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}
