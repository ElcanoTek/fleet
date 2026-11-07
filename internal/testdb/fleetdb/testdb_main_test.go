package fleetdb

import (
	"os"
	"testing"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestMain runs the suite in testdb's background mode: per-test databases are
// cloned ahead of the tests that need them and dropped off their critical
// path, and none is left behind when the run ends (testdb.Run).
func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
