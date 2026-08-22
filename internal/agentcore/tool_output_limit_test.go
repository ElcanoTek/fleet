package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"

	fleettools "github.com/ElcanoTek/fleet/internal/tools"
)

type captureOutputStager struct {
	content string
}

func (s *captureOutputStager) StageModelOutputArtifact(_ context.Context, _, _, _, content string) (string, error) {
	s.content = content
	return testArtifactPath(0), nil
}

func testArtifactPath(slot int) string {
	return fmt.Sprintf(".fleet/tool-output/slot-%02d/artifact-%s.txt", slot, strings.Repeat("a", 64))
}

func TestApplyOutputCeiling(t *testing.T) {
	if out, trunc := applyOutputCeiling("short", 100); trunc || out != "short" {
		t.Errorf("under limit: trunc=%v out=%q, want false/short", trunc, out)
	}

	// Zero used to disable the cap. It now selects the safe default.
	big := strings.Repeat("not binary prose ", defaultMaxToolOutputBytes)
	out, trunc := applyOutputCeiling(big, 0)
	if !trunc {
		t.Fatal("limit 0 must not disable truncation")
	}
	if len(out) > defaultMaxToolOutputBytes {
		t.Fatalf("zero-normalized result = %d bytes, want <= %d", len(out), defaultMaxToolOutputBytes)
	}
	if !strings.Contains(out, "original_bytes") || !strings.Contains(out, "recovery_action") {
		t.Fatalf("bounded output is missing size/recovery metadata: %q", out[:min(len(out), 200)])
	}

	content := strings.Repeat("alpha ", 5000) + "MIDDLE_NEEDLE" + strings.Repeat(" omega", 5000)
	out, trunc = applyOutputCeiling(content, 2000)
	if !trunc {
		t.Fatal("expected truncation over the limit")
	}
	if len(out) > 2000 {
		t.Fatalf("rendered envelope exceeds operational limit: %d", len(out))
	}
	if strings.Contains(out, "MIDDLE_NEEDLE") {
		t.Error("the middle should have been dropped")
	}
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "omega") {
		t.Error("text preview should retain useful head and tail context")
	}
}

