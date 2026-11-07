package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// dsnEnvKeys is every key either resolver reads; the shell is run with all
// of them cleared so only the fixture's env file decides.
var dsnEnvKeys = []string{
	"DATABASE_URL", "FLEET_CHAT_DATABASE_URL", "FLEET_SCHED_DATABASE_URL", "SCHED_DATABASE_URL",
	"DB_HOST", "DB_PORT", "DB_USER", "DB_PASSWORD", "DB_NAME", "DB_SSLMODE",
}

// TestIdleLibResolveDSNParity pins scripts/lib/idle.sh's fleet_resolve_dsn to
// config.ResolveDSNs, byte for byte, for both stores: the store's schema
// added to the one DSN (#2049), and the refusal of a retired per-store key.
// update.sh's idle gate psqls into the stores with these DSNs, so a
// divergence would read the wrong schema.
func TestIdleLibResolveDSNParity(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"DATABASE_URL alone", map[string]string{"DATABASE_URL": "postgres://fleet:pw@127.0.0.1:5432/fleet?sslmode=disable"}},
		{"DATABASE_URL without a query", map[string]string{"DATABASE_URL": "postgres://fleet@localhost/fleet"}},
		{"DATABASE_URL with options", map[string]string{"DATABASE_URL": "postgres://fleet@h:5432/fleet?options=-cstatement_timeout%3D5000&sslmode=require"}},
		{"DB_NAME parts", map[string]string{"DB_HOST": "db.internal", "DB_PASSWORD": "päss:1", "DB_NAME": "fleet"}},
		{"DB_NAME socket parts", map[string]string{"DB_HOST": "/var/run/postgresql", "DB_NAME": "fleet"}},
		{"DB_NAME IPv6 parts", map[string]string{"DB_HOST": "2001:db8::1", "DB_NAME": "fleet", "DB_USER": "app"}},
		{"key=value DATABASE_URL", map[string]string{"DATABASE_URL": "host=127.0.0.1 dbname=fleet user=fleet"}},
		{"key=value DATABASE_URL with options", map[string]string{"DATABASE_URL": "host=127.0.0.1 options='-c statement_timeout=5000' dbname=fleet"}},
		{"bare defaults: fleet/fleet", map[string]string{}},
		{"DB_* parts without DB_NAME", map[string]string{"DB_HOST": "/var/run/postgresql"}},
		{"retired store keys are refused", map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://chat:a@127.0.0.1:5432/chat", "FLEET_SCHED_DATABASE_URL": "postgres://sched:b@127.0.0.1:5432/sched"}},
		{"retired SCHED_DATABASE_URL next to DATABASE_URL is refused", map[string]string{"SCHED_DATABASE_URL": "postgres://s@h/sched", "DATABASE_URL": "postgres://c@h/fleet"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			for k, v := range tc.env {
				b.WriteString(k + "=" + v + "\n")
			}
			envFile := filepath.Join(t.TempDir(), "fleet.env")
			if err := os.WriteFile(envFile, []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			want, werr := config.ResolveDSNs(func(k string) string { return tc.env[k] })
			for key, w := range map[string]string{"FLEET_CHAT_DATABASE_URL": want.Chat, "FLEET_SCHED_DATABASE_URL": want.Sched} {
				cmd := exec.Command("bash", "-c", `. "$1"; fleet_resolve_dsn "$2" "$3"`, "_", lib, key, envFile)
				cmd.Env = os.Environ()
				for _, k := range dsnEnvKeys {
					cmd.Env = append(cmd.Env, k+"=")
				}
				var stdout, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err := cmd.Run()
				if werr != nil {
					if err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), "set DATABASE_URL") {
						t.Errorf("fleet_resolve_dsn %s = %q (err %v, stderr %q), want the refusal the Go resolver gives: %v", key, stdout.String(), err, stderr.String(), werr)
					}
					continue
				}
				if err != nil {
					t.Fatalf("fleet_resolve_dsn %s: %v\n%s", key, err, stderr.String())
				}
				if got := stdout.String(); got != w {
					t.Errorf("fleet_resolve_dsn %s = %q, want the Go resolver's %q", key, got, w)
				}
			}
		})
	}
}
