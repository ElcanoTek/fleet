package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// TestBuildMCPDescriptors_NilClient confirms the descriptor builder is nil-safe
// (no MCP client → no descriptors → the agent advertises no MCP surface, which is
// exactly the in-process behavior when nothing is connected).
func TestBuildMCPDescriptors_NilClient(t *testing.T) {
	if got := buildMCPDescriptors(nil, nil, nil, nil); got != nil {
		t.Fatalf("buildMCPDescriptors(nil, ...) = %v, want nil", got)
	}
}

// recordingApprovalStager / recordingMemoryProposer / recordingNoteProposer are
// the host stagers the stageBroker forwards to. They mirror the real stagers'
// signatures so the broker wiring is exercised end-to-end without a DB.
type recordingApprovalStager struct {
	staged     []string
	suggestion string
}

func (a *recordingApprovalStager) Stage(toolName, _, _ string) (string, error) {
	a.staged = append(a.staged, toolName)
	return "appr-1", nil
}
func (a *recordingApprovalStager) StageSuggestion(reason string) (string, string, error) {
	a.suggestion = reason
	return "sug-1", "SUGGESTION_DISPLAYED", nil
}

type recordingMemoryProposer struct{ content string }

func (m *recordingMemoryProposer) Propose(content string) (string, error) {
	m.content = content
	return "mem-1", nil
}

type recordingNoteProposer struct{ slug string }

func (n *recordingNoteProposer) Propose(slug, _, _, _ string) (string, error) {
	n.slug = slug
	return "note-1", nil
}

// TestStageBroker_RoutesToHostStagers proves the stageBroker forwards each
// delegated staging effect to the matching host stager — the seam that keeps the
// DB write + SSE card host-side while the agent's in-loop policy decides WHEN.
func TestStageBroker_RoutesToHostStagers(t *testing.T) {
	appr := &recordingApprovalStager{}
	mem := &recordingMemoryProposer{}
	note := &recordingNoteProposer{}
	b := &stageBroker{approval: appr, memory: mem, note: note}

	if id, err := b.StageApproval("send_email", "c1", `{}`); err != nil || id != "appr-1" {
		t.Fatalf("StageApproval = (%q, %v), want (appr-1, nil)", id, err)
	}
	if len(appr.staged) != 1 || appr.staged[0] != "send_email" {
		t.Fatalf("approval not forwarded: %v", appr.staged)
	}
	if id, msg, err := b.StageSuggestion("be faster"); err != nil || id != "sug-1" || msg == "" {
		t.Fatalf("StageSuggestion = (%q, %q, %v)", id, msg, err)
	}
	if id, err := b.StageMemory("likes blue"); err != nil || id != "mem-1" || mem.content != "likes blue" {
		t.Fatalf("StageMemory = (%q, %v), content=%q", id, err, mem.content)
	}
	if id, err := b.StageNote("s", "T", "B", "R"); err != nil || id != "note-1" || note.slug != "s" {
		t.Fatalf("StageNote = (%q, %v), slug=%q", id, err, note.slug)
	}
}

// TestStageBroker_UnwiredSurfaceFailsClosed proves a delegated request for a
// surface the host has no stager for returns an error (the agent maps it onto the
// same "not wired" agent-facing message the in-process gate produces) — never a
// silent success.
func TestStageBroker_UnwiredSurfaceFailsClosed(t *testing.T) {
	b := &stageBroker{} // no stagers wired
	if _, err := b.StageApproval("send_email", "", ""); !errors.Is(err, errStagerNotWired) {
		t.Fatalf("StageApproval with no stager = %v, want errStagerNotWired", err)
	}
	if _, _, err := b.StageSuggestion("x"); !errors.Is(err, errStagerNotWired) {
		t.Fatalf("StageSuggestion with no stager = %v, want errStagerNotWired", err)
	}
	if _, err := b.StageMemory("x"); !errors.Is(err, errStagerNotWired) {
		t.Fatalf("StageMemory with no stager = %v, want errStagerNotWired", err)
	}
	if _, err := b.StageNote("s", "t", "b", "r"); !errors.Is(err, errStagerNotWired) {
		t.Fatalf("StageNote with no stager = %v, want errStagerNotWired", err)
	}
}

// (The native-acp host broker is agentcore.NewLocalMCPBroker, whose return type IS
// the MCPBroker seam — and acpruntime.MCPBroker aliases it — so interface
// satisfaction is compile-guaranteed by buildACPHostGovernance's assignment; the
// concrete-type assertion lives in agentcore. The host-side round-trip is proved
// in acpruntime's TestACPGovern_MCPDelegatedHostSide.)

