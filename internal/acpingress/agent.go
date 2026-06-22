package acpingress

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/tools"
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

// Initialize advertises fleet's agent capabilities. Prompt accepts text and image
// content blocks (the governed turn supports vision). loadSession + resume are
// advertised: the SessionId IS the durable conversation ID, so an editor can
// reconnect to a prior conversation across `fleet acp` restarts (LoadSession
// replays the persisted transcript; ResumeSession rebinds without replay).
func (a *IngressAgent) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			// loadSession/resume: the conversation row is the durable binding, so a
			// reconnect rehydrates from the store (see LoadSession/ResumeSession).
			LoadSession:         true,
			SessionCapabilities: acp.SessionCapabilities{Resume: &acp.SessionResumeCapabilities{}},
			// Image: the editor may attach images; we decode them to the
			// conversation workspace and feed them to the governed turn as vision
			// input (see decodeImageBlocks). Audio stays unset (not consumed).
			PromptCapabilities: acp.PromptCapabilities{Image: true},
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

	// The SessionId IS the conversation ID — the one durable, principal-scoped key.
	// We still record the in-mem session for the live cancel hook; a reconnect after
	// a restart rehydrates it from the store via getOrLoadSession.
	a.mu.Lock()
	a.sessions[conv.ID] = &ingressSession{conversationID: conv.ID, cwd: p.Cwd}
	a.mu.Unlock()

	log.Printf("acpingress: new session %s (cwd=%s, model=%s, runtime=%q, lockdown=%t, principal=%s)",
		conv.ID, p.Cwd, a.cfg.Model, a.cfg.Runtime, a.cfg.Lockdown, a.cfg.Principal.Email)
	return acp.NewSessionResponse{SessionId: acp.SessionId(conv.ID)}, nil
}

// getOrLoadSession returns the in-memory session for sid, else rehydrates it from
// the store (the conversation row is the durable binding) — so a Prompt after a
// `fleet acp` restart finds its conversation instead of cold-starting. Returns an
// error when no conversation with that id exists for the bound principal.
func (a *IngressAgent) getOrLoadSession(ctx context.Context, sid string) (*ingressSession, error) {
	a.mu.Lock()
	sess, ok := a.sessions[sid]
	a.mu.Unlock()
	if ok {
		return sess, nil
	}
	conv, err := a.store.Get(ctx, a.cfg.Principal.Email, sid)
	if err != nil {
		return nil, fmt.Errorf("load session %s: %w", sid, err)
	}
	if conv == nil {
		return nil, fmt.Errorf("session %s not found", sid)
	}
	sess = &ingressSession{conversationID: conv.ID}
	a.mu.Lock()
	a.sessions[conv.ID] = sess
	a.mu.Unlock()
	return sess, nil
}

// Prompt runs ONE governed interactive turn for the session. It loads the
// conversation's history, assembles the TurnInput (with the ingress streaming
// sink + the ingress approval surface), drives the SAME engine the web path
// drives, persists the new history, and maps the result onto an ACP StopReason.
func (a *IngressAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sess, err := a.getOrLoadSession(ctx, string(p.SessionId))
	if err != nil {
		return acp.PromptResponse{}, err
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
		// Decode any image prompt blocks the editor attached to per-conversation
		// workspace files and feed them to the turn as vision input — the SAME
		// TurnInput.ImageAttachments the web path populates. Text-only prompts yield
		// nil. Decode/cap failures are logged and dropped, never fatal.
		ImageAttachments: a.decodeImageBlocks(sess.conversationID, p.Prompt),
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

// imageBlock caps mirror loadImageAttachments (internal/agent): at most 8 images,
// 8 MiB each. We bound at decode time so a hostile/large prompt can't fill the
// workspace before the turn even runs.
const (
	maxIngressImages       = 8
	maxIngressImageBytes   = 8 * 1024 * 1024
	defaultIngressImageExt = ".png"
)

// extForImageMIME maps a VALIDATED image MIME to a safe file extension. We key on
// the validated MIME (never on the raw, editor-supplied MimeType string, which
// could carry path separators) — this writer is the sole trust boundary, since
// loadImageAttachments does not re-validate the path.
func extForImageMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return defaultIngressImageExt
	}
}

