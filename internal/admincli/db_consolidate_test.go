// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// fakePG scripts the superuser side: each query is answered by the first
// rule whose fragment it contains (and whose db matches, when set).
type fakePG struct {
	checkErr     error
	clientMajor  int
	rules        []fakeRule
	execs        []string
	dumpRestores []string
	dumpErr      error
}

type fakeRule struct {
	db, frag string
	rows     [][]string
	err      error
}

func (f *fakePG) Check(context.Context) error { return f.checkErr }
func (f *fakePG) ClientMajor(context.Context) (int, error) {
	return f.clientMajor, nil
}

func (f *fakePG) Query(_ context.Context, db, q string) ([][]string, error) {
	for _, r := range f.rules {
		if (r.db == "" || r.db == db) && strings.Contains(q, r.frag) {
			return r.rows, r.err
		}
	}
	return nil, errors.New("fakePG: unscripted query in " + db + ": " + q)
}

func (f *fakePG) Exec(_ context.Context, db, q string) error {
	f.execs = append(f.execs, db+": "+q)
	return nil
}

func (f *fakePG) DumpRestore(_ context.Context, src, schema, dst, role string) error {
	f.dumpRestores = append(f.dumpRestores, src+"/"+schema+"->"+dst+" as "+role)
	return f.dumpErr
}

// prepend puts rules ahead of the defaults so a test can override one answer.
func (f *fakePG) prepend(rules ...fakeRule) { f.rules = append(rules, f.rules...) }

const (
	testChatDSN  = "postgres://chat:placeholder-chat-pw@127.0.0.1:5432/chat?sslmode=disable"
	testSchedDSN = "postgres://sched:placeholder-sched-pw@127.0.0.1:5432/sched?sslmode=disable"
)

func healthyFake() *fakePG {
	inv := func(db string) []fakeRule {
		return []fakeRule{
			{db: db, frag: "query_to_xml", rows: [][]string{{"schema_migrations", "3"}, {"users", "2"}}},
			{db: db, frag: "pg_sequences", rows: [][]string{{"users_id_seq", "2"}}},
			{db: db, frag: "schema_migrations t", rows: [][]string{{"(1),(2),(3)"}}},
		}
	}
	f := &fakePG{clientMajor: 16}
	f.rules = append(f.rules,
		fakeRule{frag: "current_setting('server_version_num')", rows: [][]string{{"160004", "5432"}}},
		fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "t"}, {"sched", "sched", "t"}}},
		fakeRule{frag: "FROM pg_roles", rows: [][]string{{"1"}}},
		fakeRule{frag: "pg_stat_activity", rows: nil},
		fakeRule{frag: "bool_or(dirty)", rows: [][]string{{"f"}}},
		fakeRule{frag: "pg_database_size", rows: [][]string{{"1048576", "/var/lib/pgsql/data"}}},
		fakeRule{frag: "pg_get_userbyid(owner) <>", rows: nil},
		fakeRule{frag: "public_objects", rows: [][]string{{"0"}}},
		fakeRule{frag: "store_objects", rows: [][]string{{"0"}}},
		fakeRule{frag: "pg_postmaster_start_time", rows: [][]string{{"1793000000.5 port 5432 cluster 16/main"}}},
		fakeRule{frag: "pg_control_system", rows: [][]string{{"7701435355509418483"}}},
	)
	f.rules = append(f.rules, inv("chat")...)
	f.rules = append(f.rules, inv("sched")...)
	f.rules = append(f.rules, inv("fleet")...) // the target, for verification
	// The scratch copies of the (closed) sources: what the target is
	// verified against.
	f.rules = append(f.rules, inv("fleet_consolidate_chat")...)
	f.rules = append(f.rules, inv("fleet_consolidate_sched")...)
	f.rules = append(f.rules,
		fakeRule{frag: "to_regclass('public.schema_migrations') IS NOT NULL,\n", rows: [][]string{{"t", "0"}}},
	)
	return f
}

func testDeps(f *fakePG, out *bytes.Buffer) consolidateDeps {
	return consolidateDeps{
		pg:           f,
		unitState:    func(string) string { return "inactive" },
		freeBytes:    func(string) (uint64, error) { return 1 << 40, nil },
		verifyStores: func(context.Context, string, string) error { return nil },
		// The DSN side of the instance check answers from the same rules as
		// the superuser side: one cluster.
		dsnQuery: func(ctx context.Context, _ string, q string) ([][]string, error) { return f.Query(ctx, "postgres", q) },
		now:      func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
		out:      out,
	}
}

