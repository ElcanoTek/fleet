package fakellm

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// nonce returns a short random hex string for synthesizing chunk/tool-call IDs.
func nonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail; fall back to a time-based value.
		return hex.EncodeToString([]byte(time.Now().Format("150405.000")))
	}
	return hex.EncodeToString(b[:])
}

// ── Step constructors (ergonomic helpers for building Scenarios) ──

// TextStep returns a final-text step.
func TextStep(text string) Step { return Step{Kind: StepText, Text: text} }

// BashStep returns a step that calls the bash tool with the given command.
func BashStep(id, command string) Step {
	return Step{Kind: StepToolCalls, ToolCalls: []ToolCall{{
		ID:        id,
		Name:      "bash",
		Arguments: jsonObj("command", command),
	}}}
}

// PythonStep returns a step that calls the run_python tool with the given code.
func PythonStep(id, code string) Step {
	return Step{Kind: StepToolCalls, ToolCalls: []ToolCall{{
		ID:        id,
		Name:      "run_python",
		Arguments: jsonObj("code", code),
	}}}
}

// ToolStep returns a step emitting an arbitrary set of tool calls.
func ToolStep(calls ...ToolCall) Step {
	return Step{Kind: StepToolCalls, ToolCalls: calls}
}

// StatusStep returns a step that replies with an HTTP error status.
func StatusStep(code int) Step { return Step{Kind: StepStatus, Status: code} }

// jsonObj builds a one-key JSON object string {"k":"v"} with v properly escaped.
func jsonObj(k, v string) string {
	// Hand-roll a tiny encoder to avoid pulling marshalling into a hot path and
	// to keep the produced argument string compact and predictable.
	return `{"` + k + `":` + quote(v) + `}`
}

// quote JSON-escapes a string value (including surrounding quotes).
func quote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			out = append(out, string(r)...)
		}
	}
	out = append(out, '"')
	return string(out)
}
