package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/metrics"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// The model-visible output boundary has two limits. Operators may lower the
// operational limit, but no configuration can raise or disable the hard one.
// Keeping the hard maximum explicit also bounds future AgentTool routes that do
// not have a useful tool-specific production cap of their own.
const (
	defaultMaxToolOutputBytes = 64 * 1024
	// HardMaxToolOutputBytes is the non-disableable maximum for one rendered
	// tool result, including its truncation envelope.
	HardMaxToolOutputBytes = 128 * 1024
	// MinMaxToolOutputBytes leaves enough room for a valid structured envelope.
	// Smaller legacy env values are clamped rather than producing invalid JSON.
	MinMaxToolOutputBytes = 1024
)

var maxToolOutputBytesOnce struct {
	sync.Once
	v int
}

// maxToolOutputOverride is the admin-settings live override; nil means use the
// environment/default. The resolved value is normalized on every read so even
// a stale persisted zero or an oversized override cannot bypass the hard cap.
var maxToolOutputOverride atomic.Pointer[int]

// SetMaxToolOutputBytes installs the process-wide admin override. A negative
// value clears it; zero selects the safe default (it no longer disables the
// boundary), and positive values are clamped to the documented range.
func SetMaxToolOutputBytes(n int) {
	if n < 0 {
		maxToolOutputOverride.Store(nil)
		return
	}
	maxToolOutputOverride.Store(&n)
}

// EnvMaxToolOutputBytes returns the effective, normalized env-derived ceiling
// so the admin panel reports the value Fleet actually enforces.
func EnvMaxToolOutputBytes() int { return normalizeToolOutputLimit(envMaxToolOutputBytes()) }

func maxToolOutputBytes() int {
	if p := maxToolOutputOverride.Load(); p != nil {
		return normalizeToolOutputLimit(*p)
	}
	return normalizeToolOutputLimit(envMaxToolOutputBytes())
}

func normalizeToolOutputLimit(n int) int {
	if n <= 0 {
		return defaultMaxToolOutputBytes
	}
	if n < MinMaxToolOutputBytes {
		return MinMaxToolOutputBytes
	}
	if n > HardMaxToolOutputBytes {
		return HardMaxToolOutputBytes
	}
	return n
}

// applyOutputCeiling is the pure text-only form used by low-level tests and
// callers that do not have a ToolResponse/context. Production AgentTool routes
// use boundModelVisibleToolResponse so they also get structured JSON handling,
// artifact recovery, and media suppression. A non-positive limit selects the
// safe default; it never disables truncation.
func applyOutputCeiling(content string, limit int) (string, bool) {
	limit = normalizeToolOutputLimit(limit)
	encodedBinary := containsEncodedBinary(content)
	if len(content) <= limit && !encodedBinary {
		return content, false
	}
	env := toolOutputEnvelope{
		Tool:             "tool",
		OriginalBytes:    len(content),
		Truncated:        true,
		Format:           "text",
		BinarySuppressed: encodedBinary,
		RecoveryAction:   "Re-run the tool with narrower output.",
	}
	return renderTextEnvelope(env, content, limit), true
}

func envMaxToolOutputBytes() int {
	maxToolOutputBytesOnce.Do(func() {
		maxToolOutputBytesOnce.v = defaultMaxToolOutputBytes
		if s := os.Getenv("FLEET_MAX_TOOL_OUTPUT_BYTES"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				maxToolOutputBytesOnce.v = n
			}
		}
	})
	return maxToolOutputBytesOnce.v
}

// modelOutputBoundaryTool is the final rendering boundary around an AgentTool.
// Registration normally wraps it outside the policy/redaction layers. It still
// repeats the idempotent output-governance chain itself so auxiliary calls and
// any future raw registration route cannot stage ungoverned bytes. The same
// wrapper is used for native, loader, direct/deferred MCP, disclosure-bridge,
// and media responses.
type modelOutputBoundaryTool struct {
	inner fantasy.AgentTool
	info  fantasy.ToolInfo
}

type outputGovernanceState struct{ governed atomic.Bool }
type outputGovernanceStateKey struct{}

func ensureOutputGovernanceState(ctx context.Context) context.Context {
	if state, ok := ctx.Value(outputGovernanceStateKey{}).(*outputGovernanceState); ok && state != nil {
		return ctx
	}
	return context.WithValue(ctx, outputGovernanceStateKey{}, &outputGovernanceState{})
}

func markOutputGoverned(ctx context.Context) {
	if state, ok := ctx.Value(outputGovernanceStateKey{}).(*outputGovernanceState); ok && state != nil {
		state.governed.Store(true)
	}
}

