package httpapi

import (
	"os"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// Most fake turns in these tests are registered and cancelled but never
// sealed; do not let every Stop wait long to confirm them.
func TestMain(m *testing.M) {
	stopConfirmWait = 50 * time.Millisecond
	os.Exit(testdb.Run(m)) // testdb background mode: clones made ahead, dropped off the critical path
}
