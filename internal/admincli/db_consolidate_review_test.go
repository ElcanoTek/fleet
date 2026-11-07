// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestConsolidateTargetsTheDSNsInstance (#2055 review): the superuser runner
// is aimed at the instance the legacy DSNs name — their socket directory
// when they give one, their port always — and preflight refuses when the
// instance that answers is on another port.
func TestConsolidateTargetsTheDSNsInstance(t *testing.T) {
	clearDSNEnv(t)
	for _, tc := range []struct {
		name, chat, sched string
		wantConn          []string
	}{
		{"loopback TCP", "postgres://c:x@127.0.0.1:5433/chat", "postgres://s:x@localhost:5433/sched", []string{"-p", "5433"}},
		{"socket directory", "postgres://c@/chat?host=/run/pg&port=5444", "postgres://s@/sched?host=/run/pg&port=5444", []string{"-h", "/run/pg", "-p", "5444"}},
		{"default port", "postgres://c@127.0.0.1/chat", "postgres://s@127.0.0.1/sched", []string{"-p", "5432"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), "fleet.env")
			if err := os.WriteFile(env, []byte("FLEET_CHAT_DATABASE_URL="+tc.chat+"\nFLEET_SCHED_DATABASE_URL="+tc.sched+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			p, _, err := buildPlan(consolidateOptions{envFile: env, targetDB: "fleet"}, testDeps(healthyFake(), &bytes.Buffer{}))
			if err != nil {
				t.Fatal(err)
			}
			e, ok := superuserPG(p).(*execPG)
			if !ok || !reflect.DeepEqual(e.connArgs, tc.wantConn) || e.prefix[0] != "runuser" {
				t.Errorf("superuserPG = %+v, want runuser with %v", e, tc.wantConn)
			}
		})
	}

	env := writeLegacyEnv(t, "") // the DSNs name port 5432
	f := healthyFake()
	f.prepend(fakeRule{frag: "current_setting('server_version_num')", rows: [][]string{{"160004", "5433"}}})
	var out bytes.Buffer
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 1 {
		t.Fatalf("exit %d, want the refusal:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "reached a server on port 5433") || len(f.execs) != 0 {
		t.Errorf("a different instance was not refused:\n%s", out.String())
	}
}

// countingPG answers the connection check busy for the first busy calls.
type countingPG struct {
	*fakePG
	busy  int32
	calls atomic.Int32
}

func (c *countingPG) Query(ctx context.Context, db, q string) ([][]string, error) {
	if strings.Contains(q, "pg_stat_activity") {
		if !strings.Contains(q, "backend_type = 'client backend'") {
			return [][]string{{"chat", "", "", "1"}}, nil // a worker the check must not count
		}
		if c.calls.Add(1) <= c.busy {
			return [][]string{{"chat", "chat", "fleet", "1"}}, nil
		}
		return nil, nil
	}
	return c.fakePG.Query(ctx, db, q)
}

// TestConsolidateConnectionCheck (#2055 review, CI -race): only client
// backends count, and a client that is still disconnecting is waited for
// (connWait) before the check refuses.
func TestConsolidateConnectionCheck(t *testing.T) {
	clearDSNEnv(t)
	env := writeLegacyEnv(t, "")
	pg := &countingPG{fakePG: healthyFake(), busy: 2}
	var out bytes.Buffer
	d := testDeps(pg.fakePG, &out)
	d.pg = pg
	d.connWait = 5 * time.Second
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet"}, d); code != 0 {
		t.Fatalf("a briefly lingering client was refused (exit %d):\n%s", code, out.String())
	}
	if pg.calls.Load() < 3 {
		t.Errorf("the check did not re-check (calls=%d)", pg.calls.Load())
	}

	pg = &countingPG{fakePG: healthyFake(), busy: 1 << 30}
	out.Reset()
	d = testDeps(pg.fakePG, &out)
	d.pg = pg
	d.connWait = 600 * time.Millisecond
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet"}, d); code != 1 || !strings.Contains(out.String(), "1 connection(s) as chat (fleet)") {
		t.Fatalf("a client that stays was not refused (exit %d):\n%s", code, out.String())
	}
}

// TestConsolidateRedactionKeepsNames (#2055 review): a short password that is
// also part of database and role names ("fleet") is scrubbed only where it is
// a credential; a long one everywhere; names are never rewritten.
func TestConsolidateRedactionKeepsNames(t *testing.T) {
	p := &consolidatePlan{
		target: "cons_fleet_1", owner: "fleet",
		chat: pgSource{database: "chat_fleet", role: "fleet"}, sched: pgSource{database: "sched", role: "fleet"},
		passwords: []string{"fleet", "placeholder-not-a-secret"},
	}
	in := `database "cons_fleet_1" owned by "fleet"; dsn postgres://fleet:fleet@h/x; password=fleet; leaked placeholder-not-a-secret; postgres://u:placeholder-not-a-secret@h/x`
	got := p.redact(in)
	for _, keep := range []string{`"cons_fleet_1"`, `owned by "fleet"`, "postgres://fleet:<password>@h/x", "password=<password>"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redact lost %q:\n%s", keep, got)
		}
	}
	if strings.Contains(got, "placeholder-not-a-secret") || strings.Contains(got, ":fleet@") {
		t.Errorf("a password survived redaction:\n%s", got)
	}
}
