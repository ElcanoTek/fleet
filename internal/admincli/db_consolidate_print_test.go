// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestConsolidatePrintedCommandsReachTheInstance (#2055 review): every psql
// command the tool prints for the operator carries the same socket
// directory and port the runner used, shell-quoted so it survives a paste.
func TestConsolidatePrintedCommandsReachTheInstance(t *testing.T) {
	clearDSNEnv(t)
	env := filepath.Join(t.TempDir(), "fleet.env")
	body := "FLEET_CHAT_DATABASE_URL=postgres://chat:placeholder-not-a-secret@/chat?host=/run/pg%20dir&port=5444\n" +
		"FLEET_SCHED_DATABASE_URL=postgres://sched:placeholder-not-a-secret@/sched?host=/run/pg%20dir&port=5444\n"
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := healthyFake()
	f.prepend(fakeRule{frag: "current_setting('server_version_num')", rows: [][]string{{"160004", "5444"}}})
	var out bytes.Buffer
	if code := runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet", apply: true}, testDeps(f, &out)); code != 0 {
		t.Fatalf("apply exit %d:\n%s", code, out.String())
	}
	var lines []string
	for _, ln := range strings.Split(out.String(), "\n") {
		if i := strings.Index(ln, "runuser -u postgres -- psql "); i >= 0 && strings.Contains(ln, " -c ") {
			lines = append(lines, ln[i:])
		}
	}
	want := [][]string{
		// Rollback: re-open the sources the move left closed.
		{"runuser", "-u", "postgres", "--", "psql", "-h", "/run/pg dir", "-p", "5444", "-c", `ALTER DATABASE "chat" ALLOW_CONNECTIONS true`, "-c", `ALTER DATABASE "sched" ALLOW_CONNECTIONS true`},
		{"runuser", "-u", "postgres", "--", "psql", "-h", "/run/pg dir", "-p", "5444", "-c", `DROP DATABASE "chat"`, "-c", `DROP DATABASE "sched"`},
		{"runuser", "-u", "postgres", "--", "psql", "-h", "/run/pg dir", "-p", "5444", "-c", `DROP ROLE "chat"`},
	}
	if len(lines) != len(want) {
		t.Fatalf("want the re-open, database drop and role drop commands, got %q\n%s", lines, out.String())
	}
	for i, ln := range lines {
		// Let bash split it, exactly as a pasted command would be.
		argv, err := exec.Command("bash", "-c", `eval "set -- $1"; printf '%s\n' "$@"`, "_", ln).Output()
		if err != nil {
			t.Fatalf("bash could not parse %q: %v", ln, err)
		}
		if got := strings.Split(strings.TrimSuffix(string(argv), "\n"), "\n"); !reflect.DeepEqual(got, want[i]) {
			t.Errorf("printed command %q parses to %q, want %q", ln, got, want[i])
		}
	}
	if !strings.Contains(out.String(), "superuser work as runuser -u postgres -- psql/pg_dump/pg_restore -h '/run/pg dir' -p 5444") {
		t.Errorf("the plan does not name the instance:\n%s", out.String())
	}
}
