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
	Get(ctx context.Context, userEmail, convID string) (*store.Conversation, error)
	List(ctx context.Context, userEmail string) ([]store.Conversation, error)
	Delete(ctx context.Context, userEmail, convID string) error
	DeleteAllUnpinned(ctx context.Context, userEmail string) (int, error)
	SetPinned(ctx context.Context, userEmail, convID string, pinned bool) error
	SetModel(ctx context.Context, userEmail, convID, model string) error
	SetOptionalMCPServers(ctx context.Context, userEmail, convID string, servers []string) error
	SetRuntime(ctx context.Context, userEmail, convID, runtime string) error
	UpdateTitle(ctx context.Context, userEmail, convID, title string) error

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
	LookupTurn(ctx context.Context, turnID string) (*store.TurnRecord, error)

	// Memories + memory proposals.
	ListMemories(ctx context.Context, userEmail string) ([]store.Memory, error)
	CreateMemory(ctx context.Context, userEmail, content, source string) (*store.Memory, error)
	UpdateMemory(ctx context.Context, userEmail, id, content string) (*store.Memory, error)
	DeleteMemory(ctx context.Context, userEmail, id string) error
	CreateMemoryProposal(ctx context.Context, userEmail, conversationID, content string) (*store.Memory, error)
	AcceptMemoryProposal(ctx context.Context, userEmail, id string) (*store.Memory, error)
	ListPendingMemoryProposalsForConversation(ctx context.Context, userEmail, conversationID string) ([]store.Memory, error)

	// Approvals.
	CreateApproval(ctx context.Context, convID, userEmail, toolName, toolCallID, argsJSON string) (*store.Approval, error)
	GetApproval(ctx context.Context, userEmail, approvalID string) (*store.Approval, error)
	ClaimApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) (bool, error)
	ResolveApproval(ctx context.Context, userEmail, approvalID, newStatus, resultText string) error
	SetApprovalResult(ctx context.Context, userEmail, approvalID, resultText string) error
	ListPendingApprovals(ctx context.Context, userEmail, convID string) ([]store.Approval, error)
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
	SweepOrphanWorkspaces(ctx context.Context, root string) (int, error)
}

// Compile-time proof that the concrete Postgres store satisfies the interface —
// if a server call site needs a method not listed above, this fails to build.
var _ chatStore = (*store.Store)(nil)