func TestApplyOutputCeiling_UTF8Safe(t *testing.T) {
	content := strings.Repeat("™ words ", 4000)
	out, trunc := applyOutputCeiling(content, 3000)
	if !trunc {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(out) {
		t.Error("truncated output must remain valid UTF-8")
	}
}

func TestMaxToolOutputBytes_Normalization(t *testing.T) {
	if got := normalizeToolOutputLimit(0); got != defaultMaxToolOutputBytes {
		t.Fatalf("zero = %d, want safe default %d", got, defaultMaxToolOutputBytes)
	}
	if got := normalizeToolOutputLimit(-99); got != defaultMaxToolOutputBytes {
		t.Fatalf("negative = %d, want safe default %d", got, defaultMaxToolOutputBytes)
	}
	if got := normalizeToolOutputLimit(1); got != MinMaxToolOutputBytes {
		t.Fatalf("tiny = %d, want envelope floor %d", got, MinMaxToolOutputBytes)
	}
	if got := normalizeToolOutputLimit(HardMaxToolOutputBytes * 100); got != HardMaxToolOutputBytes {
		t.Fatalf("oversized = %d, want hard cap %d", got, HardMaxToolOutputBytes)
	}
}

func TestBoundModelVisibleToolResponse_JSONPythonVarArtifactRoundTrip(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(4096)
	stager := &captureOutputStager{}
	ctx := fleettools.WithModelOutputArtifactStager(context.Background(), stager)

	// Larger than both the operational setting and the non-disableable hard cap.
	original := `{"status":"success","vars":{"payload":"` + strings.Repeat("QUJD", HardMaxToolOutputBytes) + `"},"stdout":"done"}`
	resp := boundModelVisibleToolResponse(ctx, "run_python", "python-call-1", fantasy.NewTextResponse(original))
	if len(resp.Content) > 4096 {
		t.Fatalf("model-visible Python response = %d bytes, want <= 4096", len(resp.Content))
	}
	var envelope toolOutputEnvelope
	if err := json.Unmarshal([]byte(resp.Content), &envelope); err != nil {
		t.Fatalf("truncated structured response must remain valid JSON: %v\n%s", err, resp.Content)
	}
	if !envelope.Truncated || !envelope.BinarySuppressed {
		t.Fatalf("unexpected Python envelope: %+v", envelope)
	}
	if envelope.OriginalBytes != len(original) || envelope.ShownBytes != 0 {
		t.Fatalf("size accounting = original %d shown %d, want %d/0", envelope.OriginalBytes, envelope.ShownBytes, len(original))
	}
	if envelope.ArtifactPath != "" || strings.Contains(envelope.RecoveryAction, "view_file") || !strings.Contains(envelope.RecoveryAction, "workspace-relative path") {
		t.Fatalf("binary output advertised recursive artifact recovery: %+v", envelope)
	}
	if strings.Contains(resp.Content, strings.Repeat("QUJD", 16)) {
		t.Fatal("base64 preview leaked into model-visible Python envelope")
	}

	if stager.content != "" {
		t.Fatalf("encoded binary unexpectedly consumed an artifact slot: %d bytes", len(stager.content))
	}
}

func TestModelOutputArtifactReceivesOnlyGovernedContent(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(2048)
	const secret = "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	original := "export OPENAI_API_KEY=" + secret + "\n" + strings.Repeat("safe report row\n", 1000)
	tool := fantasy.NewAgentTool("governed_large", "large governed output",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(original), nil
		})
	registered, err := buildFantasyTools([]fantasy.AgentTool{tool}, nil, nil, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stager := &captureOutputStager{}
	ctx := fleettools.WithModelOutputArtifactStager(context.Background(), stager)
	resp, err := findRegisteredTool(registered, "governed_large").Run(ctx, fantasy.ToolCall{ID: "governed-call", Input: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stager.content, secret) || !strings.Contains(stager.content, "[REDACTED]") {
		t.Fatalf("artifact was staged before secret governance: %.200s", stager.content)
	}
	if strings.Contains(resp.Content, secret) {
		t.Fatal("model-visible envelope leaked the secret")
	}
}

func TestBoundModelOutputToolsGovernsRawAuxiliaryRouteBeforeArtifact(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(2048)
	const secret = "sk-ZYXWVUTSRQPONMLKJIHGFEDCBA987654"
	original := "OPENROUTER_API_KEY=" + secret + "\n" + strings.Repeat("auxiliary safe row\n", 1000)
	raw := fantasy.NewAgentTool("auxiliary_raw", "raw auxiliary route",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(original), nil
		})
	stager := &captureOutputStager{}
	ctx := fleettools.WithModelOutputArtifactStager(context.Background(), stager)
	resp, err := BoundModelOutputTools([]fantasy.AgentTool{raw})[0].Run(ctx, fantasy.ToolCall{ID: "aux-call", Input: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stager.content, secret) || !strings.Contains(stager.content, "[REDACTED]") {
		t.Fatalf("raw auxiliary output reached retention before governance: %.200s", stager.content)
	}
	if strings.Contains(resp.Content, secret) || len(resp.Content) > 2048 {
		t.Fatalf("raw auxiliary route bypassed final boundary: bytes=%d content=%.200s", len(resp.Content), resp.Content)
	}
}

func TestFinalBoundaryDoesNotRunGuardrailTwiceForGovernedRoutes(t *testing.T) {
	detector := &fakeGuardrailDetector{}
	SetGuardrail(true, false, "observe", "prompt-injection", detector)
	t.Cleanup(func() { SetGuardrail(false, false, "off", "", nil) })
	tool := fantasy.NewAgentTool("governed_once", "guardrail call count",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ordinary result"), nil
		})
	registered, err := buildFantasyTools([]fantasy.AgentTool{tool}, nil, nil, nil, nil, nil, nil, toolBuildConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := findRegisteredTool(registered, "governed_once").Run(context.Background(), fantasy.ToolCall{ID: "once", Input: "{}"}); err != nil {
		t.Fatal(err)
	}
	if detector.calls != 1 {
		t.Fatalf("governed output was screened %d times, want exactly once", detector.calls)
	}
}

func TestBoundModelVisibleToolResponse_MediaNeverInlinesBase64(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(1024)
	data := []byte(strings.Repeat("binary", HardMaxToolOutputBytes))
	input := fantasy.NewImageResponse(data, "image/png")
	input.Content = "governed caption"
	resp := boundModelVisibleToolResponse(context.Background(), "media_tool", "media-1", input)
	if len(resp.Content) > 1024 || len(resp.Data) != 0 || resp.Type != "text" {
		t.Fatalf("media was not converted to a bounded text envelope: type=%s text=%d data=%d", resp.Type, len(resp.Content), len(resp.Data))
	}
	var envelope toolOutputEnvelope
	if err := json.Unmarshal([]byte(resp.Content), &envelope); err != nil {
		t.Fatalf("media envelope invalid JSON: %v", err)
	}
	if !envelope.BinarySuppressed || envelope.ShownBytes != 0 || envelope.MediaType != "image/png" || envelope.OriginalBytes != len(data)+len(input.Content) {
		t.Fatalf("unexpected media envelope: %+v", envelope)
	}
}

func TestBoundModelVisibleToolResponse_SuppressesURLBase64AndEscapedControls(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(1024)
	for name, content := range map[string]string{
		"url-safe base64": `{"payload":"` + strings.Repeat("Ab-_", 300) + `"}`,
		"escaped control": `{"payload":"\u0000","rows":"` + strings.Repeat("ordinary row ", 300) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp := boundModelVisibleToolResponse(context.Background(), "run_python", "call", fantasy.NewTextResponse(content))
			var envelope toolOutputEnvelope
			if err := json.Unmarshal([]byte(resp.Content), &envelope); err != nil {
				t.Fatalf("invalid JSON envelope: %v", err)
			}
			if !envelope.BinarySuppressed || envelope.ShownBytes != 0 || envelope.Preview != "" {
				t.Fatalf("binary/control preview leaked: %+v", envelope)
			}
		})
	}
}