func outputAlreadyGoverned(ctx context.Context) bool {
	state, ok := ctx.Value(outputGovernanceStateKey{}).(*outputGovernanceState)
	return ok && state != nil && state.governed.Load()
}

type boundedModelToolError struct {
	cause   error
	message string
}

func (e *boundedModelToolError) Error() string { return e.message }
func (e *boundedModelToolError) Unwrap() error { return e.cause }

func (g *modelOutputBoundaryTool) Info() fantasy.ToolInfo { return g.info }
func (g *modelOutputBoundaryTool) ProviderOptions() fantasy.ProviderOptions {
	return g.inner.ProviderOptions()
}
func (g *modelOutputBoundaryTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	g.inner.SetProviderOptions(opts)
}
func (g *modelOutputBoundaryTool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	ctx = ensureOutputGovernanceState(ctx)
	resp, err := g.inner.Run(ctx, params)
	name := g.info.Name
	if err != nil {
		// Fantasy stores a non-nil Go error directly as the tool result and ignores
		// ToolResponse.Content. Govern and bound Error() here as well, otherwise a
		// tool (or future adapter) could bypass the final boundary by returning its
		// oversized payload as an error. Unwrap preserves errors.Is/As for callers.
		if !outputAlreadyGoverned(ctx) || resp.Content == "" {
			resp.Type = "text"
			resp.Content = err.Error()
			resp.Data = nil
			resp.MediaType = ""
			resp.IsError = true
			resp = governFinalToolResponse(ctx, name, resp)
			markOutputGoverned(ctx)
		} else {
			// An inner policy wrapper already converted this error into the exact
			// governed bytes it recorded. Preserve those bytes at the outer history
			// boundary instead of reconstructing the raw error and drifting audit.
			resp.Type = "text"
			resp.Data = nil
			resp.MediaType = ""
			resp.IsError = true
		}
		resp = boundModelVisibleToolResponse(ctx, name, params.ID, resp)
		return resp, &boundedModelToolError{cause: err, message: resp.Content}
	}
	if !outputAlreadyGoverned(ctx) {
		resp = governFinalToolResponse(ctx, name, resp)
		markOutputGoverned(ctx)
	}
	resp = boundModelVisibleToolResponse(ctx, name, params.ID, resp)
	return resp, err
}

// governFinalToolResponse is intentionally idempotent with the inner route
// wrappers. Keeping it at the actual history boundary makes governance-before-
// retention a structural property rather than an assumption about callers.
func governFinalToolResponse(ctx context.Context, toolName string, resp fantasy.ToolResponse) fantasy.ToolResponse {
	if resp.Content == "" {
		return resp
	}
	var blocked bool
	resp.Content, blocked = governToolOutput(ctx, toolName, resp.Content)
	if blocked {
		resp.IsError = true
	}
	return resp
}

func withModelOutputBoundary(t fantasy.AgentTool) fantasy.AgentTool {
	if t == nil {
		return nil
	}
	if _, ok := t.(*modelOutputBoundaryTool); ok {
		return t
	}
	// Fantasy's typed tool computes and caches its schema lazily in Info(). Cache
	// the immutable result while registration is single-threaded; calling Info
	// for the first time from parallel Run calls races inside that upstream cache.
	return &modelOutputBoundaryTool{inner: t, info: t.Info()}
}

// BoundModelOutputTools installs the final rendering boundary used by
// agentcore.Run and by auxiliary Fantasy calls that replay its governed roster.
// It does not add an authorization path; callers remain responsible for
// supplying tools that already passed the run's registration gates.
func BoundModelOutputTools(in []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(in))
	for _, t := range in {
		if t != nil {
			out = append(out, withModelOutputBoundary(t))
		}
	}
	return out
}

// GovernAndBoundModelVisibleToolText applies the same post-execution boundary
// to one-shot tool paths that intentionally run outside Fantasy's agent loop,
// notably human-approved actions. The returned text is safe to persist/replay;
// blocked governance decisions are reflected in the returned error flag.
func GovernAndBoundModelVisibleToolText(ctx context.Context, toolName, toolCallID, content string, isError bool) (string, bool) {
	resp := fantasy.NewTextResponse(content)
	resp.IsError = isError
	resp = governFinalToolResponse(ctx, toolName, resp)
	resp = boundModelVisibleToolResponse(ctx, toolName, toolCallID, resp)
	return resp.Content, resp.IsError
}

