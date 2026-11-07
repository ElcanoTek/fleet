package admincli

import (
	"bytes"
	"strings"
	"testing"
)

// TestStatusLayoutFollowsOverrides (#2055 review): `fleet status` with
// --*-database-url reports the layout of the DSNs it checked, not the
// environment's.
func TestStatusLayoutFollowsOverrides(t *testing.T) {
	for _, k := range dsnEnvKeys {
		t.Setenv(k, "")
	}
	t.Setenv("FLEET_ENV_FILE", t.TempDir()+"/none.env")
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)
	orig := probePublicStore
	t.Cleanup(func() { probePublicStore = orig })

	run := func(chat, sched string) string {
		var out bytes.Buffer
		checkLayout(&report{out: &out, summary: &out}, chat, sched)
		return out.String()
	}

	// A legacy deployment, overrides aimed at a one-database install.
	t.Setenv("FLEET_CHAT_DATABASE_URL", "postgres://c@h/chat")
	t.Setenv("FLEET_SCHED_DATABASE_URL", "postgres://s@h/sched")
	probePublicStore = func(string) (bool, error) { return false, nil }
	if got := run("postgres://u@dr/fleet", "postgres://u@dr/fleet"); !strings.Contains(got, "one database, schemas chat + sched") {
		t.Errorf("one-database overrides on a legacy box reported %q", got)
	}
	if got := run("", ""); !strings.Contains(got, "legacy") {
		t.Errorf("no overrides on a legacy box reported %q", got)
	}

	// A one-database deployment, overrides aimed at a legacy pair.
	t.Setenv("FLEET_CHAT_DATABASE_URL", "")
	t.Setenv("FLEET_SCHED_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "postgres://u@h/fleet")
	probePublicStore = func(string) (bool, error) { return true, nil }
	if got := run("postgres://c@old/chat", "postgres://s@old/sched"); !strings.Contains(got, "legacy") {
		t.Errorf("legacy overrides on a one-database box reported %q", got)
	}
	if got := run("", ""); !strings.Contains(got, "one database") {
		t.Errorf("no overrides on a one-database box reported %q", got)
	}
}
