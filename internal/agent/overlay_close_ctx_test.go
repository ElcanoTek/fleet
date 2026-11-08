package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// An overlay's teardown failure is logged with the context it was opened
// under — detached from that context's cancellation — so it names the turn
// (#2074 review: Close built its context from context.Background()).
func TestOverlayCloseFailureCarriesTheOpeningContext(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	openCtx, cancel := context.WithCancel(logging.WithTurn(context.Background(), "conv-1", "turn-2"))
	var closeErr error
	o := &RemoteMCPOverlay{
		OpenCtx: openCtx,
		CloseScope: func(ctx context.Context) error {
			closeErr = ctx.Err()
			return errors.New("scope close refused")
		},
	}
	cancel() // the turn has ended by the time the deferred Close runs
	o.Close()
	if closeErr != nil {
		t.Fatalf("Close ran under a cancelled context: %v", closeErr)
	}
	out := buf.String()
	if !strings.Contains(out, "close overlay scope failed") || !strings.Contains(out, `"conv_id":"conv-1"`) || !strings.Contains(out, `"turn_id":"turn-2"`) {
		t.Fatalf("teardown failure without the turn's correlation: %s", out)
	}
}
