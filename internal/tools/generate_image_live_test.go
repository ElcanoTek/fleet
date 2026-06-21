package tools

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGenerateImage_Live hits OpenRouter for real. Skipped unless
// OPENROUTER_API_KEY is set, so it doesn't run in CI by default.
//
// Spends real money (~$0.04 per run with the default model). Run manually:
//
//	go test ./server/internal/tools/ -run TestGenerateImage_Live -v
func TestGenerateImage_Live(t *testing.T) {
	if os.Getenv("OPENROUTER_API_KEY") == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live OpenRouter smoke test")
	}
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	tmp := t.TempDir()
	t.Setenv("FLEET_ALLOWED_DIRS", tmp)
	t.Chdir(tmp)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	res, err := runGenerateImage(ctx, &http.Client{Timeout: 180 * time.Second}, GenerateImageParams{
		Prompt:   "A simple solid blue square on a white background, 256x256, no text, minimalist.",
		Filename: "smoke",
	})
	if err != nil {
		t.Fatalf("runGenerateImage: %v", err)
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
