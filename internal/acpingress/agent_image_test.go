package acpingress

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"
)

// TestPromptImageBlockDecodedToWorkspace pins the TRANSPORT contract for issue
// #47: an ACP image prompt block is decoded to a conversation-workspace file
// (already mounted into the sandbox) and surfaced on TurnInput.ImageAttachments —
// the SAME field the web path populates, which the agent package's
// loadImageAttachments (covered by its own image_input_test.go) turns into a
// fantasy.FilePart for the model. This proves ingress → TurnInput → disk; model
// delivery from there is the agent package's tested concern.
func TestPromptImageBlockDecodedToWorkspace(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	model := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("I can see the image")}}
	w := setup(t, model, baseCfg())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The Image capability is advertised so an editor knows it may attach images.
	initResp, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !initResp.AgentCapabilities.PromptCapabilities.Image {
		t.Fatal("PromptCapabilities.Image must be advertised true")
	}

	sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-but-decodable-image-bytes")
	b64 := base64.StdEncoding.EncodeToString(imgBytes)
	resp, err := w.client.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("describe this"), acp.ImageBlock(b64, "image/png")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", resp.StopReason)
	}

	atts := w.engine.lastTurnInput().ImageAttachments
	if len(atts) != 1 {
		t.Fatalf("ImageAttachments = %d, want 1", len(atts))
	}
	if atts[0].MediaType != "image/png" {
		t.Errorf("MediaType = %q, want image/png", atts[0].MediaType)
	}
	if atts[0].Name == "" {
		t.Error("attachment Name must be set")
	}
	got, err := os.ReadFile(atts[0].Path)
	if err != nil {
		t.Fatalf("decoded image not written to the workspace: %v", err)
	}
	if !bytes.Equal(got, imgBytes) {
		t.Errorf("workspace file bytes do not round-trip the decoded image")
	}
}

// TestPromptImageBlockSilentDegrade: a non-image MIME and an undecodable base64
// block are dropped non-fatally — no ImageAttachment, the turn still completes.
func TestPromptImageBlockSilentDegrade(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())

	cases := []struct {
		name  string
		block acp.ContentBlock
	}{
		{"non-image mime", acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("x")), "application/pdf")},
		{"bad base64", acp.ContentBlock{Image: &acp.ContentBlockImage{Data: "!!!not-base64!!!", MimeType: "image/png", Type: "image"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			model := &scriptedModel{rounds: [][]fantasy.StreamPart{textRound("ok")}}
			w := setup(t, model, baseCfg())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := w.client.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			sess, err := w.client.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			resp, err := w.client.Prompt(ctx, acp.PromptRequest{
				SessionId: sess.SessionId,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hi"), c.block},
			})
			if err != nil {
				t.Fatalf("prompt: %v", err)
			}
			if resp.StopReason != acp.StopReasonEndTurn {
				t.Fatalf("stop reason = %q, want end_turn (drop must be non-fatal)", resp.StopReason)
			}
			if atts := w.engine.lastTurnInput().ImageAttachments; len(atts) != 0 {
				t.Fatalf("ImageAttachments = %d, want 0 (block should be dropped)", len(atts))
			}
		})
	}
}

// TestExtForImageMIME: extension is derived from the VALIDATED MIME, with a png
// fallback — never from the raw wire string.
func TestExtForImageMIME(t *testing.T) {
	cases := map[string]string{
		"image/png":  ".png",
		"image/jpeg": ".jpg",
		"image/gif":  ".gif",
		"image/webp": ".webp",
		"image/bmp":  ".png", // unknown image MIME → safe default
	}
	for mime, want := range cases {
		if got := extForImageMIME(mime); got != want {
			t.Errorf("extForImageMIME(%q) = %q, want %q", mime, got, want)
		}
	}
}