func writeLegacyEnv(t *testing.T, extra string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fleet.env")
	body := "OPENROUTER_API_KEY=placeholder\nFLEET_CHAT_DATABASE_URL=" + testChatDSN + "\nFLEET_SCHED_DATABASE_URL=" + testSchedDSN + "\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearDSNEnv(t *testing.T) {
	for _, k := range dsnEnvKeys {
		t.Setenv(k, "")
	}
}

func TestConsolidateDryRunPrintsPlanAndChangesNothing(t *testing.T) {
	clearDSNEnv(t)
	env := writeLegacyEnv(t, "")
	before, _ := os.ReadFile(env)
	f := healthyFake()
	var out bytes.Buffer
	code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet"}, testDeps(f, &out))
	if code != 0 {
		t.Fatalf("dry run exit %d:\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"DRY RUN", `database "chat", role "chat"`, `owned by role "sched"`, "chat.users", "Dry run complete", "CREATE DATABASE fleet_consolidate_chat TEMPLATE chat", "--role=sched"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{"placeholder-chat-pw", "placeholder-sched-pw"} {
		if strings.Contains(got, secret) {
			t.Errorf("dry-run output leaks a password:\n%s", got)
		}
	}
	if len(f.execs) != 0 || len(f.dumpRestores) != 0 {
		t.Errorf("dry run changed something: execs %v, copies %v", f.execs, f.dumpRestores)
	}
	if after, _ := os.ReadFile(env); !bytes.Equal(before, after) {
		t.Error("dry run rewrote the env file")
	}
}

func TestConsolidatePreflightRefusals(t *testing.T) {
	clearDSNEnv(t)
	cases := []struct {
		name  string
		setup func(f *fakePG, d *consolidateDeps)
		opts  func(o *consolidateOptions)
		want  string
	}{
		{"service running", func(_ *fakePG, d *consolidateDeps) {
			d.unitState = func(u string) string {
				if u == "fleet.service" {
					return "active"
				}
				return "inactive"
			}
		}, nil, "still running: fleet.service (active)"},
		{"timer active", func(_ *fakePG, d *consolidateDeps) {
			d.unitState = func(u string) string {
				if u == "fleet-backup.timer" {
					return "active"
				}
				return "inactive"
			}
		}, nil, "systemctl stop fleet-backup.timer"},
		{"client connections", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{frag: "pg_stat_activity", rows: [][]string{{"chat", "chat", "fleet", "3"}}})
		}, nil, "chat: 3 connection(s) as chat (fleet)"},
		{"client tools missing", func(f *fakePG, _ *consolidateDeps) { f.checkErr = errors.New("not found on PATH: pg_restore") }, nil, "pg_restore"},
		{"old pg_dump", func(f *fakePG, _ *consolidateDeps) { f.clientMajor = 15 }, nil, "pg_dump 15 is older than the server (16)"},
		{"source missing", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "t"}}})
		}, nil, `database "sched" not found`},
		{"source not a fleet store", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{db: "chat", frag: "to_regclass('public.schema_migrations') IS NOT NULL,\n", rows: [][]string{{"f", "0"}}})
		}, nil, "not a fleet store"},
		{"source already has schemas", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{db: "sched", frag: "to_regclass('public.schema_migrations') IS NOT NULL,\n", rows: [][]string{{"t", "1"}}})
		}, nil, "already has a chat or sched schema"},
		{"dirty sched", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{frag: "bool_or(dirty)", rows: [][]string{{"t"}}})
		}, nil, "DIRTY"},
		{"target half filled", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(
				fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "t"}, {"sched", "sched", "t"}, {"fleet", "sched", "t"}}},
				fakeRule{db: "fleet", frag: "store_objects", rows: [][]string{{"12"}}},
			)
		}, nil, "--force-clean"},
		{"target owned by someone else", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "t"}, {"sched", "sched", "t"}, {"fleet", "postgres", "t"}}})
		}, nil, `owned by "postgres"`},
		{"target holds a legacy store", func(f *fakePG, _ *consolidateDeps) {
			f.prepend(
				fakeRule{frag: "FROM pg_database WHERE datname IN", rows: [][]string{{"chat", "chat", "t"}, {"sched", "sched", "t"}, {"fleet", "sched", "t"}}},
				fakeRule{db: "fleet", frag: "public_objects", rows: [][]string{{"40"}}},
			)
		}, nil, "has 40 object(s) in public"},
		{"no disk", func(_ *fakePG, d *consolidateDeps) {
			d.freeBytes = func(string) (uint64, error) { return 1024, nil }
		}, nil, "need about"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := writeLegacyEnv(t, "")
			f := healthyFake()
			var out bytes.Buffer
			d := testDeps(f, &out)
			tc.setup(f, &d)
			o := consolidateOptions{envFile: env, targetDB: "fleet", apply: true}
			if tc.opts != nil {
				tc.opts(&o)
			}
			if code := runConsolidate(t.Context(), o, d); code != 1 {
				t.Fatalf("exit %d, want the preflight refusal (1):\n%s", code, out.String())
			}
			if !strings.Contains(out.String(), tc.want) || !strings.Contains(out.String(), "Nothing was changed") {
				t.Errorf("output lacks %q:\n%s", tc.want, out.String())
			}
			if len(f.execs) != 0 || len(f.dumpRestores) != 0 {
				t.Errorf("a refused run changed something: %v %v", f.execs, f.dumpRestores)
			}
		})
	}
}

