package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestKeepMessageUploadsBackfillEscalatesRepeatedFailures: until the record of
// earlier messages' uploads completes, no upload is removed, so a backfill
// that keeps failing logs an error from the fifth failure in a row on, after
// warnings, and the loop stops once an attempt succeeds.
func TestKeepMessageUploadsBackfillEscalatesRepeatedFailures(t *testing.T) {
	// Not parallel: it swaps the process-wide default logger.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	attempts := 0
	backfill := func(context.Context) (int, error) {
		attempts++
		if attempts <= 6 {
			return 0, errors.New("database unavailable")
		}
		return 3, nil
	}
	done := make(chan struct{})
	go func() {
		keepMessageUploadsBackfill(context.Background(), backfill, time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the backfill loop did not stop after a successful attempt")
	}

	var warns, errs int
	for _, line := range strings.Split(buf.String(), "\n") {
		if !strings.Contains(line, "message uploads backfill: failed") {
			continue
		}
		switch {
		case strings.Contains(line, "level=WARN"):
			warns++
		case strings.Contains(line, "level=ERROR"):
			errs++
		}
	}
	if attempts != 7 || warns != 4 || errs != 2 {
		t.Errorf("attempts=%d warnings=%d errors=%d; want 7 attempts, 4 warnings, then 2 errors\n%s", attempts, warns, errs, buf.String())
	}
	if !strings.Contains(buf.String(), "references=3") {
		t.Errorf("the successful attempt was not logged:\n%s", buf.String())
	}
}
