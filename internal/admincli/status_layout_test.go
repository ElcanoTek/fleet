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

	run := func(chat, sched string) string {
		var out bytes.Buffer
		checkLayout(&report{out: &out, summary: &out}, chat, sched)
		return out.String()
	}

	t.Setenv("DATABASE_URL", "postgres://u@h/fleet")
	if got := run("", ""); !strings.Contains(got, "one database, schemas chat + sched") {
		t.Errorf("no overrides reported %q", got)
	}
	// Overrides aimed at another one-database install get the stores' schemas.
	if got := run("postgres://u@dr/fleet", "postgres://u@dr/fleet"); !strings.Contains(got, "one database, schemas chat + sched") {
		t.Errorf("overrides to another database reported %q", got)
	}
	// #2055 review: schemas chat and sched in two DIFFERENT databases are not
	// "one database".
	if got := run("postgres://u@a/fleet", "postgres://u@b/fleet"); strings.Contains(got, "one database") || !strings.Contains(got, "two different databases") {
		t.Errorf("schemas in two databases reported %q", got)
	}
	// Overrides that name their own schema are reported as they are.
	pub := "postgres://u@h/fleet?options=-csearch_path%3Dpublic"
	if got := run(pub, pub); !strings.Contains(got, `chat in schema "public", sched in schema "public"`) {
		t.Errorf("explicit public overrides reported %q", got)
	}

	// A retired per-store key: the layout cannot be told.
	t.Setenv("FLEET_CHAT_DATABASE_URL", "postgres://c@h/chat")
	if got := run("", ""); !strings.Contains(got, "DSN unresolved") {
		t.Errorf("a retired per-store key reported %q", got)
	}
}
