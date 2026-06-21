package agent

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
)

func TestLoadImageAttachments(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(pngPath, []byte{0x89, 0x50, 0x4e, 0x47}, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	parts, refs := loadImageAttachments([]ImageAttachment{
		{Path: pngPath, MediaType: "image/png", Name: "shot.png"},
		{Path: filepath.Join(dir, "missing.jpg"), MediaType: "image/jpeg", Name: "missing.jpg"},
	})
	if len(parts) != 1 {
		t.Fatalf("expected 1 fantasy.FilePart, got %d", len(parts))
	}
	if parts[0].MediaType != "image/png" {
		t.Errorf("media = %q", parts[0].MediaType)
	}
	if string(parts[0].Data) != string([]byte{0x89, 0x50, 0x4e, 0x47}) {
		t.Errorf("data mismatch")
	}
	if len(refs) != 1 || refs[0].Path != pngPath {
		t.Errorf("refs unexpected: %+v", refs)
	}
}

func TestLoadImageAttachments_DefaultsMimeWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.png")
	_ = os.WriteFile(p, []byte("a"), 0o600)
	parts, refs := loadImageAttachments([]ImageAttachment{{Path: p, Name: "x.png"}})
	if len(parts) != 1 || parts[0].MediaType != "image/png" {
		t.Errorf("expected default media image/png, got %+v", parts)
	}
	if refs[0].MediaType != "image/png" {
		t.Errorf("ref media = %q", refs[0].MediaType)
	}
}

// TestReplayHistory_PreservesUserImages persists a user message with an
// attached image, replays it, and verifies a fantasy.FilePart accompanies
// the user TextPart on the next turn.
func TestReplayHistory_PreservesUserImages(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "asset.png")
	if err := os.WriteFile(imgPath, []byte("PIXEL_DATA"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{
			Text: "what's in this image?",
			Images: []ImageRefMeta{
				{Path: imgPath, MediaType: "image/png", Name: "asset.png"},
			},
		}),
		mustEntry("assistant", "text", TextContent{Text: "a square"}),
	}
	msgs, err := replayHistory(entries)
	if err != nil {
		t.Fatalf("replayHistory: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	user := msgs[0]
	if user.Role != fantasy.MessageRoleUser {
		t.Errorf("expected user role")
	}
	var hasFile bool
	var hasText bool
	for _, p := range user.Content {
		if _, ok := fantasy.AsMessagePart[fantasy.TextPart](p); ok {
			hasText = true
		}
		if fp, ok := fantasy.AsMessagePart[fantasy.FilePart](p); ok {
			hasFile = true
			if fp.MediaType != "image/png" {
				t.Errorf("media = %q", fp.MediaType)
			}
			if string(fp.Data) != "PIXEL_DATA" {
				t.Errorf("data mismatch")
			}
		}
	}
	if !hasText || !hasFile {
		t.Errorf("user message missing parts: text=%v file=%v", hasText, hasFile)
	}
}

func TestReplayHistory_ImageMissingFileGracefullyDropped(t *testing.T) {
	entries := []HistoryEntry{
		mustEntry("user", "text", TextContent{
			Text: "hi",
			Images: []ImageRefMeta{
				{Path: "/nonexistent/foo.png", MediaType: "image/png", Name: "foo.png"},
			},
		}),
	}
	msgs, err := replayHistory(entries)
	if err != nil {
		t.Fatalf("replayHistory should not fail on missing image: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	for _, p := range msgs[0].Content {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](p); ok {
			t.Error("missing file should not produce a FilePart")
		}
	}
}
