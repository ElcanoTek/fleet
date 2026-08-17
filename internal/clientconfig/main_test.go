package clientconfig

import (
	"os"
	"testing"
)

// Pin FLEET_DATA_DIR for the whole package so Load/materialize writes
// merged skills under a disposable tree instead of the developer's
// cache or ./data (#1121). Individual tests can t.Setenv to override.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fleet-skills-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("FLEET_DATA_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
