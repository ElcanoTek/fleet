package admincli

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"testing"

	"github.com/ElcanoTek/fleet/internal/migrations"
)

// TestMigrateStatusEnvelope — `fleet migrate status --json` emits the one
// sequence's report at the top level and, for one release, the pre-M6
// per-store reports under "chat" and "sched", derived from it (#2049 M6). A
// database that could not be read still yields the complete envelope, every
// member carrying "error", so a CI consumer sees why.
func TestMigrateStatusEnvelope(t *testing.T) {
	t.Parallel()

	decode := func(t *testing.T, env migrateStatusJSON) map[string]any {
		t.Helper()
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("envelope is not an object: %v\n%s", err, b)
		}
		return got
	}

	cur := 177
	at := int64(1700000000)
	rep := &migrations.Report{DB: "fleet", Runner: "fleet", MigrationTable: migrations.Table, Layout: "unified", Adopted: true,
		CurrentVersion: &cur,
		Applied: []migrations.Info{
			{Version: 1, Name: "chat/001_initial", AppliedAt: &at},
			{Version: 101, Name: "sched/001_initial_schema", AppliedAt: &at},
			{Version: 177, Name: "sched/077_move_chat_schedules", AppliedAt: &at},
		},
	}
	got := decode(t, migrateStatusEnvelope(rep, nil))
	if got["layout"] != "unified" || got["migration_table"] != "core.schema_versions" || got["current_version"] != float64(177) || got["adopted"] != true {
		t.Errorf("top level = %v", got)
	}
	if _, has := got["error"]; has {
		t.Errorf("a readable database must not carry an error: %v", got)
	}
	chat, _ := got["chat"].(map[string]any)
	if chat["runner"] != "hand-rolled" || len(chat["applied"].([]any)) != 1 {
		t.Errorf("chat view = %v", chat)
	}
	sched, _ := got["sched"].(map[string]any)
	if sched["runner"] != "golang-migrate" || sched["current_version"] != float64(77) || len(sched["applied"].([]any)) != 2 {
		t.Errorf("sched view = %v", sched)
	}
	if first := sched["applied"].([]any)[0].(map[string]any); first["version"] != float64(1) || first["name"] != "001_initial_schema" {
		t.Errorf("sched view keeps the legacy numbering and names: %v", first)
	}

	got = decode(t, migrateStatusEnvelope(nil, errors.New("DATABASE_URL unset")))
	if got["error"] != "DATABASE_URL unset" {
		t.Errorf("error = %v", got["error"])
	}
	for _, side := range []string{"chat", "sched"} {
		m, _ := got[side].(map[string]any)
		if m["error"] != "DATABASE_URL unset" || len(m) != 1 {
			t.Errorf("%s = %v, want only the error", side, m)
		}
	}
}

func TestParseForceArgs(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want int
		ok   bool
	}{
		{[]string{"173"}, 173, true},
		{[]string{"0"}, 0, true},
		{[]string{"173", "--no-confirm"}, 173, true},
		{[]string{"--database-url", "postgres://x/5", "173"}, 173, true},
		{[]string{"--database-url=postgres://x", "--no-confirm", "0"}, 0, true},
		{nil, 0, false},
		{[]string{"73", "74"}, 0, false},
		{[]string{"seventy"}, 0, false},
		{[]string{"-1"}, 0, false},
		{[]string{"73", "--bogus"}, 0, false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.String("database-url", "", "")
		fs.Bool("no-confirm", false, "")
		got, ok := parseForceArgs(fs, c.argv)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseForceArgs(%q) = %d, %v; want %d, %v", c.argv, got, ok, c.want, c.ok)
		}
	}
}