type toolOutputEnvelope struct {
	FleetEnvelope    string `json:"_fleet_envelope,omitempty"`
	Tool             string `json:"tool"`
	OriginalBytes    int    `json:"original_bytes"`
	ShownBytes       int    `json:"shown_bytes"`
	Truncated        bool   `json:"truncated"`
	Format           string `json:"format"`
	Preview          string `json:"preview,omitempty"`
	BinarySuppressed bool   `json:"binary_suppressed,omitempty"`
	MediaType        string `json:"media_type,omitempty"`
	ArtifactPath     string `json:"artifact_path,omitempty"`
	RecoveryAction   string `json:"recovery_action"`
}

const fleetToolOutputEnvelopeV1 = "tool-output/v1"

// boundModelVisibleToolResponse converts oversized or binary responses into a
// compact envelope whose TOTAL encoded size is within the effective limit. Text
// artifacts are staged only here, after the inner wrapper's redaction, PII, and
// guardrail decisions. Media bytes are never base64-encoded into model context.
func boundModelVisibleToolResponse(ctx context.Context, toolName, toolCallID string, resp fantasy.ToolResponse) fantasy.ToolResponse {
	limit := maxToolOutputBytes()
	if len(resp.Data) > 0 || resp.Type == "image" || resp.Type == "media" {
		// Both fields are discarded at this boundary. Count both so a custom
		// media tool carrying a caption alongside binary data cannot underreport
		// how many model-visible bytes were suppressed.
		original := len(resp.Data) + len(resp.Content)
		env := toolOutputEnvelope{
			Tool:             boundedToolName(toolName),
			OriginalBytes:    original,
			Truncated:        true,
			Format:           "media",
			BinarySuppressed: true,
			MediaType:        resp.MediaType,
			RecoveryAction:   "Re-run the tool so it saves the media in the conversation workspace and returns a workspace-relative path.",
		}
		resp.Type = "text"
		resp.Content = renderJSONEnvelope(env, "", limit)
		resp.Data = nil
		resp.MediaType = ""
		metrics.RecordToolOutputTruncation(toolName, "media")
		return resp
	}

	content := resp.Content
	encodedBinary := containsEncodedBinary(content)
	if len(content) <= limit && !encodedBinary {
		return resp
	}

	format := "text"
	if json.Valid([]byte(strings.TrimSpace(content))) {
		format = "json"
	}
	artifactPath := ""
	if !encodedBinary {
		var artifactErr error
		artifactPath, artifactErr = tools.StageModelOutputArtifact(ctx, toolName, toolCallID, format, content)
		switch {
		case artifactErr == nil:
			metrics.RecordToolOutputArtifact("success")
		case errors.Is(artifactErr, tools.ErrModelOutputArtifactScope):
			metrics.RecordToolOutputArtifact("unavailable")
		case errors.Is(artifactErr, tools.ErrModelOutputArtifactCapacity), errors.Is(artifactErr, tools.ErrModelOutputArtifactTooLarge):
			metrics.RecordToolOutputArtifact("capacity")
		default:
			metrics.RecordToolOutputArtifact("failure")
			log.Printf("agentcore: failed to stage governed %s output artifact: %v", boundedToolName(toolName), artifactErr)
		}
	}

	recovery := fmt.Sprintf("Re-run %s with narrower filters, pagination, or a workspace-file output.", boundedToolName(toolName))
	if encodedBinary {
		recovery = fmt.Sprintf("Re-run %s so it writes the encoded/binary value directly to a conversation workspace file and returns only the workspace-relative path; binary previews are intentionally unavailable.", boundedToolName(toolName))
	}
	if artifactPath != "" {
		recovery = fmt.Sprintf("Use view_file with path %q and offset/limit chunks no larger than %d bytes to inspect the governed full output.", artifactPath, artifactRecoveryChunkBytes(limit))
	}
	env := toolOutputEnvelope{
		Tool:             boundedToolName(toolName),
		OriginalBytes:    len(content),
		Truncated:        true,
		Format:           format,
		BinarySuppressed: encodedBinary,
		ArtifactPath:     artifactPath,
		RecoveryAction:   recovery,
	}
	if format == "json" {
		resp.Content = renderJSONEnvelope(env, content, limit)
	} else {
		resp.Content = renderTextEnvelope(env, content, limit)
	}
	resp.Type = "text"
	resp.Data = nil
	resp.MediaType = ""
	metrics.RecordToolOutputTruncation(toolName, format)
	log.Printf("agentcore: bounded %s output from %d to %d bytes (limit=%d, format=%s, binary_suppressed=%t)",
		env.Tool, len(content), len(resp.Content), limit, format, encodedBinary)
	return resp
}

func boundedToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "tool"
	}
	if len(name) > 96 {
		name = name[:96]
	}
	return strings.ToValidUTF8(name, "?")
}

