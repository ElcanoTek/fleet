package admincli

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestIdleLibIdentityParity pins fleet_dsn_identity to config.DSNIdentity
// (#2055 review round 3: the shell decides the layout from it, so a spelling
// one side sees as "the same database" and the other does not would resolve
// the stores into different schemas).
func TestIdleLibIdentityParity(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	for _, dsn := range []string{
		"postgres://u@h/db", "postgres://v:pw@H:5432/db?sslmode=require", "postgresql://u@h:5433/db",
		"postgres://u@localhost/db", "postgres://u@127.0.0.1:5432/db", "postgres://u@[::1]/db", "postgres://u@[2001:db8::1]:6000/db",
		"postgres://u@/db", "postgres://u@/db?host=%2Fvar%2Frun%2Fpostgresql&port=5444", "postgres://u@/db?host=/tmp",
		"postgres://fleet@h", "postgres://fleet@h/", "postgres://h/db+x", "postgres://u@h/d%20b", "postgres://u@h?dbname=q",
		"host=h dbname=db", "host=h port=5432 dbname=db user=x", "host=/tmp dbname=db", "hostaddr=127.0.0.1 dbname=db",
		"host=h user=fleet", "host=h", "postgres://", "not a dsn",
	} {
		want := config.DSNIdentity(dsn)
		out, err := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; fleet_dsn_identity "$2"`, "_", lib, dsn).CombinedOutput()
		if err != nil {
			t.Fatalf("fleet_dsn_identity %q: %v\n%s", dsn, err, out)
		}
		if got := string(out); got != want {
			t.Errorf("fleet_dsn_identity(%q) = %q, want config.DSNIdentity's %q", dsn, got, want)
		}
	}
}
