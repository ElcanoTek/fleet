package admincli

import (
	"encoding/json"
	"errors"
	"testing"

	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestMigrateStatusEnvelope — `fleet migrate status --json` must ALWAYS emit
// the {"chat":…,"sched":…} envelope: a DB that could not be read carries an
// "error" member instead of being dropped, and both failing still yields valid
// JSON. It used to print nothing when both failed and to omit the failing DB
// when one did, so a CI consumer could not tell "sched unreachable" from
// "sched fine, key missing".
func TestMigrateStatusEnvelope(t *testing.T) {
	decode := func(t *testing.T, env migrateStatusJSON) map[string]map[string]any {
		t.Helper()
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got map[string]map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("envelope is not an object of objects: %v\n%s", err, b)
		}
		return got
	}

	// One side failed, the other reported.
	v := 12
	sched := &scheddb.MigrationReport{DB: "sched", Runner: "golang-migrate", MigrationTable: "schema_migrations", CurrentVersion: &v}
	got := decode(t, migrateStatusEnvelope(nil, errors.New("chat DSN unset"), sched, nil))
	if got["chat"]["error"] != "chat DSN unset" {
		t.Errorf("chat.error = %v, want the DSN error", got["chat"]["error"])
	}
	if _, has := got["chat"]["runner"]; has {
		t.Errorf("a failed DB must not carry report fields: %v", got["chat"])
	}
	if got["sched"]["runner"] != "golang-migrate" || got["sched"]["current_version"] != float64(12) {
		t.Errorf("sched report not flattened into its member: %v", got["sched"])
	}
	if _, has := got["sched"]["error"]; has {
		t.Errorf("a healthy DB must not carry an error member: %v", got["sched"])
	}

	// Both failed: still a complete envelope.
	got = decode(t, migrateStatusEnvelope(nil, errors.New("c"), nil, errors.New("s")))
	if got["chat"]["error"] != "c" || got["sched"]["error"] != "s" {
		t.Errorf("both-failed envelope = %v", got)
	}

	// Both reported: no error members anywhere.
	chat := &store.MigrationReport{DB: "chat", Runner: "hand-rolled", MigrationTable: "schema_migrations"}
	got = decode(t, migrateStatusEnvelope(chat, nil, sched, nil))
	for _, side := range []string{"chat", "sched"} {
		if _, has := got[side]["error"]; has {
			t.Errorf("%s should have no error member: %v", side, got[side])
		}
	}
	if got["chat"]["runner"] != "hand-rolled" {
		t.Errorf("chat report missing: %v", got["chat"])
	}
}
