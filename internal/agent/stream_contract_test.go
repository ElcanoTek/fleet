package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/contracttest"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// The chat stream contract (docs/TESTING-STRATEGY.md, "Contracts").
//
// The chat event stream has one producer — this package's RunTurn, framed
// verbatim onto the wire by httpapi's turnBuffer and piped untouched through
// web/src/app/api/chat — and several consumers: the web chat hook
// (useTurnStream), the terminal client (internal/chattui) and the ACP adapter
// (internal/acp). Each side used to be tested against its own hand-written idea
// of the other. This test records what the producer REALLY emits for a fixed set
// of scripted turns into testdata/contracts/chat-stream/*.sse, in the exact
// `id/event/data` wire framing, and every consumer's suite replays those files.
// A change on either side then breaks a fast unit test instead of production.
//
// The files are generated, never hand-edited:
//
//	go test -tags fleet_host_executor ./internal/agent -run TestChatStreamContract -update
//
// Without -update the test fails when the recorded stream no longer matches
// what RunTurn emits, and the diff in the failure is the protocol change to
// review (and to make every consumer handle) before regenerating.
var updateContract = flag.Bool("update", false, "rewrite testdata/contracts/chat-stream from the live producer")

// contractScenario is one scripted turn. The script lives here, next to the
// recorder, so the recorded stream and the turn that produced it are reviewed
// together.
type contractScenario struct {
	name   string
	about  string
	steps  []fakellm.Step
	cancel bool // cancel the turn once the model has stalled
}

func contractScenarios() []contractScenario {
	return []contractScenario{
		{
			name:  "text-answer",
			about: "a plain streamed answer, no tools",
			steps: []fakellm.Step{
				fakellm.TextStep("Here is a streamed answer from the contract fixture."),
			},
		},
		{
			name:  "tool-loop",
			about: "a command that succeeds, one that exits non-zero, and a tool error, then the final answer",
			steps: []fakellm.Step{
				fakellm.BashStep("call_contract_ok", "echo CONTRACT_TOOL_OK"),
				// A non-zero exit is a RESULT (is_err:false, exit_code in the
				// envelope); a tool that cannot run at all is an ERROR (is_err:true).
				fakellm.BashStep("call_contract_exit", "ls /contract-path-that-does-not-exist"),
				fakellm.ToolStep(fakellm.ToolCall{ID: "call_contract_err", Name: "view_file",
					Arguments: `{"path":"contract-file-that-does-not-exist.txt"}`}),
				fakellm.TextStep("All three tools reported back."),
			},
		},
		{
			name:  "model-required",
			about: "the provider rejects the request (non-retryable 400)",
			steps: []fakellm.Step{{
				Kind: fakellm.StepStatus, Status: 400,
				StatusBody: `{"error":{"code":400,"message":"contract fixture: bad request"}}`,
			}},
		},
		{
			name:   "cancelled",
			about:  "the user stops the turn while the model is stalled",
			steps:  []fakellm.Step{{Kind: fakellm.StepText, Text: "never sent", Delay: time.Minute}},
			cancel: true,
		},
	}
}

func TestChatStreamContract(t *testing.T) {
	dir := contractDir(t)
	if *updateContract {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]bool{}
	for _, sc := range contractScenarios() {
		want[sc.name+".sse"] = true
		t.Run(sc.name, func(t *testing.T) {
			got := recordContractTurn(t, sc)
			path := filepath.Join(dir, sc.name+".sse")
			if *updateContract {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			recorded, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v — record it with: go test -tags fleet_host_executor ./internal/agent -run TestChatStreamContract -update", err)
			}
			if !bytes.Equal(recorded, got) {
				t.Errorf("the chat stream RunTurn emits for %q no longer matches %s.\n"+
					"This is a protocol change: make every consumer (web useTurnStream, internal/chattui, internal/acp) handle it, then regenerate with -update.\n"+
					"--- recorded\n%s\n--- emitted now\n%s", sc.name, path, recorded, got)
			}
		})
	}
	// A recording whose scenario was removed would keep being replayed by the
	// consumers while nothing produces it any more.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sse") && !want[e.Name()] {
			t.Errorf("%s has no scenario in contractScenarios(); delete it or add the scenario", e.Name())
		}
	}
}

// TestChatStreamContractIsDeterministic guards the recorder itself: a field
// that varies between runs (a duration, a host path) must be normalized, or
// the contract would fail at random instead of on a protocol change.
func TestChatStreamContractIsDeterministic(t *testing.T) {
	for _, sc := range contractScenarios() {
		if a, b := recordContractTurn(t, sc), recordContractTurn(t, sc); !bytes.Equal(a, b) {
			t.Errorf("%s: two recordings differ — normalize the varying field:\n%s\n---\n%s", sc.name, a, b)
		}
	}
}

func contractDir(t *testing.T) string {
	t.Helper()
	return contracttest.ChatStreamDir()
}

// recordContractTurn runs one scripted turn through the real Manager.RunTurn
// and returns its event stream in wire framing, normalized.
func recordContractTurn(t *testing.T, sc contractScenario) []byte {
	t.Helper()
	fake := fakellm.New()
	fake.SetDefault(fakellm.Scenario{Steps: sc.steps})
	mgr := newFakeLLMManager(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sink := &recordingSink{}
	if sc.cancel {
		go func() {
			// Cancel once the turn is under way (turn.started is out and the
			// provider call is stalled), the way a Stop click arrives.
			for !sink.has("turn.started") {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
	}
	_, _ = mgr.RunTurn(ctx, TurnInput{UserMessage: "contract: " + sc.about, Model: "anthropic/claude-opus-4.8"}, sink)

	var out bytes.Buffer
	fmt.Fprintf(&out, ": chat-stream contract %q — %s\n", sc.name, sc.about)
	fmt.Fprintf(&out, ": generated by internal/agent TestChatStreamContract; do not edit\n\n")
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for i, e := range sink.events {
		data, err := json.Marshal(e.payload) // turnBuffer.Emit's encoding
		if err != nil {
			t.Fatalf("marshal %s: %v", e.name, err)
		}
		fmt.Fprintf(&out, "id: %d\nevent: %s\ndata: %s\n\n", i+1, e.name, normalizeContractJSON(data))
	}
	return out.Bytes()
}

// Fields whose values legitimately vary between runs: their presence and type
// are part of the contract, their values are not. The substitution runs on the
// raw bytes, so the recording stays byte-for-byte what the wire carries
// everywhere else — including inside a tool result's JSON envelope, which is
// itself a JSON string (hence the optional backslash before each quote).
var contractVolatile = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(\\?"(?:duration_ms|execution_time_ms)\\?":)\d+`), "${1}0"},
	{regexp.MustCompile(`(\\?"working_directory\\?":\\?")[^"\\]*`), "${1}<workspace>"},
}

func normalizeContractJSON(data []byte) []byte {
	for _, v := range contractVolatile {
		data = v.re.ReplaceAll(data, []byte(v.with))
	}
	return data
}
