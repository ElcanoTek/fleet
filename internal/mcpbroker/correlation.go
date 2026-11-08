package mcpbroker

import (
	"context"
	"log/slog"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// correlationKeys are the log correlation attributes a request carries to the
// broker (request.Correlation): identifiers only, never a user, a value or a
// credential. The child drops any other key, and any value past
// maxCorrelationValue bytes.
var correlationKeys = map[string]bool{
	logging.KeyConvID:    true,
	logging.KeyTurnID:    true,
	logging.KeyTaskID:    true,
	logging.KeyRunID:     true,
	logging.KeyRequestID: true,
	logging.KeyTraceID:   true,
	logging.KeySpanID:    true,
}

const maxCorrelationValue = 256

// correlationFrom is ctx's allowlisted correlation, for the request frame.
func correlationFrom(ctx context.Context) map[string]string {
	var out map[string]string
	for _, a := range logging.Attrs(ctx) {
		if !correlationKeys[a.Key] {
			continue
		}
		v := a.Value.String()
		if v == "" || len(v) > maxCorrelationValue {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(correlationKeys))
		}
		out[a.Key] = v
	}
	return out
}

// withCorrelation attaches a request's correlation to the child's context,
// keeping only the allowlisted keys.
func withCorrelation(ctx context.Context, c map[string]string) context.Context {
	if len(c) == 0 {
		return ctx
	}
	attrs := make([]slog.Attr, 0, len(c))
	for k, v := range c {
		if correlationKeys[k] && v != "" && len(v) <= maxCorrelationValue {
			attrs = append(attrs, slog.String(k, v))
		}
	}
	if len(attrs) == 0 {
		return ctx
	}
	return logging.WithAttrs(ctx, attrs...)
}
