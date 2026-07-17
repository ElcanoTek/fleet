package tools

import (
	"context"
	"errors"
	"testing"
)

type recordingOutputStager struct {
	tool, call, format string
	content            string
}

const recordingArtifactPath = ".fleet/tool-output/slot-00/artifact-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.txt"

func (s *recordingOutputStager) StageModelOutputArtifact(_ context.Context, tool, call, format, content string) (string, error) {
	s.tool, s.call, s.format = tool, call, format
	s.content = content
	return recordingArtifactPath, nil
}

func TestStageModelOutputArtifactContextSeam(t *testing.T) {
	stager := &recordingOutputStager{}
	ctx := WithModelOutputArtifactStager(context.Background(), stager)
	path, err := StageModelOutputArtifact(ctx, "run_python", "call-1", "json", `{"safe":true}`)
	if err != nil {
		t.Fatalf("StageModelOutputArtifact: %v", err)
	}
	if path != recordingArtifactPath || stager.tool != "run_python" || stager.call != "call-1" || stager.format != "json" || stager.content != `{"safe":true}` {
		t.Fatalf("stager did not receive governed payload: path=%q stager=%+v", path, stager)
	}
}

func TestStageModelOutputArtifactRequiresInstalledSandboxScope(t *testing.T) {
	_, err := StageModelOutputArtifact(context.Background(), "tool", "call", "text", "safe")
	if !errors.Is(err, ErrModelOutputArtifactScope) {
		t.Fatalf("without sandbox scope err=%v, want %v", err, ErrModelOutputArtifactScope)
	}
}
