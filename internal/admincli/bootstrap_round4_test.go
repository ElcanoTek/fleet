package admincli

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/creds"
)

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func mustQueryString(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	var s string
	if err := db.QueryRow(q).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEnvValueMatchesFleetsParser pins lib/envfile.sh's fleet_env_value to
// fleet's own env-file parser (creds.ReadEnvValues → ParseEnvLine), which
// config.Load and the admin CLI use: the scripts must decide from the value
// fleet will see, not from raw text (#2055 review).
func TestEnvValueMatchesFleetsParser(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "envfile.sh")
	body := strings.Join([]string{
		`# a comment`,
		`EMPTY_DQ=""`,
		`EMPTY_SQ=''`,
		`EMPTY=`,
		`SPACES=   `,
		`export EXPORTED=postgres://u@h/db`,
		`  INDENTED = postgres://u@h/indented  `,
		`COMMENTED=postgres://u@h/db # trailing note`,
		"TABBED=postgres://u@h/db\t# note",
		`BOTH=a\t#b #c`,
		`QUOTED="postgres://u@h/db # not a comment"`,
		`SQUOTED='x y'`,
		`LAST=first`,
		`LAST=second`,
		`#COMMENTED_OUT=postgres://u@h/db`,
		`# [fleet db consolidate 2026-11-07] FLEET_CHAT_DATABASE_URL=postgres://u@h/chat`,
		`HALFQUOTE="abc`,
		"CRLF=value\r",
	}, "\n") + "\n"
	file := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := creds.ReadEnvValues(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"EMPTY_DQ", "EMPTY_SQ", "EMPTY", "SPACES", "EXPORTED", "INDENTED", "COMMENTED", "TABBED", "BOTH",
		"QUOTED", "SQUOTED", "LAST", "COMMENTED_OUT", "FLEET_CHAT_DATABASE_URL", "HALFQUOTE", "CRLF", "ABSENT"} {
		out, err := exec.Command("bash", "-c", `. "$1"; fleet_env_value "$2" "$3"`, "_", lib, file, key).Output()
		if err != nil {
			t.Fatalf("fleet_env_value %s: %v", key, err)
		}
		// ResolveDSNs trims what it reads; fleet_env_value returns it trimmed.
		if got, w := string(out), strings.TrimSpace(strings.TrimSuffix(want[key], "\r")); got != w {
			t.Errorf("fleet_env_value %s = %q, want fleet's %q", key, got, w)
		}
	}
}

// TestBootstrapLayoutFromParsedValues (#2055 review): the layout decision
// reads the env file as fleet does — FLEET_CHAT_DATABASE_URL="" is unset, a
// commented-out key is absent, an `export`ed one counts.
func TestBootstrapLayoutFromParsedValues(t *testing.T) {
	const one = "DATABASE_URL=postgres://fleet:placeholder-not-a-secret@127.0.0.1:5432/fleet?sslmode=disable\n"
	for name, tc := range map[string]struct {
		extra  string
		legacy bool
	}{
		"empty per-store keys":         {"FLEET_CHAT_DATABASE_URL=\"\"\nFLEET_SCHED_DATABASE_URL=\nSCHED_DATABASE_URL=''\n", false},
		"commented-out per-store keys": {"# [fleet db consolidate 2026-11-07] FLEET_CHAT_DATABASE_URL=postgres://c:x@127.0.0.1/chat\n", false},
		"an exported per-store key":    {"export FLEET_SCHED_DATABASE_URL=postgres://s:x@127.0.0.1:5432/sched\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			envFile := filepath.Join(t.TempDir(), "fleet.env")
			if err := os.WriteFile(envFile, []byte(one+tc.extra), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh", "--dry-run", "--postgres=local")
			if err != nil {
				t.Fatalf("exit: %v\n%s", err, out)
			}
			if got := strings.Contains(out, "legacy two-database layout"); got != tc.legacy {
				t.Errorf("legacy = %v, want %v\n%s", got, tc.legacy, out)
			}
		})
	}
}

// TestDSNIsLocalDefault (#2055 review): the local-cluster check parses the
// port — 5432 exactly, never 54321 — from the authority and from a decoded
// port query parameter.
func TestDSNIsLocalDefault(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "pgprovision.sh")
	for dsn, want := range map[string]bool{
		"postgres://u:p@127.0.0.1:5432/db?sslmode=disable":     true,
		"postgres://u:p@127.0.0.1/db":                          true,
		"postgres://u:p@localhost:5432/db":                     true,
		"postgres://u:p@[::1]:5432/db":                         true,
		"postgresql://u@/db":                                   true,
		"postgres://u@127.0.0.1/db?port=5432":                  true,
		"postgres://u@127.0.0.1/db?port=%35%34%33%32":          true,
		"postgres://u@127.0.0.1:54321/db":                      false,
		"postgres://u@127.0.0.1:5433/db":                       false,
		"postgres://u@127.0.0.1/db?port=54321":                 false,
		"postgres://u@127.0.0.1/db?sslmode=disable&port=5432x": false,
		"postgres://u@127.0.0.1/db?port=5432&port=5433":        false,
		"postgres://u@db.example.com:5432/db":                  false,
		"postgres://u@127.0.0.1/db?host=/tmp":                  false,
		"postgres://u@127.0.0.1/db?hostaddr=10.0.0.1":          false,
		"postgres://u@127.0.0.1:5432:1/db":                     false,
		"postgres://u@[::1]x/db":                               false,
		"host=127.0.0.1 dbname=db":                             false,
		"postgres://u@127.0.0.1":                               false,
	} {
		err := exec.Command("bash", "-c", `. "$1"; fleet_dsn_is_local_default "$2"`, "_", lib, dsn).Run()
		if (err == nil) != want {
			t.Errorf("fleet_dsn_is_local_default(%q) = %v, want %v", dsn, err == nil, want)
		}
	}
}

// TestDoctorDecidesLayoutFromParsedValues: doctor.sh's database check reads
// the DSN keys with fleet_env_value, never raw text (#2055 review).
func TestDoctorDecidesLayoutFromParsedValues(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, key := range []string{"FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL", "DATABASE_URL"} {
		if strings.Contains(src, "env_get "+key+")") {
			t.Errorf("doctor.sh reads %s with env_get (raw text), not fleet_env_value", key)
		}
		if !strings.Contains(src, `fleet_env_value "$ENV_FILE" `+key+`)`) {
			t.Errorf("doctor.sh does not read %s with fleet_env_value", key)
		}
	}
}
