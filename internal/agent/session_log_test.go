package agent

import (
	"strings"
	"testing"
	"time"
)

func TestNewLogSession(t *testing.T) {
	beforeCreation := time.Now().Unix()

	session := NewLogSession()

	afterCreation := time.Now().Unix()

	// Check ID prefix
	if !strings.HasPrefix(session.ID, "session-") {
		t.Errorf("Expected ID to start with 'session-', got: %s", session.ID)
	}

	// Check Title
	expectedTitle := "Task Execution"
	if session.Title != expectedTitle {
		t.Errorf("Expected Title to be %q, got: %q", expectedTitle, session.Title)
	}

	// Check timestamps (allow for standard time execution duration)
	if session.CreatedAt < beforeCreation || session.CreatedAt > afterCreation {
		t.Errorf("CreatedAt %d is not between %d and %d", session.CreatedAt, beforeCreation, afterCreation)
	}

	if session.UpdatedAt < beforeCreation || session.UpdatedAt > afterCreation {
		t.Errorf("UpdatedAt %d is not between %d and %d", session.UpdatedAt, beforeCreation, afterCreation)
	}

	// Check Messages
	if session.Messages == nil {
		t.Error("Expected Messages slice to be initialized (non-nil)")
	} else if len(session.Messages) != 0 {
		t.Errorf("Expected Messages slice to be empty, got length: %d", len(session.Messages))
	}

	// Check numeric fields that should default to 0
	if session.PromptTokens != 0 {
		t.Errorf("Expected PromptTokens to be 0, got: %d", session.PromptTokens)
	}

	if session.CompletionTokens != 0 {
		t.Errorf("Expected CompletionTokens to be 0, got: %d", session.CompletionTokens)
	}

	if session.CachedTokens != 0 {
		t.Errorf("Expected CachedTokens to be 0, got: %d", session.CachedTokens)
	}

	if session.CacheCreationTokens != 0 {
		t.Errorf("Expected CacheCreationTokens to be 0, got: %d", session.CacheCreationTokens)
	}

	if session.LastStepPromptTokens != 0 {
		t.Errorf("Expected LastStepPromptTokens to be 0, got: %d", session.LastStepPromptTokens)
	}

	if session.Cost != 0.0 {
		t.Errorf("Expected Cost to be 0.0, got: %f", session.Cost)
	}
}
