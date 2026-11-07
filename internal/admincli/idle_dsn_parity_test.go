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
// config.ResolveDSNs, byte for byte, for both stores, across the one-database
// configuration (the store's schema added to the one DSN) and the legacy
// two-database one (#2049). update.sh's idle gate psqls into the stores with
// these DSNs, so a divergence would read the wrong schema.
func TestIdleLibResolveDSNParity(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	cases := []struct {
		name   string
		env    map[string]string
		layout config.Layout
	}{
		{"DATABASE_URL alone", map[string]string{"DATABASE_URL": "postgres://fleet:pw@127.0.0.1:5432/fleet?sslmode=disable"}, config.LayoutSchemas},
		{"DATABASE_URL without a query", map[string]string{"DATABASE_URL": "postgres://fleet@localhost/fleet"}, config.LayoutSchemas},
		{"DATABASE_URL with options", map[string]string{"DATABASE_URL": "postgres://fleet@h:5432/fleet?options=-cstatement_timeout%3D5000&sslmode=require"}, config.LayoutSchemas},
		{"DB_NAME parts", map[string]string{"DB_HOST": "db.internal", "DB_PASSWORD": "päss:1", "DB_NAME": "fleet"}, config.LayoutSchemas},
		{"DB_NAME socket parts", map[string]string{"DB_HOST": "/var/run/postgresql", "DB_NAME": "fleet"}, config.LayoutSchemas},
		{"DB_NAME IPv6 parts", map[string]string{"DB_HOST": "2001:db8::1", "DB_NAME": "fleet", "DB_USER": "app"}, config.LayoutSchemas},
		{"key=value DATABASE_URL", map[string]string{"DATABASE_URL": "host=127.0.0.1 dbname=fleet user=fleet"}, config.LayoutSchemas},
		{"key=value DATABASE_URL with options", map[string]string{"DATABASE_URL": "host=127.0.0.1 options='-c statement_timeout=5000' dbname=fleet"}, config.LayoutSchemas},
		// LEGACY-DB-LAYOUT: remove after consolidation — the rows below.
		{"legacy store keys", map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://chat:a@127.0.0.1:5432/chat", "FLEET_SCHED_DATABASE_URL": "postgres://sched:b@127.0.0.1:5432/sched"}, config.LayoutLegacy},
		{"legacy SCHED_DATABASE_URL + DATABASE_URL", map[string]string{"SCHED_DATABASE_URL": "postgres://s@h/sched", "DATABASE_URL": "postgres://c@h/chat"}, config.LayoutLegacy},
		{"legacy store keys naming one database", map[string]string{"FLEET_CHAT_DATABASE_URL": "postgres://a@h:5432/one", "FLEET_SCHED_DATABASE_URL": "postgres://b@h:5432/one?sslmode=disable"}, config.LayoutSchemas},
		{"legacy bare defaults", map[string]string{}, config.LayoutLegacy},
		{"legacy DB_* parts without DB_NAME", map[string]string{"DB_HOST": "/var/run/postgresql"}, config.LayoutLegacy},
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
			want := config.ResolveDSNs(func(k string) string { return tc.env[k] })
			if want.Layout != tc.layout {
				t.Fatalf("Go layout = %q, want %q", want.Layout, tc.layout)
			}
			for key, w := range map[string]string{"FLEET_CHAT_DATABASE_URL": want.Chat, "FLEET_SCHED_DATABASE_URL": want.Sched} {
				cmd := exec.Command("bash", "-c", `. "$1"; fleet_resolve_dsn "$2" "$3"`, "_", lib, key, envFile)
				cmd.Env = os.Environ()
				for _, k := range dsnEnvKeys {
					cmd.Env = append(cmd.Env, k+"=")
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("fleet_resolve_dsn %s: %v\n%s", key, err, out)
				}
				if got := string(out); got != w {
					t.Errorf("fleet_resolve_dsn %s = %q, want the Go resolver's %q", key, got, w)
				}
			}
		})
	}
}
