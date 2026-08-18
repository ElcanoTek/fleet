package httpapi

import (
	"context"
	"database/sql"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// chatStore is the persistence surface the HTTP layer depends on. The concrete
// *store.Store (Postgres) satisfies it; abstracting it as an interface lets the
// always-on tests drive the real /chat→RunTurn→persistence glue against an
// in-memory fake — provider-free AND DB-free, so `go test ./...` exercises the
// path by default rather than only under FLEET_TEST_DATABASE_URL (see issue #49).
//
// This mirrors the existing turnEngine (engine.go) and eventSinkPersister
// (turn_buffer.go) seams: the transport layer depends on interfaces, and
// cmd/fleet supplies the concrete Postgres-backed implementations. chatStore is a
// superset of eventSinkPersister so the same store value the server holds can be
// handed to turnBuffer.attachPersister.
type chatStore interface {
	// Conversations.
	CreateConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool) (*store.Conversation, error)
	// BranchConversation forks a conversation at a chosen message into a new
	// independent conversation that copies the parent's messages up to that point
	// (#454). Backs POST /conversations/{id}/branch.
	BranchConversation(ctx context.Context, userEmail, parentConvID string, branchPointMessageID int64, title string) (*store.Conversation, error)
	Get(ctx context.Context, userEmail, convID string) (*store.Conversation, error)
	List(ctx context.Context, userEmail string, archivedOnly bool) ([]store.Conversation, error)
	// Labels (#258): ListFiltered backs the ?label= filter on GET /conversations.
	ListFiltered(ctx context.Context, userEmail string, f store.ListFilter) ([]store.Conversation, error)
	Delete(ctx context.Context, userEmail, convID string) error
	DeleteAllUnpinned(ctx context.Context, userEmail string) (int, error)
	// Bulk conversation operations (#279). DeleteByIDs hard-deletes (or, when
	// soft-delete is enabled, tombstones) the supplied IDs scoped by ownership;
	// a foreign or unknown ID returns store.ErrForeignConversation (→ 403) and
	// the whole operation is a no-op. BulkPatch applies additive mutations
	// (nil pointer = leave the field untouched) to the supplied IDs in a single
	// transaction with the same ownership pre-check.
	DeleteByIDs(ctx context.Context, userEmail string, ids []string) (int, error)
	DeleteAllMatching(ctx context.Context, userEmail, label string) (int, error)
	BulkPatch(ctx context.Context, userEmail string, ids []string, pinned *bool, labels []string) (int, error)
	SetPinned(ctx context.Context, userEmail, convID string, pinned bool) error
	SetArchived(ctx context.Context, userEmail, convID string, archived bool) error
	// SetConversationProject re-files a conversation into a project ("" =
	// unfile); the handler validates membership first (#509 follow-up).
	SetConversationProject(ctx context.Context, userEmail, convID, projectID string) error
	SetModel(ctx context.Context, userEmail, convID, model string) error
	SetApprovalTimeout(ctx context.Context, userEmail, convID string, seconds *int) error
	SetThinkingConfig(ctx context.Context, userEmail, convID string, cfg *store.ThinkingConfig) error
	SetOptionalMCPServers(ctx context.Context, userEmail, convID string, servers []string) error
	// Per-user connector availability preferences (unified connector UX).
	SetConnectorPref(ctx context.Context, userEmail string, p store.ConnectorPref) error
	DeleteConnectorPref(ctx context.Context, userEmail, kind, connectorID string) error
	ListConnectorPrefs(ctx context.Context, userEmail string) (map[string]store.ConnectorPref, error)
	// User-authored Agent Skills (docs/SKILLS.md phase 2).
	CreateUserSkill(ctx context.Context, userEmail, name, description, body string) (*store.UserSkill, error)
	CreateUserSkillProposal(ctx context.Context, userEmail, name, description, body string) (*store.UserSkill, error)
	UpdateUserSkill(ctx context.Context, userEmail, id, name, description, body, status string) (*store.UserSkill, error)
	ListUserSkills(ctx context.Context, userEmail string) ([]store.UserSkill, error)
	DeleteUserSkill(ctx context.Context, userEmail, id string) error
	// Read-only public sharing (#226): the owner issues/revokes a share token;
	// GetConversationByShareToken serves the unauthenticated /shared/{token} read.
	SetShareToken(ctx context.Context, ownerEmail, convID, token string, expiresAt *int64) error
	RevokeShareToken(ctx context.Context, ownerEmail, convID string) error
	GetConversationByShareToken(ctx context.Context, token string, now int64) (*store.SharedConversation, error)
	UpdateTitle(ctx context.Context, userEmail, convID, title string) error
	RenameTitle(ctx context.Context, userEmail, convID, title string) error

	// Full-text search (#308): ranked title + message-content matches, scoped to
	// the user and paginated; returns (results, total, error).
	SearchConversations(ctx context.Context, userEmail, query string, limit, offset int) ([]store.SearchResult, int, error)

	// History + summaries.
	LoadHistory(ctx context.Context, convID string) ([]agent.HistoryEntry, error)
	AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) ([]int64, error)
	// Durable turn journal + gated canonical projection (#798).
	CommitUserMessage(ctx context.Context, convID, turnID string, entry agent.HistoryEntry) (int64, error)
	CommitTurnHistory(ctx context.Context, convID, turnID string, entries []agent.HistoryEntry) ([]int64, error)
	InsertTurnJournal(ctx context.Context, r store.TurnJournalRow) error
	// Conversation input queue + mid-turn steering (#785).
	EnqueueInput(ctx context.Context, r store.InputQueueRow) (store.InputQueueRow, bool, error)
	CountPendingInputs(ctx context.Context, convID string) (int, error)
	ListQueuedInputs(ctx context.Context, userEmail, convID string) ([]store.InputQueueRow, error)
	ClaimNextQueuedInput(ctx context.Context, convID, turnID string) (*store.InputQueueRow, error)
	MarkInputInjected(ctx context.Context, id, turnID string) (bool, error)
	MarkInputTerminal(ctx context.Context, id, state string) error
	CompleteInjectedInputs(ctx context.Context, turnID string) error
	CancelQueuedInputs(ctx context.Context, userEmail, convID string) (int, error)
	RemoveQueuedInput(ctx context.Context, userEmail, convID, id string) (bool, error)
	PromoteQueuedInput(ctx context.Context, userEmail, convID, id string) (bool, error)
	BindInputTurn(ctx context.Context, id, turnID string) error
	LookupInput(ctx context.Context, convID, clientID string) (*store.InputQueueRow, error)
	SettleTurnInputs(ctx context.Context, turnID, drainedID string) (requeued, cancelled int, err error)
	ReplaceSummary(ctx context.Context, userEmail, convID string, entry agent.HistoryEntry) error
	TruncateAfter(ctx context.Context, userEmail, convID string, afterMessageID int64) error
	MaxMessageIDForRole(ctx context.Context, convID, role string) (int64, error)
	SecondMaxMessageIDForRole(ctx context.Context, convID, role string) (int64, error)

	// Turn metrics + the incremental turn-event persistence the buffer uses
	// (the eventSinkPersister subset: CreateTurn / InsertTurnEvents / FinishTurn).
	RecordTurn(ctx context.Context, m store.TurnMetric) error
	CreateTurn(ctx context.Context, turnID, convID string, startedAt int64) error
	InsertTurnEvents(ctx context.Context, events []store.TurnEvent) error
	FinishTurn(ctx context.Context, turnID string, status store.TurnStatus, finishedAt int64, lossy bool) error
	LoadTurnEvents(ctx context.Context, turnID string, afterEventID uint64) ([]store.TurnEvent, error)
	// GetTurnEventPage is the cursor-paginated read path over a whole
	// conversation's turn events (#189). See store.GetTurnEventPage for the
	// cursor/direction contract.
	GetTurnEventPage(ctx context.Context, conversationID string, cursor int64, limit int, asc bool) ([]store.TurnEvent, int64, error)
	LookupTurn(ctx context.Context, turnID string) (*store.TurnRecord, error)
	// LookupTurnInConversation folds conversation scope into the query
	// (#1112) so the stream DB-fallback cannot leak another conversation's
	// turn events if the handler's equality check is ever dropped.
	LookupTurnInConversation(ctx context.Context, turnID, conversationID string) (*store.TurnRecord, error)

	// Tool-call audit ledger (#224): one row per tool invocation, written from
	// the post-turn persistence path and read by GET /conversations/{id}/audit.
	RecordToolCalls(ctx context.Context, entries []store.ToolCallEntry) error
	ListToolCalls(ctx context.Context, conversationID, toolFilter string, fromUnix int64, limit int) ([]store.ToolCallEntry, error)

	// Projects / Spaces (#509).
	CreateProject(ctx context.Context, p *store.Project) (*store.Project, error)
	GetProject(ctx context.Context, id string) (*store.Project, error)
	ListProjectsForUser(ctx context.Context, email, teamID string) ([]store.Project, error)
	UpdateProject(ctx context.Context, ownerEmail, id string, patch store.ProjectPatch) (*store.Project, error)
	DeleteProject(ctx context.Context, ownerEmail, id string) error
	CreateProjectConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool, projectID string, mcpServers []string) (*store.Conversation, error)
	CreateProjectMemory(ctx context.Context, projectID, creatorEmail, content, kind string) (*store.Memory, error)
	ListProjectMemories(ctx context.Context, projectID string) ([]store.Memory, error)
	DeleteProjectMemory(ctx context.Context, projectID, memoryID string) error
	ListProjectConversationIDs(ctx context.Context, projectID string) ([]string, error)
	// ListProjectConversationsForUser is the project home's chat list —
	// the CALLER'S OWN conversations only (chats stay private to their
	// creators even inside a shared project).
	ListProjectConversationsForUser(ctx context.Context, userEmail, projectID string) ([]store.Conversation, error)
	// ListProjectConversationPreviews is the home's 1–2 line chat history:
	// last text-message snippet per conversation, same caller scoping.
	ListProjectConversationPreviews(ctx context.Context, userEmail, projectID string) (map[string]string, error)

	// Memories + memory proposals.
	ListMemories(ctx context.Context, userEmail string) ([]store.Memory, error)
	GetMemory(ctx context.Context, userEmail, id string) (*store.Memory, error)
	CreateMemory(ctx context.Context, userEmail, content, source, kind string) (*store.Memory, error)
	// Temporal knowledge graph (#523): derived entity/relation rows over the
	// memories table, plus the two-axis as-of reads (see store.GraphQuery).
	ReplaceRelationsForMemory(ctx context.Context, userEmail, memoryID string, g store.GraphExtraction) (int, error)
	GraphAsOf(ctx context.Context, userEmail string, q store.GraphQuery) (*store.Graph, error)
	ListMemoriesAsOf(ctx context.Context, userEmail string, q store.GraphQuery) ([]store.Memory, error)
	UpdateMemory(ctx context.Context, userEmail, id string, patch store.MemoryPatch) (*store.Memory, error)
	DeleteMemory(ctx context.Context, userEmail, id string) error
	CreateMemoryProposal(ctx context.Context, userEmail, conversationID string, p store.MemoryProposalParams) (*store.Memory, error)
	AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*store.Memory, string, error)
	ListPendingMemoryProposalsForConversation(ctx context.Context, userEmail, conversationID string) ([]store.Memory, error)

	// Approvals.
	CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string, expiresAt int64, seat store.ApprovalSeat) (*store.Approval, error)
	GetApproval(ctx context.Context, userEmail, approvalID string) (*store.Approval, error)
	ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error)
	// ClaimExpiredApproval is the sweep-only counterpart: it claims a
	// pending row whose expires_at has already passed. User-facing
	// ClaimApproval refuses those rows so default-deny is authoritative
	// at click time (#1109), not whenever the next sweep tick runs.
	ClaimExpiredApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error)
	ResolveApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) error
	SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string) error
	ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]store.Approval, error)
	// ListExpiredApprovals + ClaimExpiredApproval back the server-side
	// expiry sweep (#225): pending approvals past their expires_at
	// deadline are auto-denied for notification/audit. The claim-time
	// check on ClaimApproval is what makes the deny authoritative.
	ListExpiredApprovals(ctx context.Context, now int64) ([]store.Approval, error)
	LatestApprovalByTool(ctx context.Context, convID, toolName string) (*store.Approval, error)
	SupersedePendingApprovals(ctx context.Context, convID, toolName string) (int64, error)
	CountUserMessagesAfterTimestamp(ctx context.Context, convID string, ts int64) (int64, error)

	// Browser Web Push subscriptions (#292): the POST /push/subscribe upsert
	// and the owner-scoped DELETE /push/unsubscribe. (The send path reads
	// subscriptions through internal/webpush's own store seam, not this one.)
	UpsertPushSubscription(ctx context.Context, userEmail, endpoint, keysAuth, keysP256dh string) error
	DeleteUserPushSubscription(ctx context.Context, userEmail, endpoint string) error

	// Health summary (#301): DB liveness + pool snapshot + chat-side LLM spend.
	Ping(ctx context.Context) error
	PoolStats() sql.DBStats
	LLMUsageSince(ctx context.Context, since int64) (calls int64, costUSD float64, err error)

	// Users (auth gate) + admin stats + sweeps.
	IsUser(ctx context.Context, email string) (bool, error)
	// GetUser returns the full record (role + team) used by membershipMiddleware
	// to admit AND enrich a request with the caller's role/team (#237).
	GetUser(ctx context.Context, email string) (*store.User, error)
	// SessionEpoch backs GET /auth/session-epoch: the value the Next.js tier
	// stamps into a session cookie it is about to mint.
	SessionEpoch(ctx context.Context, email string) (string, error)
	// ListUsers + SetUserRoleTeam back the admin Users tab (#237): list every
	// provisioned account, and PATCH a single account's role/team.
	ListUsers(ctx context.Context) ([]store.User, error)
	SetUserRoleTeam(ctx context.Context, email string, role, teamID *string) (*store.User, error)
	// SetOwnTeam is the member-facing team write (#1157): the caller sets its
	// OWN team_id. Creating a team and leaving one are self-serve; joining a
	// team that already has members is refused with store.ErrTeamExists unless
	// allowExisting (admins). See internal/httpapi/me.go.
	SetOwnTeam(ctx context.Context, email, teamID string, allowExisting bool) (*store.User, error)
	RenameTeam(ctx context.Context, from, to string) (usersUpdated, projectsUpdated int64, err error)
	// CreateUser/DeleteUser/UpdatePassword complete the admin Users tab CRUD so
	// user management no longer requires CLI access to the box (`fleet admin
	// add` / `fleet chat user ...` stay the scriptable equivalents).
	CreateUser(ctx context.Context, email, plainPassword string) (*store.User, error)
	DeleteUser(ctx context.Context, email string) error
	UpdatePassword(ctx context.Context, email, plainPassword string) error
	CountUsers(ctx context.Context) (int, error)
	VerifyUser(ctx context.Context, email, plainPassword string) error
	// Team-scoped, opt-in conversation sharing (#237). ListTeamConversations
	// returns the conversations same-team members have shared (team_visible),
	// read-only; SetConversationTeamVisible flips the owner's opt-in flag.
	ListTeamConversations(ctx context.Context, callerEmail string) ([]store.Conversation, error)
	SetConversationTeamVisible(ctx context.Context, ownerEmail, convID string, visible bool) error
	AdminStats(ctx context.Context) ([]store.AdminRow, error)
	// MigrationStatus reports applied vs pending chat-DB migrations for
	// GET /admin/migrations (#256). Read-only.
	MigrationStatus(ctx context.Context) (store.MigrationReport, error)
	// Admin-managed LLM providers (migration 034): CRUD for /admin/llm-providers
	// plus the member-level names+models read for the model picker. Key VALUES
	// are write-only — no method here ever returns one.
	ListLLMProviders(ctx context.Context) ([]store.LLMProvider, error)
	// GetLLMProviderConfig decrypts ONE row's key for the host-side
	// test-connection probe — the result never serializes to HTTP.
	GetLLMProviderConfig(ctx context.Context, id string) (*store.LLMProviderConfig, error)
	CreateLLMProvider(ctx context.Context, in store.LLMProviderInput) (*store.LLMProvider, error)
	UpdateLLMProvider(ctx context.Context, id string, in store.LLMProviderInput) (*store.LLMProvider, error)
	DeleteLLMProvider(ctx context.Context, id string) error
	SweepExpired(ctx context.Context, ttl time.Duration, unpinnedCap int) (expired int, evicted int, err error)
	PurgeTerminalInputs(ctx context.Context, retention time.Duration) (int, error)
	// SweepTurnEvents prunes the durable SSE ledger (turns + turn_events +
	// turn_journal via cascade) for turns terminal longer than ttl; a
	// non-positive ttl disables it. See store.SweepTurnEvents.
	SweepTurnEvents(ctx context.Context, ttl time.Duration) (int, error)
	AutoArchiveOlderThan(ctx context.Context, d time.Duration) (int, error)
	SweepOrphanWorkspaces(ctx context.Context, root string) (int, error)
	// Admin storage panel (disk visibility + reclaim).
	StorageConversationStats(ctx context.Context, cutoff time.Time) (store.StorageConversationStats, error)
	DeleteUnpinnedOlderThan(ctx context.Context, cutoff time.Time) (int, error)
	ConversationStorageMetaByIDs(ctx context.Context, ids []string) (map[string]store.ConversationStorageMeta, error)
}

// Compile-time proof that the concrete Postgres store satisfies the interface —
// if a server call site needs a method not listed above, this fails to build.
var _ chatStore = (*store.Store)(nil)