// decodeImageBlocks turns the editor's ACP image content blocks into governed
// vision input: each base64 block is decoded and written to the conversation
// workspace (already mounted into the sandbox, conversation-scoped) under an
// agent-uninfluenced basename, and returned as an agent.ImageAttachment for the
// turn. Non-image / oversized / undecodable blocks are dropped non-fatally so a
// bad block never fails the turn (the silent-degrade contract). Text blocks are
// ignored here (promptText handles them).
func (a *IngressAgent) decodeImageBlocks(convID string, blocks []acp.ContentBlock) []agent.ImageAttachment {
	var out []agent.ImageAttachment
	var dir string
	for _, b := range blocks {
		if b.Image == nil {
			continue
		}
		if len(out) >= maxIngressImages {
			log.Printf("acpingress: dropping image block (over %d-image cap, conv=%s)", maxIngressImages, convID)
			continue
		}
		// Resolve + validate the MIME: prefer the declared MimeType, else the
		// extension fallback; skip anything that isn't a supported image type.
		mime := strings.TrimSpace(b.Image.MimeType)
		if !tools.IsImageMIME(mime) {
			log.Printf("acpingress: dropping non-image prompt block (mime=%q, conv=%s)", b.Image.MimeType, convID)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(b.Image.Data)
		if err != nil {
			log.Printf("acpingress: dropping image block (base64 decode: %v, conv=%s)", err, convID)
			continue
		}
		if len(data) > maxIngressImageBytes {
			log.Printf("acpingress: dropping image block (%d bytes > %d cap, conv=%s)", len(data), maxIngressImageBytes, convID)
			continue
		}
		if dir == "" {
			d, err := tools.EnsureWorkspaceDir(convID)
			if err != nil {
				log.Printf("acpingress: cannot ensure workspace dir for images (conv=%s): %v", convID, err)
				return out
			}
			dir = d
		}
		name := fmt.Sprintf("acp-image-%d%s", len(out)+1, extForImageMIME(mime))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // image written into the conversation-scoped workspace dir under an agent-uninfluenced basename
			log.Printf("acpingress: write image %s (conv=%s): %v", name, convID, err)
			continue
		}
		out = append(out, agent.ImageAttachment{Path: path, MediaType: mime, Name: name})
	}
	return out
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

// LoadSession rebinds an ACP session to its durable fleet conversation (the
// SessionId IS the conversation ID) and replays the persisted transcript to the
// editor, so reconnecting after a `fleet acp` restart resumes the same governed
// conversation with its history rendered. It runs NO turn and executes NO tools —
// it is a read-only rehydrate + replay against the SAME store the web path uses.
// The host-advertised mcpServers are IGNORED (as in NewSession): fleet brokers
// its own catalog host-side; no editor credential path opens here.
func (a *IngressAgent) LoadSession(ctx context.Context, p acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	sess, err := a.getOrLoadSession(ctx, string(p.SessionId))
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	history, err := a.store.LoadHistory(ctx, sess.conversationID)
	if err != nil {
		return acp.LoadSessionResponse{}, fmt.Errorf("load history: %w", err)
	}
	newIngressSink(a.conn, p.SessionId).replayToEditor(history)
	return acp.LoadSessionResponse{}, nil
}

// ResumeSession rebinds the session WITHOUT replaying history (the spec contract:
// only loadSession replays). A follow-up Prompt then continues the conversation
// with full persisted context. Same read-only rehydrate as LoadSession.
func (a *IngressAgent) ResumeSession(ctx context.Context, p acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	if _, err := a.getOrLoadSession(ctx, string(p.SessionId)); err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	return acp.ResumeSessionResponse{}, nil
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

func (a *IngressAgent) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (a *IngressAgent) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}
