package handlers

import "time"

// The A2A lifecycle tests drive a task through its states and wait for the
// stream / unary wait / push delivery to observe each one. The row is polled,
// so at the production 1s interval each transition costs a second of pure
// waiting; the assertions are about the sequence observed, not the cadence.
func init() {
	a2aStreamPollInterval = 50 * time.Millisecond
}
