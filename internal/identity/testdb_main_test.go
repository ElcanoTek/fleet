package identity_test

import (
	"os"
	"testing"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// TestMain runs the suite in testdb's background mode (testdb.Run).
func TestMain(m *testing.M) { os.Exit(testdb.Run(m)) }
