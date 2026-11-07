package admincli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// fleetEnv gives the test a one-database fleet of its own (fleetdb) and
// points the CLI's environment at it the way a provisioned box does:
// DATABASE_URL names the database (config.ResolveDSNs adds each store's
// schema), the per-store keys are cleared, and FLEET_ENV_FILE names an empty
// env file so no file on the machine leaks in. It uses t.Setenv, so the test
// cannot be parallel — a verb that resolves the deployment's DSNs reads the
// process environment.
func fleetEnv(t *testing.T) fleetdb.DB {
	t.Helper()
	d := fleetdb.New(t)
	clearDSNEnv(t)
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	t.Setenv("DATABASE_URL", d.DSN)
	return d
}

// clearDSNEnv empties every DSN key either resolver reads (dsnEnvKeys), so
// only what the test sets decides.
func clearDSNEnv(t *testing.T) {
	t.Helper()
	for _, k := range dsnEnvKeys {
		t.Setenv(k, "")
	}
}
