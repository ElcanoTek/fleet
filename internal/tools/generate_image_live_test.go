//go:build fleet_host_executor

package tools

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGenerateImage_Live hits OpenRouter for real. Gated on the SAME explicit
// opt-in as the other live-model tests (FLEET_MODELS_LIVE=1) plus the key —
// keying only off OPENROUTER_API_KEY's presence made every `make test` on an
// operator box spend money against a nondeterministic provider, which is how
// this test earned a reputation for flakiness that wasn't the code's fault.
//
// Spends real money (~$0.04 per run with the default model). Run manually:
//
//	FLEET_MODELS_LIVE=1 go test ./internal/tools/ -run TestGenerateImage_Live -v
func TestGenerateImage_Live(t *testing.T) {
	if os.Getenv("FLEET_MODELS_LIVE") != "1" {
		t.Skip("set FLEET_MODELS_LIVE=1 (and OPENROUTER_API_KEY) to run this live test")
	}
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live OpenRouter smoke test")
	}
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	tmp := t.TempDir()
	t.Setenv("FLEET_ALLOWED_DIRS", tmp)
	t.Chdir(tmp)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// The provider occasionally answers a generation request with no image at
	// all — a known model-side nondeterminism, not a fleet bug. A live test
	// that can fail on provider mood is worse than no test, so retry a few
	// times and only fail when the LAST attempt still yields nothing.
	var (
		res *imageGenResult
		err error
	)
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		res, err = runGenerateImage(ctx, fsTestSandbox(t), &http.Client{Timeout: 180 * time.Second}, GenerateImageParams{
			Prompt:   "A simple solid blue square on a white background, 256x256, no text, minimalist.",
			Filename: "smoke",
		})
		if err == nil {
			break
		}
		t.Logf("attempt %d/%d: %v", i, attempts, err)
		if i < attempts {
			select {
			case <-ctx.Done():
				t.Fatalf("context expired during retries: %v", ctx.Err())
			case <-time.After(5 * time.Second):
			}
		}
	}
	if err != nil {
		t.Fatalf("runGenerateImage after %d attempts: %v", attempts, err)
	}
	if res.Bytes < 100 {
		t.Errorf("output too small: %d bytes", res.Bytes)
	}
	if res.MediaType == "" {
		t.Errorf("media type missing")
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("output file missing: %v", err)
	}
	if filepath.Ext(res.Path) == "" {
		t.Errorf("expected an extension on saved path, got %q", res.Path)
	}
	t.Logf("generated %d bytes to %s (model=%s media=%s cost=%v)",
		res.Bytes, res.Path, res.Model, res.MediaType, res.CostUSD)
}