func TestConsolidatePlanRefusals(t *testing.T) {
	clearDSNEnv(t)
	cases := []struct {
		name, env, target, want string
	}{
		{"remote server", "FLEET_CHAT_DATABASE_URL=postgres://c@db.example.com/chat\nFLEET_SCHED_DATABASE_URL=postgres://s@db.example.com/sched\n", "fleet", "not this box"},
		{"two servers", "FLEET_CHAT_DATABASE_URL=postgres://c@127.0.0.1:5432/chat\nFLEET_SCHED_DATABASE_URL=postgres://s@127.0.0.1:5433/sched\n", "fleet", "do not name the same Postgres instance"},
		{"socket vs loopback", "FLEET_CHAT_DATABASE_URL=postgres://c@/chat?host=/run/postgresql&port=5432\nFLEET_SCHED_DATABASE_URL=postgres://s@127.0.0.1:5432/sched\n", "fleet", "do not name the same Postgres instance"},
		{"two socket dirs", "FLEET_CHAT_DATABASE_URL=postgres://c@/chat?host=/run/a\nFLEET_SCHED_DATABASE_URL=postgres://s@/sched?host=/run/b\n", "fleet", "do not name the same Postgres instance"},
		{"target is a source", "FLEET_CHAT_DATABASE_URL=postgres://c@127.0.0.1/chat\nFLEET_SCHED_DATABASE_URL=postgres://s@127.0.0.1/fleet\n", "fleet", "is the sched source"},
		{"bad target name", "FLEET_CHAT_DATABASE_URL=postgres://c@127.0.0.1/chat\nFLEET_SCHED_DATABASE_URL=postgres://s@127.0.0.1/sched\n", "Fleet;", "--target-db"},
		{"keyword DSN", "FLEET_CHAT_DATABASE_URL=host=127.0.0.1 dbname=chat\nFLEET_SCHED_DATABASE_URL=postgres://s@127.0.0.1/sched\n", "fleet", "URL DSNs only"},
		{"nothing configured", "OPENROUTER_API_KEY=x\n", "fleet", "configures no database"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), "fleet.env")
			if err := os.WriteFile(env, []byte(tc.env), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := buildPlan(consolidateOptions{envFile: env, targetDB: tc.target}, testDeps(healthyFake(), &bytes.Buffer{}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildPlan = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestConsolidateAlreadyOneDatabase(t *testing.T) {
	clearDSNEnv(t)
	env := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(env, []byte("DATABASE_URL=postgres://fleet@127.0.0.1/fleet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := healthyFake()
	var out bytes.Buffer
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "Nothing to do") || len(f.execs) != 0 {
		t.Errorf("an already consolidated box was touched:\n%s", out.String())
	}
}

func TestConsolidateApplyRewritesEnvAfterVerification(t *testing.T) {
	clearDSNEnv(t)
	env := writeLegacyEnv(t, "DATABASE_URL="+testChatDSN+"\n")
	f := healthyFake()
	var out bytes.Buffer
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 0 {
		t.Fatalf("apply exit %d:\n%s", code, out.String())
	}
	if len(f.dumpRestores) != 2 || f.dumpRestores[0] != "fleet_consolidate_chat/chat->fleet as sched" {
		t.Errorf("copies = %v", f.dumpRestores)
	}
	joined := strings.Join(f.execs, "\n")
	for _, want := range []string{`CREATE DATABASE "fleet" OWNER "sched"`, `CREATE DATABASE "fleet_consolidate_chat" TEMPLATE "chat"`, `ALTER SCHEMA public RENAME TO "sched"`, `DROP DATABASE IF EXISTS "fleet_consolidate_sched"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("execs lack %q:\n%s", want, joined)
		}
	}
	vals, err := creds.ReadEnvValues(env)
	if err != nil {
		t.Fatal(err)
	}
	if vals["DATABASE_URL"] != "postgres://sched:placeholder-sched-pw@127.0.0.1:5432/fleet?sslmode=disable" {
		t.Errorf("DATABASE_URL = %q", vals["DATABASE_URL"])
	}
	for _, k := range legacyEnvKeys {
		if vals[k] != "" {
			t.Errorf("%s still set after consolidation", k)
		}
	}
	d := config.ResolveDSNs(func(k string) string { return vals[k] })
	if d.Layout != config.LayoutSchemas {
		t.Errorf("rewritten env resolves to %q", d.Layout)
	}
	raw, _ := os.ReadFile(env)
	if !strings.Contains(string(raw), "# [fleet db consolidate 2026-11-07: now DATABASE_URL, database fleet] FLEET_CHAT_DATABASE_URL=") {
		t.Errorf("retired keys not commented with a dated note:\n%s", raw)
	}
	if strings.Count(string(raw), "\nDATABASE_URL=") != 1 {
		t.Errorf("want exactly one live DATABASE_URL line:\n%s", raw)
	}
	backup := env + ".pre-consolidate-20261107T120000Z"
	b, err := os.ReadFile(backup)
	if err != nil || !strings.Contains(string(b), "FLEET_SCHED_DATABASE_URL="+testSchedDSN) {
		t.Errorf("backup %s missing or wrong: %v", backup, err)
	}
	if st, err := os.Stat(backup); err == nil && st.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", st.Mode().Perm())
	}
	got := out.String()
	for _, want := range []string{"Rollback", "cp -p " + backup + " " + env, `DROP DATABASE "chat"`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "placeholder-sched-pw") {
		t.Errorf("output leaks the password:\n%s", got)
	}
}

func TestConsolidateFailedVerificationKeepsEnv(t *testing.T) {
	clearDSNEnv(t)
	for name, mutate := range map[string]func(f *fakePG, d *consolidateDeps){
		"row count mismatch": func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{db: "fleet", frag: "query_to_xml", rows: [][]string{{"schema_migrations", "3"}, {"users", "1"}}})
		},
		"sequence mismatch": func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{db: "fleet", frag: "pg_sequences", rows: [][]string{{"users_id_seq", "unused"}}})
		},
		"ownership": func(f *fakePG, _ *consolidateDeps) {
			f.prepend(fakeRule{frag: "pg_get_userbyid(owner) <>", rows: [][]string{{"relation chat.users"}}})
		},
		"store check": func(_ *fakePG, d *consolidateDeps) {
			d.verifyStores = func(context.Context, string, string) error { return errors.New("pending migration") }
		},
		"copy fails": func(f *fakePG, _ *consolidateDeps) { f.dumpErr = errors.New("pg_restore: boom") },
	} {
		t.Run(name, func(t *testing.T) {
			env := writeLegacyEnv(t, "")
			before, _ := os.ReadFile(env)
			f := healthyFake()
			var out bytes.Buffer
			d := testDeps(f, &out)
			mutate(f, &d)
			if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet", apply: true}, d); code != 5 {
				t.Fatalf("exit %d, want 5:\n%s", code, out.String())
			}
			if after, _ := os.ReadFile(env); !bytes.Equal(before, after) {
				t.Error("the env file changed although verification failed")
			}
			if !strings.Contains(out.String(), "The env file was NOT changed") {
				t.Errorf("output does not say the box is unchanged:\n%s", out.String())
			}
			if m, _ := filepath.Glob(env + ".pre-consolidate-*"); len(m) != 0 {
				t.Errorf("backup written before verification passed: %v", m)
			}
			// The scratch copies are always dropped.
			if !strings.Contains(strings.Join(f.execs, "\n"), `DROP DATABASE IF EXISTS "fleet_consolidate_chat"`) {
				t.Errorf("scratch copy not dropped: %v", f.execs)
			}
		})
	}
}