func TestBoundModelVisibleToolResponse_LargeInputHasBoundedAllocation(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(4096)
	content := `{"rows":"` + strings.Repeat("ordinary row value ", (64<<20)/19) + `"}`
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	resp := boundModelVisibleToolResponse(context.Background(), "large_json", "call", fantasy.NewTextResponse(content))
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(content)
	if len(resp.Content) > 4096 || !json.Valid([]byte(resp.Content)) {
		t.Fatalf("large result boundary invalid: bytes=%d", len(resp.Content))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16<<20 {
		t.Fatalf("bounding a preallocated 64MiB result allocated %d additional bytes", allocated)
	}
}

func TestModelOutputBoundary_BoundsReturnedGoError(t *testing.T) {
	t.Cleanup(func() { SetMaxToolOutputBytes(-1) })
	SetMaxToolOutputBytes(2048)
	original := `{"rows":"` + strings.Repeat("ordinary governed error row ", 2000) + `"}`
	cause := errors.New(original)
	wrapped := withModelOutputBoundary(fantasy.NewAgentTool("failing_tool", "returns a large transport error",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, cause
		}))

	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{ID: "error-call", Name: "failing_tool", Input: "{}"})
	if !errors.Is(err, cause) {
		t.Fatalf("bounded error must preserve its cause: %v", err)
	}
	if len(err.Error()) > 2048 || resp.Content != err.Error() || !resp.IsError {
		t.Fatalf("returned error bypassed boundary: err=%d response=%d is_error=%t", len(err.Error()), len(resp.Content), resp.IsError)
	}
	var envelope toolOutputEnvelope
	if unmarshalErr := json.Unmarshal([]byte(err.Error()), &envelope); unmarshalErr != nil {
		t.Fatalf("structured Go error must become valid bounded JSON: %v\n%s", unmarshalErr, err.Error())
	}
	if envelope.OriginalBytes != len(original) || envelope.RecoveryAction == "" {
		t.Fatalf("bounded Go error has dishonest metadata: %+v", envelope)
	}
}

// TestLooksLikeJSONDocument_TieredClassification pins both halves of the
// format classifier. The exact tier must keep answering precisely what
// json.Valid answered before the cap existed — that is the tier every
// realistically-sized tool result lands in, so a regression there would
// silently relabel ordinary output. The sniff tier only has to be structural,
// and must NOT fall back to a full parse, which is what the companion
// allocation test guards.
func TestLooksLikeJSONDocument_TieredClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    bool
	}{
		"empty":                   {"", false},
		"whitespace only":         {"   \n\t ", false},
		"object":                  {`{"a":1}`, true},
		"object with surrounding": {"\n  {\"a\":1}\t\n", true},
		"array":                   {`[1,2,3]`, true},
		"bare string":             {`"hello"`, true},
		"bare number":             {`42`, true},
		"bare null":               {`null`, true},
		"plain prose":             {"ordinary row value", false},
		"truncated object":        {`{"a":1`, false},
		"trailing garbage":        {`{"a":1} trailing`, false},
		"brace-wrapped non-json":  {`{not json at all}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if len(tc.content) > maxExactJSONValidationBytes {
				t.Fatalf("case is meant to exercise the exact tier but is %d bytes", len(tc.content))
			}
			if got := looksLikeJSONDocument(tc.content); got != tc.want {
				t.Errorf("looksLikeJSONDocument(%q) = %v, want %v", tc.content, got, tc.want)
			}
			// The exact tier must agree with the pre-cap implementation.
			exact := json.Valid([]byte(strings.TrimSpace(tc.content)))
			if got := looksLikeJSONDocument(tc.content); got != exact {
				t.Errorf("exact tier diverged from json.Valid for %q: got %v, json.Valid %v", tc.content, got, exact)
			}
		})
	}

	// Above the cap the classifier switches to a structural sniff.
	filler := strings.Repeat("ordinary row value ", (maxExactJSONValidationBytes/19)+64)
	for name, tc := range map[string]struct {
		content string
		want    bool
	}{
		"huge object":       {`{"rows":"` + filler + `"}`, true},
		"huge array":        {`["` + filler + `"]`, true},
		"huge bare string":  {`"` + filler + `"`, true},
		"huge plain text":   {filler, false},
		"huge unterminated": {`{"rows":"` + filler, false},
		// The documented cost of the trade: brace-wrapped but invalid content
		// is labelled json above the cap where json.Valid would have said text.
		"huge brace-wrapped junk": {"{" + filler + "}", true},
	} {
		t.Run(name, func(t *testing.T) {
			if len(tc.content) <= maxExactJSONValidationBytes {
				t.Fatalf("case is meant to exercise the sniff tier but is only %d bytes", len(tc.content))
			}
			if got := looksLikeJSONDocument(tc.content); got != tc.want {
				t.Errorf("looksLikeJSONDocument(<%d bytes>) = %v, want %v", len(tc.content), got, tc.want)
			}
		})
	}
}
