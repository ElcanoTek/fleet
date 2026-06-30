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
	// Folders & labels (#258). ListFiltered backs the ?folder= / ?label= filters
	// on GET /conversations; ListFolders enumerates the user's folders + counts
	// for GET /folders; RenameFolder backs POST /folders/rename.
	ListFiltered(ctx context.Context, userEmail string, f store.ListFilter) ([]store.Conversation, error)
	ListFolders(ctx context.Context, userEmail string) ([]store.FolderCount, error)
	RenameFolder(ctx context.Context, userEmail, from, to string) (int, error)
	Delete(ctx context.Context, userEmail, convID string) error
	DeleteAllUnpinned(ctx context.Context, userEmail string) (int, error)
	// Bulk conversation operations (#279). DeleteByIDs hard-deletes (or, when
	// soft-delete is enabled, tombstones) the supplied IDs scoped by ownership;
	// a foreign or unknown ID returns store.ErrForeignConversation (→ 403) and
	// the whole operation is a no-op. BulkPatch applies additive mutations
	// (nil pointer = leave the field untouched) to the supplied IDs in a single
	// transaction with the same ownership pre-check.
	DeleteByIDs(ctx context.Context, userEmail string, ids []string) (int, error)
	DeleteAllMatching(ctx context.Context, userEmail, folder, label string) (int, error)
	BulkPatch(ctx context.Context, userEmail string, ids []string, pinned *bool, folder *string, labels []string) (int, error)
	SetPinned(ctx context.Context, userEmail, convID string, pinned bool) error
	SetArchived(ctx context.Context, userEmail, convID string, archived bool) error
	SetModel(ctx context.Context, userEmail, convID, model string) error
	SetApprovalTimeout(ctx context.Context, userEmail, convID string, seconds *int) error
	SetOptionalMCPServers(ctx context.Context, userEmail, convID string, servers []string) error
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
	AppendHistory(ctx context.Context, convID string, entries []agent.HistoryEntry) error
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

	// Tool-call audit ledger (#224): one row per tool invocation, written from
	// the post-turn persistence path and read by GET /conversations/{id}/audit.
	RecordToolCalls(ctx context.Context, entries []store.ToolCallEntry) error
	ListToolCalls(ctx context.Context, conversationID, toolFilter string, fromUnix int64, limit int) ([]store.ToolCallEntry, error)

	// Memories + memory proposals.
	ListMemories(ctx context.Context, userEmail string) ([]store.Memory, error)
	CreateMemory(ctx context.Context, userEmail, content, source string) (*store.Memory, error)
	UpdateMemory(ctx context.Context, userEmail, id, content string) (*store.Memory, error)
	DeleteMemory(ctx context.Context, userEmail, id string) error
	CreateMemoryProposal(ctx context.Context, userEmail, conversationID, content string) (*store.Memory, error)
	AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*store.Memory, error)
	ListPendingMemoryProposalsForConversation(ctx context.Context, userEmail, conversationID string) ([]store.Memory, error)

	// Approvals.
	CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string, expiresAt int64) (*store.Approval, error)
	GetApproval(ctx context.Context, userEmail, approvalID string) (*store.Approval, error)
	ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error)
	ResolveApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) error
	SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string) error
	ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]store.Approval, error)
	// ListExpiredApprovals + ClaimApproval back the server-side expiry sweep
	// (#225): pending approvals past their expires_at deadline are auto-denied.
	ListExpiredApprovals(ctx context.Context, now int64) ([]store.Approval, error)
	LatestApprovalByTool(ctx context.Context, convID, toolName string) (*store.Approval, error)
	SupersedePendingApprovals(ctx context.Context, convID, toolName string) (int64, error)
	CountUserMessagesAfterTimestamp(ctx context.Context, convID string, ts int64) (int64, error)

	// Health summary (#301): DB liveness + pool snapshot + chat-side LLM spend.
	Ping(ctx context.Context) error
	PoolStats() sql.DBStats
	LLMUsageSince(ctx context.Context, since int64) (calls int64, costUSD float64, err error)

	// Users (auth gate) + admin stats + sweeps.
	IsUser(ctx context.Context, email string) (bool, error)
	CountUsers(ctx context.Context) (int, error)
	VerifyUser(ctx context.Context, email, plainPassword string) error
	AdminStats(ctx context.Context) ([]store.AdminRow, error)
	SweepExpired(ctx context.Context, ttl time.Duration, unpinnedCap int) (expired int, evicted int, err error)
	AutoArchiveOlderThan(ctx context.Context, d time.Duration) (int, error)
	SweepOrphanWorkspaces(ctx context.Context, root string) (int, error)
}

// Compile-time proof that the concrete Postgres store satisfies the interface —
// if a server call site needs a method not listed above, this fails to build.
var _ chatStore = (*store.Store)(nil)
