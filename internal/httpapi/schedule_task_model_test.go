package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// convGetterFunc adapts a func to conversationGetter.
type convGetterFunc func(ctx context.Context, userEmail, convID string) (*store.Conversation, error)

func (f convGetterFunc) Get(ctx context.Context, userEmail, convID string) (*store.Conversation, error) {
	return f(ctx, userEmail, convID)
}

// TestScheduledTaskModel pins the model a chat-created scheduled task inherits (#1014).
//
// schedule_task's `model` is optional, so the agent routinely omits it. Before
// this, an omitted model left task.Model nil with only a server env var behind it
// — and on a deployment that never set one, every chat-created task dead-lettered
// at first dispatch with "no model configured", having run nothing. Inheriting the
// conversation's model also makes the run reproducible: the task records what it
// was created against rather than drifting with the env.
func TestScheduledTaskModel(t *testing.T) {
	approval := &store.Approval{UserEmail: "someone@example.com", ConversationID: "conv-1"}

	t.Run("explicit model wins and skips the lookup", func(t *testing.T) {
		convs := convGetterFunc(func(context.Context, string, string) (*store.Conversation, error) {
			t.Fatal("must not look up the conversation when the agent pinned a model")
			return nil, nil
		})
		if got := scheduledTaskModel(context.Background(), convs, approval, "  vendor/pinned  "); got != "vendor/pinned" {
			t.Errorf("model = %q, want the trimmed explicit slug", got)
		}
	})

	t.Run("omitted model inherits the conversation's", func(t *testing.T) {
		var gotEmail, gotConv string
		convs := convGetterFunc(func(_ context.Context, userEmail, convID string) (*store.Conversation, error) {
			gotEmail, gotConv = userEmail, convID
			return &store.Conversation{Model: "vendor/conversation"}, nil
		})
		if got := scheduledTaskModel(context.Background(), convs, approval, ""); got != "vendor/conversation" {
			t.Errorf("model = %q, want the conversation's model", got)
		}
		// Scoped to the approving user's own conversation, not a bare id lookup.
		if gotEmail != "someone@example.com" || gotConv != "conv-1" {
			t.Errorf("looked up (%q, %q), want the approval's user and conversation", gotEmail, gotConv)
		}
	})

	t.Run("lookup failure falls through rather than failing the create", func(t *testing.T) {
		convs := convGetterFunc(func(context.Context, string, string) (*store.Conversation, error) {
			return nil, errors.New("database is down")
		})
		if got := scheduledTaskModel(context.Background(), convs, approval, ""); got != "" {
			t.Errorf("model = %q, want empty so the orchestrator default still applies", got)
		}
	})

	t.Run("missing conversation falls through", func(t *testing.T) {
		convs := convGetterFunc(func(context.Context, string, string) (*store.Conversation, error) {
			return nil, nil
		})
		if got := scheduledTaskModel(context.Background(), convs, approval, ""); got != "" {
			t.Errorf("model = %q, want empty", got)
		}
	})

	t.Run("nil store falls through", func(t *testing.T) {
		if got := scheduledTaskModel(context.Background(), nil, approval, ""); got != "" {
			t.Errorf("model = %q, want empty", got)
		}
	})
}
