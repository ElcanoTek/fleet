package agent

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// makeNativeWithName returns a no-op tool with the given name. Used to
// stand in for generate_image without having to import the tools package
// (which would make this an integration test, not a unit test of the
// gating logic).
func makeNativeWithName(name string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		name,
		"test probe",
		func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		},
	)
}

func TestNativeOptInGate_Mapping(t *testing.T) {
	if got := nativeOptInGate("generate_image"); got != OptionalNativeImageGenName {
		t.Errorf("nativeOptInGate(generate_image) = %q, want %q", got, OptionalNativeImageGenName)
	}
	for _, n := range []string{"bash", "view_file", "run_python", "task_tracker", "smart_search", ""} {
		if got := nativeOptInGate(n); got != "" {
			t.Errorf("nativeOptInGate(%q) = %q, want empty (always-on tool)", n, got)
		}
	}
}

func TestBuildFantasyTools_GenerateImage_GatedOff(t *testing.T) {
	client := mcp.NewClient()
	orch := newTestOrch()
	native := []fantasy.AgentTool{
		makeTestNative("native_always_on"),
		makeNativeWithName("generate_image"),
	}

	// No opt-in for image_generation -> generate_image MUST be filtered out.
	tools, err := buildFantasyTools(native, client, nil, orch, nil, nil)
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	if !hasToolNamed(tools, "native_always_on") {
		t.Error("always-on native tool should still be registered")
	}
	if hasToolNamed(tools, "generate_image") {
		t.Error("generate_image must be filtered out when image_generation is not opted in")
	}
}

func TestBuildFantasyTools_GenerateImage_GatedOn(t *testing.T) {
	client := mcp.NewClient()
	orch := newTestOrch()
	native := []fantasy.AgentTool{
		makeTestNative("native_always_on"),
		makeNativeWithName("generate_image"),
	}

	// Conversation opted in via the same list used for Optional MCPs.
	enabled := []string{OptionalNativeImageGenName}
	tools, err := buildFantasyTools(native, client, nil, orch, nil, enabled)
	if err != nil {
		t.Fatalf("buildFantasyTools: %v", err)
	}
	if !hasToolNamed(tools, "generate_image") {
		t.Error("generate_image must be registered when image_generation is opted in")
	}
}

func TestOptionalNativeImageGenInfo_CatalogShape(t *testing.T) {
	info := optionalNativeImageGenInfo()
	if info.Name != OptionalNativeImageGenName {
		t.Errorf("name = %q", info.Name)
	}
	if info.DisplayName == "" {
		t.Error("display name should be non-empty for the picker UI")
	}
	if info.ToolCount != 1 || len(info.Tools) != 1 || info.Tools[0] != "generate_image" {
		t.Errorf("unexpected tool listing: %+v", info)
	}
	if info.Tools == nil {
		t.Error("Tools must be non-nil so JSON renders [] not null")
	}
}

func TestBuildOptionalServerMetadata_IncludesImageGen(t *testing.T) {
	m := &Manager{mcpClient: mcp.NewClient()}
	out := m.buildOptionalServerMetadata(map[string]MCPServerSpec{})
	var found bool
	for _, info := range out {
		if info.Name == OptionalNativeImageGenName {
			found = true
			if info.ToolCount != 1 || info.Tools[0] != "generate_image" {
				t.Errorf("image_generation entry malformed: %+v", info)
			}
		}
	}
	if !found {
		t.Errorf("expected image_generation in catalog, got %d entries", len(out))
	}
}

// TestBuildSystemPrompt_ImageGenSectionGated verifies the image-gen section
// of the system prompt only appears when the conversation has opted in.
// Uses the same fixture manager as TestBuildSystemPrompt_Layering.
func TestBuildSystemPrompt_ImageGenSectionGated(t *testing.T) {
	m := fixtureManager(t)
	m.mcpClient = mcp.NewClient()

	off, err := m.buildSystemPrompt("victoria", "conv-x", nil, nil)
	if err != nil {
		t.Fatalf("buildSystemPrompt off: %v", err)
	}
	if strings.Contains(off, "## Image Generation") {
		t.Error("Image Generation section must NOT appear when not opted in")
	}
	if strings.Contains(off, "`generate_image`") {
		t.Error("generate_image must not appear in prompt when not opted in")
	}

	on, err := m.buildSystemPrompt("victoria", "conv-x", nil, []string{OptionalNativeImageGenName})
	if err != nil {
		t.Fatalf("buildSystemPrompt on: %v", err)
	}
	if !strings.Contains(on, "## Image Generation") {
		t.Errorf("Image Generation section must appear when opted in. Output:\n%s", on)
	}
	if !strings.Contains(on, "generate_image") {
		t.Error("opted-in prompt should mention generate_image")
	}
}
