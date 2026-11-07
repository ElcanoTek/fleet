package logging

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// output is the process log destination Configure set up: stderr, plus the
// rotating file when the file sink is on. Before Configure it is stderr.
var (
	outputMu sync.RWMutex
	output   io.Writer = os.Stderr
)

func setOutput(w io.Writer) {
	outputMu.Lock()
	output = w
	outputMu.Unlock()
}

func currentOutput() io.Writer {
	outputMu.RLock()
	defer outputMu.RUnlock()
	return output
}

// ChildStderr returns a writer for a child process's stderr that forwards
// each complete line to this process's log destination (stderr, plus
// FLEET_LOG_FILE when it is on). A child that logs through Configure with no
// file of its own (the fleet mcp-broker) so lands in the same sink, under the
// one rotator the parent owns — two processes rotating one file would rename
// it under each other. Lines are forwarded whole, so a child line never
// interleaves with a parent line mid-way.
func ChildStderr() io.Writer {
	return &lineForwarder{}
}

// lineForwarder buffers a partial line until its newline arrives.
type lineForwarder struct {
	mu      sync.Mutex
	pending []byte
}

func (f *lineForwarder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = append(f.pending, p...)
	for {
		i := bytes.IndexByte(f.pending, '\n')
		if i < 0 {
			break
		}
		line := f.pending[:i+1]
		if _, err := currentOutput().Write(line); err != nil {
			f.pending = f.pending[i+1:]
			return len(p), err
		}
		f.pending = f.pending[i+1:]
	}
	if len(f.pending) == 0 {
		f.pending = nil
	}
	return len(p), nil
}
