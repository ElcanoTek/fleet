package logging

import (
	"context"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/safe"
)

func restoreLogging(t *testing.T) {
	t.Helper()
	prevSlog := slog.Default()
	prevBridge := bridgeLogger
	prevOut := currentOutput()
	prevConfigured := configured.Load()
	t.Cleanup(func() {
		configured.Store(prevConfigured)
		startupMu.Lock()
		startup = nil
		startupMu.Unlock()
		slog.SetDefault(prevSlog)
		bridgeLogger = prevBridge
		setOutput(prevOut)
		safe.SetPanicHandler(nil)
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})
}

// Lines logged before Configure (config loading, tracing and Sentry init)
// come out through the configured handler: its format, its level and the
// file sink — not Go's default handler (#2052).
func TestStartupLinesHonourConfiguredFormatAndLevel(t *testing.T) {
	restoreLogging(t)
	BufferStartup()
	slog.Info("sentry: disabled (FLEET_SENTRY_DSN not set)")
	slog.Debug("startup debug detail")
	From(WithConversation(context.Background(), "c-boot")).WithGroup("g").With("k", "v").Warn("startup warning", "n", 1)

	path := t.TempDir() + "/fleet.log"
	closer, err := Configure(Config{File: path, MaxSizeMB: 10, Format: "text", Level: "info"})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	defer closer.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, `level=INFO msg="sentry: disabled (FLEET_SENTRY_DSN not set)"`) {
		t.Errorf("startup info line not written in the configured (logfmt) format to the file:\n%s", out)
	}
	if strings.Contains(out, "startup debug detail") {
		t.Errorf("startup debug line written although the level is info:\n%s", out)
	}
	if !strings.Contains(out, `msg="startup warning"`) || !strings.Contains(out, "conv_id=c-boot") || !strings.Contains(out, "g.k=v") || !strings.Contains(out, "g.n=1") {
		t.Errorf("startup line lost its context or group attrs:\n%s", out)
	}
	// Buffering stops at Configure: later lines go straight through.
	slog.Info("after configure")
	if data, _ = os.ReadFile(path); !strings.Contains(string(data), "after configure") {
		t.Errorf("post-Configure line not written:\n%s", data)
	}
}

// A process that ends before Configure still shows its buffered lines.
func TestFlushStartupWithoutConfigure(t *testing.T) {
	restoreLogging(t)
	BufferStartup()
	slog.Info("load config")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevStderr := os.Stderr
	os.Stderr = w
	FlushStartup()
	os.Stderr = prevStderr
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	if got := string(buf[:n]); !strings.Contains(got, `"msg":"load config"`) {
		t.Fatalf("FlushStartup wrote %q, want the buffered line as JSON", got)
	}
}

// The overflow warning is a warn record like any other: a handler gated
// above warn does not get it (#2074 review), one at warn or below does.
func TestStartupOverflowWarningHonoursTheLevel(t *testing.T) {
	for _, tc := range []struct {
		level slog.Level
		want  bool
	}{{slog.LevelError, false}, {slog.LevelWarn, true}} {
		restoreLogging(t)
		BufferStartup()
		for range maxStartupRecords + 3 {
			slog.Error("boot noise")
		}
		var buf strings.Builder
		replayStartup(NewHandler("json", &buf, tc.level))
		if got := strings.Contains(buf.String(), "startup log buffer overflowed"); got != tc.want {
			t.Fatalf("level %v: overflow warning written = %v, want %v", tc.level, got, tc.want)
		}
	}
}
