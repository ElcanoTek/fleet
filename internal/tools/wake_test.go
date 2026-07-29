package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixedNow pins the wake tools' clock so deadline assertions are exact.
var fixedNow = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

func nowFixed() time.Time { return fixedNow }

func TestSleepTool(t *testing.T) {
	var spec WakeSpec
	ctx := WithWakeHandler(context.Background(), func(s WakeSpec) error { spec = s; return nil })

	// seconds form: exact deadline, note carried.
	resp := runToolCtx(ctx, t, NewSleepTool(nowFixed), `{"seconds":3600,"note":"re-check results"}`)
	if resp.IsError || !strings.Contains(resp.Content, "pausing") {
		t.Fatalf("sleep success response: %+v", resp)
	}
	if want := fixedNow.Add(time.Hour); !spec.WakeAt.Equal(want) || spec.Note != "re-check results" || spec.EventKey != "" {
		t.Fatalf("spec = %+v; want wake at %v", spec, want)
	}

	// until form.
	until := fixedNow.Add(2 * time.Hour).Format(time.RFC3339)
	runToolCtx(ctx, t, NewSleepTool(nowFixed), `{"until":"`+until+`","note":"n"}`)
	if want := fixedNow.Add(2 * time.Hour); !spec.WakeAt.Equal(want) {
		t.Fatalf("until: wake at %v; want %v", spec.WakeAt, want)
	}

	// Sub-minute sleeps clamp UP to the minimum (no busy-loop polling).
	runToolCtx(ctx, t, NewSleepTool(nowFixed), `{"seconds":5,"note":"n"}`)
	if want := fixedNow.Add(WakeMinDelay); !spec.WakeAt.Equal(want) {
		t.Fatalf("clamp: wake at %v; want %v", spec.WakeAt, want)
	}

	// Errors, all surfaced to the model (never a Go error): both/neither
	// forms, unparseable until, too-far deadline, missing note, no handler.
	for name, args := range map[string]string{
		"both forms":   `{"seconds":60,"until":"` + until + `","note":"n"}`,
		"neither form": `{"note":"n"}`,
		"bad until":    `{"until":"tomorrow","note":"n"}`,
		"too far":      fmt.Sprintf(`{"seconds":%d,"note":"n"}`, int((WakeMaxDelay+time.Hour)/time.Second)),
		"missing note": `{"seconds":60}`,
		"blank note":   `{"seconds":60,"note":"  "}`,
	} {
		if r := runToolCtx(ctx, t, NewSleepTool(nowFixed), args); !r.IsError {
			t.Errorf("%s must error: %+v", name, r)
		}
	}
	if r := runToolCtx(context.Background(), t, NewSleepTool(nowFixed), `{"seconds":60,"note":"n"}`); !r.IsError || !strings.Contains(r.Content, "SLEEP_UNAVAILABLE") {
		t.Fatalf("sleep without handler must error clearly: %+v", r)
	}

	// A handler error (e.g. the cycle cap) is surfaced, telling the model to
	// finish normally.
	capped := WithWakeHandler(context.Background(), func(WakeSpec) error { return errors.New("cycle cap") })
	if r := runToolCtx(capped, t, NewSleepTool(nowFixed), `{"seconds":60,"note":"n"}`); !r.IsError || !strings.Contains(r.Content, "cycle cap") {
		t.Fatalf("handler error must surface: %+v", r)
	}

	if !WakeHandlerInstalled(ctx) || WakeHandlerInstalled(context.Background()) {
		t.Fatal("installed predicate")
	}
}

func TestWakeOnEventTool(t *testing.T) {
	var spec WakeSpec
	ctx := WithWakeHandler(context.Background(), func(s WakeSpec) error { spec = s; return nil })

	// Default timeout applies when unset.
	resp := runToolCtx(ctx, t, NewWakeOnEventTool(nowFixed), `{"event":"deploy-finished","note":"verify deploy"}`)
	if resp.IsError || !strings.Contains(resp.Content, "deploy-finished") {
		t.Fatalf("wake_on_event success response: %+v", resp)
	}
	if want := fixedNow.Add(WakeDefaultEventExpiry); spec.EventKey != "deploy-finished" || !spec.WakeAt.Equal(want) {
		t.Fatalf("spec = %+v; want event deadline %v", spec, want)
	}

	// Explicit timeout.
	runToolCtx(ctx, t, NewWakeOnEventTool(nowFixed), `{"event":"e","timeout_seconds":600,"note":"n"}`)
	if want := fixedNow.Add(10 * time.Minute); !spec.WakeAt.Equal(want) {
		t.Fatalf("timeout: deadline %v; want %v", spec.WakeAt, want)
	}

	for name, args := range map[string]string{
		"missing event": `{"note":"n"}`,
		"missing note":  `{"event":"e"}`,
		"huge timeout":  fmt.Sprintf(`{"event":"e","timeout_seconds":%d,"note":"n"}`, int((WakeMaxDelay+time.Hour)/time.Second)),
		"long key":      `{"event":"` + strings.Repeat("k", 300) + `","note":"n"}`,
	} {
		if r := runToolCtx(ctx, t, NewWakeOnEventTool(nowFixed), args); !r.IsError {
			t.Errorf("%s must error: %+v", name, r)
		}
	}
	if r := runToolCtx(context.Background(), t, NewWakeOnEventTool(nowFixed), `{"event":"e","note":"n"}`); !r.IsError || !strings.Contains(r.Content, "WAKE_UNAVAILABLE") {
		t.Fatalf("wake_on_event without handler must error clearly: %+v", r)
	}
}
