package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
)

// fakeRecorder captures published artifacts so tests can assert what the tool
// recorded without a runner or DB.
type fakeRecorder struct {
	calls []recordedArtifact
	err   error
}

type recordedArtifact struct {
	name, relPath, description string
	size                       int64
}

func (f *fakeRecorder) RecordArtifact(name, relPath, description string, size int64) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, recordedArtifact{name, relPath, description, size})
	return nil
}

func runPublish(t *testing.T, tool fantasy.AgentTool, ctx context.Context, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "tc-1", Input: input})
	if err != nil {
		t.Fatalf("publish_artifact Run returned a transport error: %v", err)
	}
	return resp
}

func TestPublishArtifact(t *testing.T) {
	// A workspace with a real file to publish and a subdir file.
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "report.csv"), []byte("a,b,c\n1,2,3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "out", "summary.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctxWork := WithForcedWorkingDir(context.Background(), work)

	t.Run("publishes an existing file with size + description", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"report.csv","description":"Q3 revenue"}`)
		if resp.IsError {
			t.Fatalf("expected success, got %q", resp.Content)
		}
		if len(rec.calls) != 1 {
			t.Fatalf("expected 1 record, got %d", len(rec.calls))
		}
		got := rec.calls[0]
		if got.name != "report.csv" || got.relPath != "report.csv" || got.description != "Q3 revenue" || got.size != 12 {
			t.Errorf("recorded %+v", got)
		}
	})

	t.Run("publishes a file in a subdir; name is the base", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"out/summary.txt"}`)
		if resp.IsError {
			t.Fatalf("expected success, got %q", resp.Content)
		}
		if got := rec.calls[0]; got.name != "summary.txt" || got.relPath != "out/summary.txt" {
			t.Errorf("recorded %+v, want name=summary.txt path=out/summary.txt", got)
		}
	})

	t.Run("missing path is rejected", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"  "}`)
		if !resp.IsError || len(rec.calls) != 0 {
			t.Fatalf("expected error + no record, got isErr=%v calls=%d", resp.IsError, len(rec.calls))
		}
	})

	t.Run("nonexistent file is rejected (must write first)", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"nope.bin"}`)
		if !resp.IsError || len(rec.calls) != 0 {
			t.Fatalf("expected error + no record, got isErr=%v calls=%d", resp.IsError, len(rec.calls))
		}
	})

	t.Run("path traversal is rejected", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"../escape.txt"}`)
		if !resp.IsError || len(rec.calls) != 0 {
			t.Fatalf("expected traversal rejection + no record, got isErr=%v calls=%d", resp.IsError, len(rec.calls))
		}
	})

	t.Run("a directory is rejected", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"out"}`)
		if !resp.IsError || len(rec.calls) != 0 {
			t.Fatalf("expected directory rejection + no record, got isErr=%v calls=%d", resp.IsError, len(rec.calls))
		}
	})

	t.Run("no workspace in context is rejected", func(t *testing.T) {
		rec := &fakeRecorder{}
		resp := runPublish(t, NewPublishArtifactTool(rec), context.Background(), `{"path":"report.csv"}`)
		if !resp.IsError {
			t.Fatalf("expected error with no workspace, got %q", resp.Content)
		}
	})

	t.Run("nil recorder is rejected", func(t *testing.T) {
		resp := runPublish(t, NewPublishArtifactTool(nil), ctxWork, `{"path":"report.csv"}`)
		if !resp.IsError {
			t.Fatalf("expected error with nil recorder, got %q", resp.Content)
		}
	})

	t.Run("recorder error is surfaced (e.g. cap reached)", func(t *testing.T) {
		rec := &fakeRecorder{err: errors.New("artifact limit reached")}
		resp := runPublish(t, NewPublishArtifactTool(rec), ctxWork, `{"path":"report.csv"}`)
		if !resp.IsError {
			t.Fatalf("expected error surfaced from recorder, got %q", resp.Content)
		}
	})
}
