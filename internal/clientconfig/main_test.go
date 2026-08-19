package clientconfig

import (
	"os"
	"testing"
)

// Pin FLEET_DATA_DIR for the whole package so Load/materialize writes
// merged skills under a disposable tree instead of the developer's
// cache or ./data (#1121). Individual tests can t.Setenv to override.
//
// FLEET_ENV_FILE is unset for the whole package: the first Load in the test
// process consumes the once-per-process boot env-file application (#1123),
// and this repo is developed on live deployment hosts — an ambient
// FLEET_ENV_FILE would fold a REAL env file's values into the test process.
// Tests that need one point it at a fixture via t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("FLEET_ENV_FILE")
	dir, err := os.MkdirTemp("", "fleet-skills-test-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("FLEET_DATA_DIR", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
