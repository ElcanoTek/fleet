package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// detachedCtx carries only the ids it was given, under the standard keys.
func TestDetachedCtxCarriesTheGivenIDs(t *testing.T) {
	ctx := detachedCtx("a@b.co", "conv-1", "")
	if logging.Value(ctx, logging.KeyUserID) != "a@b.co" || logging.Value(ctx, logging.KeyConvID) != "conv-1" {
		t.Fatalf("attrs = %v", logging.Attrs(ctx))
	}
	if len(logging.Attrs(ctx)) != 2 {
		t.Fatalf("an empty id was attached: %v", logging.Attrs(ctx))
	}
	if len(logging.Attrs(detachedCtx("", "", ""))) != 0 {
		t.Fatal("no ids should mean no attrs")
	}
}
