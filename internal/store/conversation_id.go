package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrConversationIDTaken is returned when a create carries an ID (see
// WithNewConversationID) that a conversation already has.
var ErrConversationIDTaken = errors.New("a conversation with this id already exists")

// conversationCreateKeyNS namespaces conversation IDs derived from a create's
// idempotency key. Fixed forever: changing it would give a retried create a
// different ID from its first attempt.
var conversationCreateKeyNS = uuid.MustParse("6f1e7c2a-3b5d-4e8f-9a1c-2d4b6e8f0a13")

// ConversationIDForCreateKey derives the ID a keyed create gives its
// conversation (POST /conversations with client_request_id). The same user and
// key always give the same ID, so a create retried after its reply was lost
// lands on the same primary key: the chat and its key are bound by the one
// INSERT, with no window in which a crash could separate them. The user is
// part of the name, so no one can aim a key at another user's chat.
func ConversationIDForCreateKey(userEmail, key string) string {
	return uuid.NewSHA1(conversationCreateKeyNS, []byte(normalizeEmail(userEmail)+"\x00"+key)).String()
}

type newConversationIDKey struct{}

// WithNewConversationID makes the next conversation create on ctx use id
// rather than a random one (CreateConversation, CreateProjectConversation).
func WithNewConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, newConversationIDKey{}, id)
}

// newConversationID is the ID a create on ctx gives its conversation.
func newConversationID(ctx context.Context) string {
	if id, ok := ctx.Value(newConversationIDKey{}).(string); ok && id != "" {
		return id
	}
	return uuid.NewString()
}

// ConversationIDFromContext is the ID set by WithNewConversationID, or "".
func ConversationIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(newConversationIDKey{}).(string)
	return id
}
