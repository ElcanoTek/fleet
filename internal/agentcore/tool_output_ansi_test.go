package agentcore

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestColoredTextIsNotEncodedBinary pins the fix for a platform-wide blind spot:
// every Python error was invisible to the agent. IPython colours its tracebacks,
// python_bridge.py passed content["traceback"] through verbatim, JSON encoding
// turned each ESC into a \u001b escape, and containsEscapedJSONControl treated
// any escaped control below 0x20 as smuggled binary — so
// boundModelVisibleToolResponse suppressed the whole result at shown_bytes 0,
// staged NO artifact (the staging branch is skipped for binary), and told the
// agent only that "binary previews are intentionally unavailable". A real session
// took two blind KeyErrors and a failed view_file before abandoning run_python
// for bash, where errors arrive as plain text. The same trap ate any bash output
// from a CLI that colours.
func TestColoredTextIsNotEncodedBinary(t *testing.T) {
	const esc = "\x1b"

	tracebackJSON, err := json.Marshal(map[string]string{
		"status": "error",
		"output": "Error:\n" + esc + "[31m---------------------------" + esc + "[39m\n" +
			esc + "[31mKeyError" + esc + "[39m Traceback (most recent call last)\n" +
			esc + "[32m      4" + esc + "[39m html = obj['page']['published']['html']\n" +
			esc + "[31mKeyError" + esc + "[39m: 'published'",
	})
	if err != nil {
		t.Fatalf("marshal traceback: %v", err)
	}
	if !strings.Contains(string(tracebackJSON), `\u001b`) {
		t.Fatalf("fixture must carry a JSON-escaped ESC: %s", tracebackJSON)
	}
	if containsEncodedBinary(string(tracebackJSON)) {
		t.Errorf("a colored python traceback is text, not binary:\n%s", tracebackJSON)
	}

	bashJSON, err := json.Marshal(map[string]any{
		"exit_code": 1,
		"stdout": esc + "[1mdiff --git a/x b/x" + esc + "[m\n" +
			esc + "[32m+added" + esc + "[m\n" + esc + "[31m-removed" + esc + "[m\n",
	})
	if err != nil {
		t.Fatalf("marshal bash output: %v", err)
	}
	if containsEncodedBinary(string(bashJSON)) {
		t.Errorf("colored CLI output through bash is text, not binary:\n%s", bashJSON)
	}

	// The detector must keep doing the job it exists for. Every one of these is
	// a genuine binary/encoded signal and must still suppress.
	for name, content := range map[string]string{
		"escaped NUL":       `{"stdout":"\u0000MZ"}`,
		"escaped backspace": `{"stdout":"a\bb"}`,
		"literal NUL":       "{\"stdout\":\"PNG\x00\x01\x02\"}",
		"data URI":          `{"src":"data:image/png;base64,iVBORw0KGgo="}`,
		"base64 key":        `{"image_base64":"iVBORw0KGgo="}`,
	} {
		if !containsEncodedBinary(content) {
			t.Errorf("%s must still be treated as binary: %s", name, content)
		}
	}
}
