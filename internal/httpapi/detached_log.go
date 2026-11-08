package httpapi

import (
	"context"
	"log/slog"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// detachedCtx is the root for work that runs outside any request's context —
// a background goroutine, a retry loop, a deferred settle — but is still ABOUT
// a user, a conversation or a turn. It carries those ids (the non-empty ones)
// so the lines logged through it join the rest of that conversation's or
// turn's log, instead of a bare context.Background() that drops them. It has
// no deadline or cancellation of its own.
func detachedCtx(user, convID, turnID string) context.Context {
	attrs := make([]slog.Attr, 0, 3)
	if user != "" {
		attrs = append(attrs, slog.String(logging.KeyUserID, user))
	}
	if convID != "" {
		attrs = append(attrs, slog.String(logging.KeyConvID, convID))
	}
	if turnID != "" {
		attrs = append(attrs, slog.String(logging.KeyTurnID, turnID))
	}
	return logging.WithAttrs(context.Background(), attrs...)
}