// TestBuildACPHostGovernance_ScheduledWiresNoteOnly proves the shared host-side
// governance builder wires the SCHEDULED seam set correctly: only the note
// proposer is wired (approval/memory are interactive-only, matching the in-process
// scheduled policy which wires the note proposer but no approval/memory staging),
// and with no MCP client there are no descriptors and no broker. This is the
// scheduled driver's exact wiring (runScheduledACP), so it pins the parity
// contract that scheduled native-acp stages notes — and only notes — host-side.
func TestBuildACPHostGovernance_ScheduledWiresNoteOnly(t *testing.T) {
	note := &recordingNoteProposer{}
	gov := buildACPHostGovernance(nil, nil, nil, nil, acpStagers{note: note})

	if gov.StagingWired {
		t.Fatalf("scheduled wiring must NOT set StagingWired (approval/memory are interactive-only)")
	}
	if !gov.NoteProposerWired {
		t.Fatalf("scheduled wiring must set NoteProposerWired when a note proposer is present")
	}
	if gov.StageBroker == nil {
		t.Fatalf("a note proposer must produce a StageBroker so propose_note delegates host-side")
	}
	// No MCP client → no descriptors and no broker (the agent advertises no MCP
	// surface), exactly as the in-process scheduled path with no servers bound.
	if gov.MCPDescriptors != nil {
		t.Fatalf("no client → no MCP descriptors, got %v", gov.MCPDescriptors)
	}
	if gov.MCPBroker != nil {
		t.Fatalf("no client → no MCP broker")
	}

	// The wired note proposer is the one the broker forwards to (the EFFECT stays
	// host-side; only the public proposal rides the seam).
	if id, err := gov.StageBroker.StageNote("s", "T", "B", "R"); err != nil || id != "note-1" || note.slug != "s" {
		t.Fatalf("StageNote = (%q, %v), slug=%q; want (note-1, nil), slug=s", id, err, note.slug)
	}
	// An approval request fails closed (interactive-only surface, unwired here).
	if _, err := gov.StageBroker.StageApproval("send_email", "", ""); !errors.Is(err, errStagerNotWired) {
		t.Fatalf("scheduled StageApproval = %v, want errStagerNotWired (interactive-only)", err)
	}
}

// TestBuildACPHostGovernance_InteractiveWiresAllStagers proves the SAME shared
// builder wires all three stagers for the interactive driver — the DRY guarantee
// that both drivers build the identical seam set, differing only in which stagers
// they pass.
func TestBuildACPHostGovernance_InteractiveWiresAllStagers(t *testing.T) {
	appr := &recordingApprovalStager{}
	mem := &recordingMemoryProposer{}
	note := &recordingNoteProposer{}
	gov := buildACPHostGovernance(nil, nil, nil, nil, acpStagers{approval: appr, memory: mem, note: note})

	if !gov.StagingWired {
		t.Fatalf("interactive wiring must set StagingWired when approval/memory are present")
	}
	if !gov.NoteProposerWired {
		t.Fatalf("interactive wiring must set NoteProposerWired when a note proposer is present")
	}
	if gov.StageBroker == nil {
		t.Fatalf("stagers present must produce a StageBroker")
	}
}

// TestBuildACPHostGovernance_NoStagersInert proves the builder leaves the staging
// seam inert when no stagers are wired (matching an in-process run with no stagers
// — the agent reports "not wired" identically).
func TestBuildACPHostGovernance_NoStagersInert(t *testing.T) {
	gov := buildACPHostGovernance(nil, nil, nil, nil, acpStagers{})
	if gov.StagingWired || gov.NoteProposerWired || gov.StageBroker != nil {
		t.Fatalf("no stagers → fully inert staging seam, got StagingWired=%v NoteProposerWired=%v broker=%v",
			gov.StagingWired, gov.NoteProposerWired, gov.StageBroker)
	}
}

// TestMCPBroker_FastIOInlineUploadRejectedHostSide proves the host broker applies
// the fast.io inline-base64 pre-guard: an oversized inline upload is rejected
// BEFORE the wire (the client is never reached, so a nil client is safe here),
// with the rejection surfaced as a tool-level error. Since the host broker is now
// agentcore.NewLocalMCPBroker — the SAME implementation the in-process mcpTool
// calls — this is by construction identical to the in-process path.
func TestMCPBroker_FastIOInlineUploadRejectedHostSide(t *testing.T) {
	b := agentcore.NewLocalMCPBroker(nil, agentcore.DefaultRemediationHints) // guard fires before any client call
	bigPayload := strings.Repeat("A", 64*1024)
	text, isErr, err := b.CallMCP(context.Background(), "fast_io", "upload", map[string]any{
		"action":         "stream_upload",
		"content_base64": bigPayload,
	})
	if err != nil {
		t.Fatalf("CallMCP returned a transport error, want a tool-level reject: %v", err)
	}
	if !isErr {
		t.Fatalf("oversized fast.io inline upload should be rejected (isErr=true)")
	}
	if !strings.Contains(text, "Rejected locally") {
		t.Fatalf("reject hint = %q, want the in-process 'Rejected locally' guard message", text)
	}
}