func renderJSONEnvelope(env toolOutputEnvelope, content string, limit int) string {
	env.FleetEnvelope = fleetToolOutputEnvelopeV1
	if !env.BinarySuppressed {
		// Begin with an already-bounded preview. Starting with the complete tool
		// result would marshal and repeatedly copy an attacker-sized string before
		// converging on the cap.
		env.Preview = headTailPreview(content, initialPreviewBudget(limit))
	}
	for {
		env.ShownBytes = len(env.Preview)
		encoded, err := json.Marshal(env)
		if err == nil && len(encoded) <= limit {
			return string(encoded)
		}
		if env.Preview == "" {
			// normalizeToolOutputLimit guarantees enough room for this shape. Keep
			// a valid JSON last resort if a future field makes that assumption false.
			return fmt.Sprintf(`{"_fleet_envelope":%q,"truncated":true,"original_bytes":%d,"shown_bytes":0,"format":"%s","recovery_action":"re-run with narrower output"}`, fleetToolOutputEnvelopeV1, env.OriginalBytes, env.Format)
		}
		env.Preview = headTailPreview(env.Preview, len(env.Preview)*3/4)
	}
}

func renderTextEnvelope(env toolOutputEnvelope, content string, limit int) string {
	preview := ""
	if !env.BinarySuppressed {
		preview = headTailPreview(content, initialPreviewBudget(limit))
	}
	for {
		env.ShownBytes = len(preview)
		var b strings.Builder
		fmt.Fprintf(&b, "[tool output truncated]\ntool: %s\noriginal_bytes: %d\nshown_bytes: %d\nformat: %s\n",
			env.Tool, env.OriginalBytes, env.ShownBytes, env.Format)
		b.WriteString("truncated: true\n")
		if env.BinarySuppressed {
			b.WriteString("binary_suppressed: true\n")
		}
		if env.ArtifactPath != "" {
			fmt.Fprintf(&b, "artifact_path: %s\n", env.ArtifactPath)
		}
		fmt.Fprintf(&b, "recovery_action: %s", env.RecoveryAction)
		if preview != "" {
			b.WriteString("\npreview:\n")
			b.WriteString(preview)
		}
		if b.Len() <= limit {
			return b.String()
		}
		if preview == "" {
			return headTailPreview(b.String(), limit)
		}
		preview = headTailPreview(preview, len(preview)*3/4)
	}
}

func initialPreviewBudget(limit int) int {
	limit = normalizeToolOutputLimit(limit)
	// Half leaves deterministic room for metadata and JSON escaping. The small
	// shrink loop remains only for unusually long tool/recovery labels and never
	// starts from untrusted result size.
	return limit / 2
}

func artifactRecoveryChunkBytes(limit int) int {
	limit = normalizeToolOutputLimit(limit)
	if limit <= 2560 {
		return max(256, limit/2)
	}
	return min(32*1024, limit-2048)
}

func headTailPreview(content string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(content) <= limit {
		return content
	}
	const marker = "\n…[omitted]…\n"
	if limit <= len(marker)+8 {
		return ""
	}
	room := limit - len(marker)
	headN := backToRuneBoundary(content, room*2/3)
	tailStart := alignToRuneBoundary(content, len(content)-(room-headN))
	if tailStart <= headN {
		return content[:backToRuneBoundary(content, limit)]
	}
	return content[:headN] + marker + content[tailStart:]
}

// containsEncodedBinary detects raw binary and large standard or URL-safe
// base64/data-URI payloads with O(1) auxiliary space. In particular, it never
// unmarshals an attacker-sized JSON document or decodes a giant base64 string
// merely to decide whether an inline preview is safe.
func containsEncodedBinary(content string) bool {
	if !utf8.ValidString(content) || strings.IndexByte(content, 0) >= 0 {
		return true
	}
	return containsEscapedJSONControl(content) || containsFoldASCII(content, ";base64,") || containsJSONBase64Key(content) || looksLikeBase64(content)
}

func looksLikeBase64(s string) bool {
	const minimumRun = 256
	runStart := 0
	run := 0
	for i := 0; i < len(s); i++ {
		if isBase64Byte(s[i]) {
			if run == 0 {
				runStart = i
			}
			run++
			if run == minimumRun && base64RunHasEncodedShape(s[runStart:i+1]) {
				return true
			}
			continue
		}
		if run > minimumRun && base64RunHasEncodedShape(s[runStart:i]) {
			return true
		}
		run = 0
	}
	return run > minimumRun && base64RunHasEncodedShape(s[runStart:])
}

