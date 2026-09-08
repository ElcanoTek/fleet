package tools

import (
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// TestBashToolBackgroundChildDoesNotHang proves cmd.Run returns once WaitDelay
// fires even though an orphaned child still holds the output pipes. That
// property holds at any delay, so the test binary uses a short one instead of
// sitting through the production 10s.
func init() {
	sandbox.BashWaitDelay = 500 * time.Millisecond
}
