package httpapi

import (
	"bytes"
	"context"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/contracttest"
)

var updateContract = flag.Bool("update", false, "rewrite testdata/contracts/chat-stream-preamble.sse from the live writer")

// The chat stream contract, wire side (docs/TESTING-STRATEGY.md, "Contracts").
//
// internal/agent TestChatStreamContract records the turn events RunTurn
// emits. What a client receives is those events as THIS package frames them:
// turnBuffer.Emit assigns the ids and encodes the payloads, and Attach writes
// a synthetic fleet.capabilities frame ahead of everything. This test pushes
// every turn recording through the real turnBuffer and Attach and requires the
// wire bytes to be exactly the recorded preamble followed by the recording — so
// the recordings the consumers replay are the stream production sends, and the
// preamble itself is recorded from the real writer rather than written by hand.
//
// Regenerate the preamble after changing the capabilities frame:
//
//	go test -tags fleet_host_executor ./internal/httpapi -run TestChatStreamWireFraming -update
func TestChatStreamWireFraming(t *testing.T) {
	// The advertised heartbeat follows FLEET_SSE_HEARTBEAT_INTERVAL; pin the
	// default so a developer's environment cannot change the recording.
	saved := sseHeartbeatInterval
	sseHeartbeatInterval = 15 * time.Second
	t.Cleanup(func() { sseHeartbeatInterval = saved })

	var preamble []byte
	for _, name := range contracttest.ChatStreamRecordings(t) {
		t.Run(name, func(t *testing.T) {
			recording, frames := contracttest.ReadTurnRecording(t, name)
			buf := newTurnBuffer("conv-contract", "turn-contract")
			for _, f := range frames {
				// The recorded bytes, not a re-encoding of the parsed map: a
				// re-encoding would sort keys and hide a framing difference.
				buf.Emit(f.Event, f.Raw)
			}
			buf.Finish()
			rw := newRecorder()
			if err := buf.Attach(context.Background(), 0, rw, nil); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			wire := []byte(rw.Body())

			// Everything ahead of the first turn event is the preamble.
			first := bytes.Index(wire, []byte("id: 1\n"))
			if first < 0 {
				t.Fatalf("no turn events on the wire:\n%s", wire)
			}
			if preamble == nil {
				preamble = wire[:first]
			} else if !bytes.Equal(preamble, wire[:first]) {
				t.Errorf("preamble differs between turns:\n%s\n---\n%s", preamble, wire[:first])
			}
			if got, want := wire[first:], withoutComments(recording); !bytes.Equal(got, want) {
				t.Errorf("Attach wrote %q differently from its recording.\n--- recorded\n%s\n--- on the wire\n%s", name, want, got)
			}
		})
	}

	path := contracttest.ChatStreamPreamblePath()
	if *updateContract {
		if err := os.WriteFile(path, preamble, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — record it with -update", err)
	}
	if !bytes.Equal(recorded, preamble) {
		t.Errorf("the stream preamble Attach writes no longer matches %s.\n"+
			"Make every consumer handle the change, then regenerate with -update.\n--- recorded\n%s\n--- written now\n%s",
			path, recorded, preamble)
	}
}

// withoutComments drops the recordings' ":" header lines, which document the
// file and are not part of any stream.
func withoutComments(sse []byte) []byte {
	var out []string
	for _, line := range strings.SplitAfter(string(sse), "\n") {
		if !strings.HasPrefix(line, ":") {
			out = append(out, line)
		}
	}
	return bytes.TrimLeft([]byte(strings.Join(out, "")), "\n")
}
