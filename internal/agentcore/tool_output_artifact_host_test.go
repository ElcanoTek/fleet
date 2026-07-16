//go:build fleet_host_executor

package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	fleettools "github.com/ElcanoTek/fleet/internal/tools"
)

func TestSandboxedCommandArtifactRecoveredThroughBoundedViewFile(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(64 * 1024)
	t.Setenv("FLEET_AUDIT_DIR", t.TempDir())

	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	sb.SetDefaultWorkingDir(root)
	ctx := fleettools.WithForcedWorkingDir(context.Background(), root)
	ctx, release, err := fleettools.WithSandboxModelOutputArtifacts(ctx, sb, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	registered, err := buildFantasyTools([]fantasy.AgentTool{
		fleettools.NewBashTool(sb),
		fleettools.NewViewFileTool(sb),
	}, nil, nil, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	bash := findRegisteredTool(registered, "bash")
	view := findRegisteredTool(registered, "view_file")
	if bash == nil || view == nil {
		t.Fatal("sandboxed bash/view_file tools were not registered")
	}

	commandInput, _ := json.Marshal(map[string]any{
		"command": `python3 -c "print('sandboxed governed ' + 'ordinary row ' * 20000)"`,
	})
	bounded, err := bash.Run(ctx, fantasy.ToolCall{ID: "large-bash", Name: "bash", Input: string(commandInput)})
	if err != nil || bounded.IsError {
		t.Fatalf("bash: err=%v response=%s", err, bounded.Content)
	}
	var envelope toolOutputEnvelope
	if err := json.Unmarshal([]byte(bounded.Content), &envelope); err != nil {
		t.Fatalf("bounded bash result is not valid JSON: %v\n%s", err, bounded.Content)
	}
	if envelope.ArtifactPath == "" || envelope.OriginalBytes <= 200000 || len(bounded.Content) > 64*1024 {
		t.Fatalf("unexpected bounded bash envelope: %+v bytes=%d", envelope, len(bounded.Content))
	}
	if !strings.Contains(envelope.RecoveryAction, "32768 bytes") {
		t.Fatalf("recovery action lacks safe view_file chunk margin: %q", envelope.RecoveryAction)
	}

	// Read the artifact back only through the registered, model-visible
	// view_file tool. Each chunk stays beneath the same output boundary, proving
	// the recovery action does not depend on a host-only path or an exemption.
	const chunkSize = 32 * 1024
	const continuation = "\n... (reading limit of "
	var recovered strings.Builder
	for offset := int64(0); ; {
		input, _ := json.Marshal(map[string]any{
			"path": envelope.ArtifactPath, "offset": offset, "limit": chunkSize,
		})
		part, runErr := view.Run(ctx, fantasy.ToolCall{ID: fmt.Sprintf("view-%d", offset), Name: "view_file", Input: string(input)})
		if runErr != nil || part.IsError {
			t.Fatalf("view_file offset %d: err=%v response=%s", offset, runErr, part.Content)
		}
		chunk := part.Content
		if marker := strings.Index(chunk, "\n\n(file metadata: sha256="); marker >= 0 {
			chunk = chunk[:marker]
		}
		more := false
		if marker := strings.Index(chunk, continuation); marker >= 0 {
			chunk = chunk[:marker]
			more = true
		}
		recovered.WriteString(chunk)
		offset += int64(len(chunk))
		if !more {
			break
		}
		if offset > int64(envelope.OriginalBytes) {
			t.Fatalf("view_file recovery exceeded reported artifact size")
		}
	}
	if recovered.Len() != envelope.OriginalBytes {
		t.Fatalf("recovered %d bytes, envelope reported %d", recovered.Len(), envelope.OriginalBytes)
	}
	var full struct {
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(recovered.String()), &full); err != nil {
		t.Fatalf("recovered artifact is not the original bash JSON: %v", err)
	}
	if !strings.HasPrefix(full.Stdout, "sandboxed governed ") || len(full.Stdout) < 200000 {
		t.Fatalf("recovered bash stdout is incomplete: %d bytes", len(full.Stdout))
	}
}
