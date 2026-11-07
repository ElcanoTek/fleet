package store

import (
	"context"
	"errors"
	"testing"
)

// TestConversationIDForCreateKey pins the derived IDs behind idempotent
// creates: stable for a user and key, different across users or keys, and a
// second create with a taken ID is refused rather than duplicated.
func TestConversationIDForCreateKey(t *testing.T) {
	t.Parallel()

	a := ConversationIDForCreateKey("Me@X.com", "k")
	if a != ConversationIDForCreateKey("me@x.com", "k") {
		t.Error("the same user (any case) and key must give the same ID")
	}
	if a == ConversationIDForCreateKey("you@x.com", "k") || a == ConversationIDForCreateKey("me@x.com", "k2") {
		t.Error("a different user or key must give a different ID")
	}

	s := newTestStore(t)
	ctx := WithNewConversationID(context.Background(), a)
	c, err := s.CreateConversation(ctx, "me@x.com", "t", "victoria", "", false)
	if err != nil || c.ID != a {
		t.Fatalf("create with a derived ID = %v, %v", c, err)
	}
	if _, err := s.CreateConversation(ctx, "me@x.com", "t", "victoria", "", false); !errors.Is(err, ErrConversationIDTaken) {
		t.Fatalf("second create with the same ID err = %v, want ErrConversationIDTaken", err)
	}
	if plain, err := s.CreateConversation(context.Background(), "me@x.com", "t", "victoria", "", false); err != nil || plain.ID == a {
		t.Fatalf("a create without a derived ID = %v, %v", plain, err)
	}
}
