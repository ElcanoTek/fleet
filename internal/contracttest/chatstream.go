// Package contracttest reads the recorded cross-process contracts in
// testdata/contracts (docs/TESTING-STRATEGY.md, "Contracts") for the Go suites
// on both sides of them. It is imported only by tests.
//
// The chat stream contract lives in testdata/contracts/chat-stream: one .sse
// file per scripted turn, recorded from the real producer (internal/agent
// TestChatStreamContract) in the exact wire framing the chat server writes.
// The producer's test regenerates them; every consumer replays them.
package contracttest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ChatStreamDir is the absolute path of testdata/contracts/chat-stream.
func ChatStreamDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "contracts", "chat-stream")
}

// Frame is one event of a recorded stream.
type Frame struct {
	ID    int
	Event string
	Data  map[string]any
}

// ChatStreamRecordings lists the recorded turns (file names), sorted. It fails
// the test when there are none, so a replay can never pass vacuously.
func ChatStreamRecordings(t testing.TB) []string {
	t.Helper()
	entries, err := os.ReadDir(ChatStreamDir())
	if err != nil {
		t.Fatalf("read chat-stream contract dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sse") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no recordings in %s", ChatStreamDir())
	}
	return names
}

// ReadChatStream returns a recording's raw bytes (to serve verbatim, as the
// chat server would) and its frames (an independent parse, for the oracle).
func ReadChatStream(t testing.TB, name string) ([]byte, []Frame) {
	t.Helper()
	// os.Root confines the read to the contracts directory whatever name says.
	root, err := os.OpenRoot(ChatStreamDir())
	if err != nil {
		t.Fatalf("open chat-stream contract dir: %v", err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(name)
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	var frames []Frame
	for _, block := range bytes.Split(raw, []byte("\n\n")) {
		var f Frame
		var data string
		for _, line := range strings.Split(string(block), "\n") {
			switch {
			case line == "" || strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id: "):
				f.ID, _ = strconv.Atoi(strings.TrimPrefix(line, "id: "))
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if f.Event == "" {
			continue
		}
		if err := json.Unmarshal([]byte(data), &f.Data); err != nil {
			t.Fatalf("%s: frame %d (%s): %v", name, f.ID, f.Event, err)
		}
		frames = append(frames, f)
	}
	return raw, frames
}

// Expected is what any consumer must end up showing for a recorded turn,
// derived from the recording alone.
type Expected struct {
	// Text is the answer as finally shown: the last text.replace, else the
	// concatenated text.delta frames.
	Text string
	// Tools are the tool calls in order, with how each one ended.
	Tools []ExpectedTool
	// Terminal is the turn's final turn.* frame (not turn.started).
	Terminal Frame
}

// ExpectedTool is one tool call and its outcome.
type ExpectedTool struct {
	ID, Name string
	// Result is false when the call never got a tool.result.
	Result bool
	IsErr  bool
}

// Expect derives the oracle for a recording.
func Expect(frames []Frame) Expected {
	var e Expected
	var deltas strings.Builder
	replaced, haveReplace := "", false
	results := map[string]Frame{}
	for _, f := range frames {
		switch f.Event {
		case "text.delta":
			s, _ := f.Data["text"].(string)
			deltas.WriteString(s)
		case "text.replace":
			replaced, _ = f.Data["text"].(string)
			haveReplace = true
		case "tool.result":
			id, _ := f.Data["id"].(string)
			results[id] = f
		}
		if strings.HasPrefix(f.Event, "turn.") && f.Event != "turn.started" {
			e.Terminal = f
		}
	}
	e.Text = deltas.String()
	if haveReplace {
		e.Text = replaced
	}
	for _, f := range frames {
		if f.Event != "tool.call" {
			continue
		}
		id, _ := f.Data["id"].(string)
		name, _ := f.Data["name"].(string)
		tool := ExpectedTool{ID: id, Name: name}
		if r, ok := results[id]; ok {
			tool.Result = true
			tool.IsErr, _ = r.Data["is_err"].(bool)
		}
		e.Tools = append(e.Tools, tool)
	}
	return e
}
