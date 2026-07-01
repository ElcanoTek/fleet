package main

import (
	"bufio"
	"io"
	"sort"
	"strings"
	"time"
)

// terminalSSEEvents are the chat SSE event names that end a turn (#296). A load
// client stops reading a turn's stream once it sees one of these.
var terminalSSEEvents = map[string]bool{
	"turn.completed": true,
	"turn.cancelled": true,
	"turn.error":     true,
}

// readTurnToTerminal consumes an SSE stream line-by-line and returns the name of
// the terminal event (turn.completed/cancelled/error) once it arrives. The chat
// server frames events as "event: <name>\n" lines; we only need the event name
// to detect completion, so data payloads are ignored. Returns io.EOF (or another
// read error) if the stream closes before any terminal event — which the caller
// treats as a failed turn.
func readTurnToTerminal(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	// SSE data frames can be large (a full assistant reply); raise the line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		name, ok := strings.CutPrefix(line, "event: ")
		if !ok {
			name, ok = strings.CutPrefix(line, "event:")
		}
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if terminalSSEEvents[name] {
			return name, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

// percentile returns the p-th percentile (0..100) of samples using the
// nearest-rank method. samples need NOT be pre-sorted (a copy is sorted).
// Returns 0 for an empty slice.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	s := make([]time.Duration, len(samples))
	copy(s, samples)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if p <= 0 {
		return s[0]
	}
	if p >= 100 {
		return s[len(s)-1]
	}
	// Nearest-rank: rank = ceil(p/100 * N), 1-indexed.
	rank := int((p/100.0)*float64(len(s)) + 0.9999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

// mean returns the arithmetic mean of samples (0 for empty).
func mean(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range samples {
		total += d
	}
	return total / time.Duration(len(samples))
}
