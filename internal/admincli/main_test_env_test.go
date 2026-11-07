package admincli

import (
	"os"
	"strings"
	"testing"
)

// TestMain pins the admin CLI suites to the test databases' layout.
//
// LEGACY-DB-LAYOUT: remove after consolidation (with M2's test databases).
// The Postgres-backed suites follow the CI convention: FLEET_TEST_DATABASE_URL
// is the chat test database and DATABASE_URL the sched test database, each
// store in its own database's public schema — the legacy two-database layout.
// Since #2049, DATABASE_URL alone means ONE database with a chat and a sched
// schema, so a verb under test that resolves the environment would look in
// schema sched of the sched test database and find nothing. Naming the two
// databases per store keeps the verbs on the layout the fixtures use. A test
// that sets its own DSN keys (t.Setenv) still overrides these.
func TestMain(m *testing.M) {
	chat, sched := os.Getenv("FLEET_TEST_DATABASE_URL"), os.Getenv("DATABASE_URL")
	if chat != "" && sched != "" && os.Getenv("FLEET_CHAT_DATABASE_URL") == "" && os.Getenv("FLEET_SCHED_DATABASE_URL") == "" {
		_ = os.Setenv("FLEET_CHAT_DATABASE_URL", chat)
		_ = os.Setenv("FLEET_SCHED_DATABASE_URL", sched)
		testInjectedEnv = map[string]bool{"FLEET_CHAT_DATABASE_URL": true, "FLEET_SCHED_DATABASE_URL": true}
	}
	os.Exit(m.Run())
}

// testInjectedEnv names the keys TestMain added, so the operator-script tests
// (runScript) hand the scripts the environment the test run was given, not
// the suite's pinning.
var testInjectedEnv map[string]bool

// scriptEnviron is os.Environ without testInjectedEnv's keys.
func scriptEnviron() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !testInjectedEnv[k] {
			out = append(out, kv)
		}
	}
	return out
}
