package main

import (
	"os"
	"testing"
)

// FLEET_ENV_FILE is unset for the whole package: the first clientconfig.Load
// in the test process consumes the once-per-process boot env-file application
// (#1123), and this repo is developed on live deployment hosts — an ambient
// FLEET_ENV_FILE would fold a REAL env file's values into the test process.
// Tests that need one point it at a fixture via t.Setenv.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("FLEET_ENV_FILE")
	os.Exit(m.Run())
}
