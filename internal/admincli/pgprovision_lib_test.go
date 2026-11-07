// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pgProvisionBox is a scratch box for scripts/lib/pgprovision.sh: a fake
// runuser that drops "-u postgres --" and runs the rest, and a fake psql that
// fails the way a real one can, quoting the statement it was given (every SQL
// line it read, as "LINE n: ...") on stderr. It also records what its stderr
// is (a pipe, or a file on disk) for the test.
type pgProvisionBox struct {
	path, tmp, stderrKind string
}

func newPgProvisionBox(t *testing.T, psqlExit int) *pgProvisionBox {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	b := &pgProvisionBox{tmp: t.TempDir()}
	b.stderrKind = filepath.Join(t.TempDir(), "stderr-kind")
	bin := toolsOnlyPath(t, "awk", "cat", "stat")
	runuser := "#!" + bash + "\nwhile [[ $# -gt 0 && $1 != -- ]]; do shift; done\nshift\nexec \"$@\"\n"
	psql := "#!" + bash + "\n" +
		"stat -L -c %F /proc/self/fd/2 > '" + b.stderrKind + "'\n" +
		"n=0; while IFS= read -r line; do n=$((n+1)); printf 'LINE %d: %s\\n' \"$n\" \"$line\" >&2; done\n" +
		"printf 'psql:<stdin>:3: ERROR:  syntax error\\n' >&2\n"
	psql += fmt.Sprintf("exit %d\n", psqlExit)
	for name, body := range map[string]string{"runuser": runuser, "psql": psql} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.path = bin
	return b
}

// script provisions one chat and one sched pair the way bootstrap does: the
// real SQL piped into fleet_psql_provision with both passwords.
const pgProvisionScript = `set -euo pipefail
. "$1"
sql="$(fleet_role_db_sql chat "$CHAT_PW" chat)"$'\n'"$(fleet_role_db_sql sched "$SCHED_PW" sched)"
rc=0
printf '%s\n' "$sql" | fleet_psql_provision "$CHAT_PW" "$SCHED_PW" || rc=$?
printf 'RC=%s\n' "$rc"
`

func (b *pgProvisionBox) command(t *testing.T, chatPW, schedPW string) *exec.Cmd {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "pgprovision.sh")
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, "-c", pgProvisionScript, "pgprovision", lib)
	// The passwords reach the script through its environment, as bootstrap's
	// do, so the only way they can show up in the output is through psql.
	cmd.Env = []string{"PATH=" + b.path, "TMPDIR=" + b.tmp, "CHAT_PW=" + chatPW, "SCHED_PW=" + schedPW}
	return cmd
}

// requireStderrInMemory: psql's raw stderr holds the passwords, so it must go
// to a pipe the shell reads into memory, never to a file (a temp file
// outlives a SIGTERM or SIGKILL, which skip an EXIT trap), and nothing may be
// left in TMPDIR.
func (b *pgProvisionBox) requireStderrInMemory(t *testing.T) {
	t.Helper()
	kind, err := os.ReadFile(b.stderrKind)
	if err != nil {
		t.Fatalf("psql never ran: %v", err)
	}
	if got := strings.TrimSpace(string(kind)); got != "fifo" {
		t.Errorf("psql's stderr is a %q, want a pipe (fifo) read into memory", got)
	}
	entries, err := os.ReadDir(b.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("provisioning left %d file(s) in TMPDIR", len(entries))
	}
}

// TestPsqlProvisionNeverPrintsPasswords — a real run's psql error can quote
// the failing statement, and on an installed box its passwords are the live
// ones from the env file. fleet_psql_provision must show the error with each
// password replaced by the plan's placeholder, whatever the password holds
// (gen_pass's letters and digits, or an env-file password with any character
// but "@", "/" and a line break, regex metacharacters and awk's "&" and "\"
// included), and when one password contains the other.
func TestPsqlProvisionNeverPrintsPasswords(t *testing.T) {
	t.Parallel()

	special := `p.*+?[x](y){2}|^$&\1\\'"%s`
	for _, tc := range []struct{ name, chat, sched string }{
		{"gen_pass", "Q7xR2mT9kLp4Vb8NcZ1wHs6J", "u3Ey8Pq1Wn5Rt0Ko2Lm7Ji4G"},
		{"env-file characters", special, "sched" + "#!;~` <>=,:" + "end"},
		{"one contains the other", special, "pre" + special + "post"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newPgProvisionBox(t, 3)
			out, err := b.command(t, tc.chat, tc.sched).CombinedOutput()
			if err != nil {
				t.Fatalf("script failed: %v\n%s", err, out)
			}
			text := string(out)
			if !strings.Contains(text, "RC=3") {
				t.Errorf("fleet_psql_provision did not return psql's status 3\n%s", text)
			}
			for _, secret := range []string{tc.chat, tc.sched} {
				if strings.Contains(text, secret) {
					t.Errorf("psql's error output reached the terminal with a password in it\n%s", text)
				}
			}
			for _, want := range []string{
				"ALTER ROLE chat WITH LOGIN PASSWORD '<chat db password>';",
				"ALTER ROLE sched WITH LOGIN PASSWORD '<sched db password>';",
				"ERROR:  syntax error",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("redacted psql error missing %q\n%s", want, text)
				}
			}
			b.requireStderrInMemory(t)
		})
	}
}

// TestPsqlProvisionSuccessIsQuiet — a run that works prints nothing, returns
// 0 and leaves no temp file.
func TestPsqlProvisionSuccessIsQuiet(t *testing.T) {
	t.Parallel()

	b := newPgProvisionBox(t, 0)
	out, err := b.command(t, "Q7xR2mT9kLp4Vb8NcZ1wHs6J", "u3Ey8Pq1Wn5Rt0Ko2Lm7Ji4G").CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	if string(out) != "RC=0\n" {
		t.Errorf("a successful provisioning run printed more than its status:\n%s", out)
	}
	b.requireStderrInMemory(t)
}

// TestBootstrapProvisionsThroughRedactingRunner pins bootstrap.sh to the lib:
// the real run pipes its SQL into fleet_psql_provision with both passwords,
// and never into a bare psql whose stderr reaches the terminal.
func TestBootstrapProvisionsThroughRedactingRunner(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, want := range []string{
		`. "$SCRIPT_DIR/lib/pgprovision.sh"`,
		`printf '%s\n' "$PSQL_SQL" | fleet_psql_provision "$CHAT_DB_PASSWORD" "$SCHED_DB_PASSWORD"`,
		`printf '%s\n' "$PSQL_PLAN" | sed`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("bootstrap.sh must contain %q", want)
		}
	}
	if strings.Contains(src, `"$PSQL_SQL" | runuser`) {
		t.Error("bootstrap.sh pipes the provisioning SQL into a bare psql, whose errors can print the passwords")
	}
}