func base64RunHasEncodedShape(run string) bool {
	var categories uint8
	var seen [4]uint64
	distinct := 0
	for i := 0; i < len(run); i++ {
		b := run[i]
		switch {
		case b >= 'A' && b <= 'Z':
			categories |= 1
		case b >= 'a' && b <= 'z':
			categories |= 2
		case b >= '0' && b <= '9':
			categories |= 4
		default:
			// Padding and standard/URL-safe punctuation are strong encoding
			// signals and cannot occur in a plain alphanumeric identifier.
			return true
		}
		word, bit := b/64, uint64(1)<<(b%64)
		if seen[word]&bit == 0 {
			seen[word] |= bit
			distinct++
		}
	}
	if (categories&(categories-1)) != 0 && distinct >= 12 {
		return true
	}
	// Repeated base64 quartets such as QUJD encode low-entropy bytes but use
	// only uppercase characters. Recognize short quartet-aligned periods while
	// rejecting pathological one-character prose/fixture runs such as "xxxxx".
	for period := 4; period <= 32 && period*2 <= len(run); period += 4 {
		if distinct < 3 {
			break
		}
		periodic := true
		for i := period; i < len(run); i++ {
			if run[i] != run[i%period] {
				periodic = false
				break
			}
		}
		if periodic {
			return true
		}
	}
	return false
}

func containsJSONBase64Key(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		start := i + 1
		j := start
		for j < len(s) && s[j] != '"' && s[j] != '\\' && j-start <= 128 {
			j++
		}
		if j >= len(s) || s[j] != '"' {
			continue
		}
		k := j + 1
		for k < len(s) && (s[k] == ' ' || s[k] == '\t' || s[k] == '\r' || s[k] == '\n') {
			k++
		}
		if k < len(s) && s[k] == ':' && containsFoldASCIIKey(s[start:j], "base64") {
			return true
		}
		i = j
	}
	return false
}

func containsFoldASCIIKey(s, needle string) bool {
	if len(s) < len(needle) {
		return false
	}
	for i := 0; i <= len(s)-len(needle); i++ {
		matched := true
		for j := range needle {
			a := s[i+j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if a != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func isBase64Byte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' ||
		b == '+' || b == '/' || b == '=' || b == '-' || b == '_'
}

func containsFoldASCII(s, needle string) bool {
	if needle == "" || len(s) < len(needle) {
		return false
	}
	for offset := 0; offset <= len(s)-len(needle); {
		// All current callers use a punctuation-leading marker. IndexByte uses
		// the runtime's optimized byte search and avoids an O(n*m) mixed-case scan
		// over ordinary multi-megabyte prose.
		rel := strings.IndexByte(s[offset:], needle[0])
		if rel < 0 {
			return false
		}
		i := offset + rel
		if i > len(s)-len(needle) {
			return false
		}
		matched := true
		for j := range needle {
			a, b := s[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 'a' - 'A'
			}
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
		offset = i + 1
	}
	return false
}

// escControl is ESC (0x1b). A JSON encoder emits it as , and that is the
// first byte of every ANSI colour sequence — so it is the one sub-0x20 control
// that ordinary TEXT output is full of, not a sign of smuggled binary. Treating
// it as binary silently destroyed every Python error on the platform: IPython
// colours its tracebacks, python_bridge.py hands them through verbatim, the JSON
// encoding turned each ESC into , and boundModelVisibleToolResponse
// suppressed the whole result with shown_bytes 0 and no staged artifact — so the
// agent was told only that "binary previews are intentionally unavailable" and
// never saw its own exception. The same trap ate any bash output from a CLI that
// colours (git diff --color, pytest --color=yes, ls --color=always).
const escControl = 0x1b

func containsEscapedJSONControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			continue
		}
		next := s[i+1]
		if next == '\\' {
			i++ // a literal backslash; do not treat the following text as an escape
			continue
		}
		if next == 'b' || next == 'f' {
			return true
		}
		if next != 'u' || i+5 >= len(s) {
			continue
		}
		value, ok := parseHex4(s[i+2 : i+6])
		if ok && value < 0x20 && value != escControl {
			return true
		}
		i += 5
	}
	return false
}

func parseHex4(s string) (int, bool) {
	if len(s) != 4 {
		return 0, false
	}
	value := 0
	for i := range s {
		value <<= 4
		switch b := s[i]; {
		case b >= '0' && b <= '9':
			value += int(b - '0')
		case b >= 'a' && b <= 'f':
			value += int(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value += int(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func backToRuneBoundary(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

func alignToRuneBoundary(s string, i int) int {
	if i < 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
