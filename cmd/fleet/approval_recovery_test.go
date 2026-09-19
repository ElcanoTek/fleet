package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestBootRecoveryOrdersApprovalOutcomeAfterOriginatingTurn(t *testing.T) {
	dsn := os.Getenv("FLEET_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL not set")
	}
	st, err := store.Open(dsn, store.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	const user = "recovery-test@example.com"
	conv, err := st.CreateConversation(ctx, user, "recovery order", "", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	turnID := "recover-" + conv.ID
	if err := st.CreateTurn(ctx, turnID, conv.ID, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitUserMessage(ctx, conv.ID, turnID, agent.HistoryEntry{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"run the action"}`)}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []store.TurnJournalRow{
		{TurnID: turnID, Seq: 1, Kind: store.TurnJournalIntent, CallID: "call", ToolName: "bash", Content: `{}`},
		{TurnID: turnID, Seq: 2, Kind: store.TurnJournalResult, CallID: "call", ToolName: "bash", Content: "APPROVAL_REQUIRED"},
	} {
		if err := st.InsertTurnJournal(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	a, err := st.CreateApproval(ctx, conv.ID, user, "bash", "call", `{}`, 0, store.ApprovalSeat{})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimApproval(ctx, user, a.ID, "approved", store.ApprovalExecutingSentinel); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	recoverStrandedTurns(st, 30)
	history, err := st.LoadHistory(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	callIndex, pendingIndex, outcomeIndex := -1, -1, -1
	for i, entry := range history {
		if entry.Type == "tool_call" {
			callIndex = i
		}
		if entry.Type != "tool_result" {
			continue
		}
		var result agent.ToolResultContent
		if err := json.Unmarshal(entry.Content, &result); err != nil {
			t.Fatal(err)
		}
		if result.Text == "APPROVAL_REQUIRED" {
			pendingIndex = i
		}
		if strings.Contains(result.Text, "Do not repeat") {
			outcomeIndex = i
		}
	}
	if callIndex < 0 || pendingIndex <= callIndex || outcomeIndex <= pendingIndex {
		t.Fatalf("recovery order: call=%d pending=%d outcome=%d", callIndex, pendingIndex, outcomeIndex)
	}
}
