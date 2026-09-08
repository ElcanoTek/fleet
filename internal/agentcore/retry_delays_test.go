package agentcore

import "time"

// The resilience tests drive real blips and real exhaustion through the retry
// paths; what they assert is the decision (retry in place, swap, give up, which
// error class), never the length of the pause. At the production delays the
// pauses alone were ~27s of this package's run, so the whole test binary uses
// short ones.
func init() {
	streamBlipRetryDelay = 10 * time.Millisecond
	terminalRetryBaseDelay = 5 * time.Millisecond
}
