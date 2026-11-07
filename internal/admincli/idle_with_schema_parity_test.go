package admincli

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestIdleLibWithSchemaParity pins fleet_dsn_with_schema to config.WithSchema
// byte for byte, including DSNs that already carry options (#2055 review: the
// shell used to leave a key=value DSN with an options pair without a schema).
func TestIdleLibWithSchemaParity(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "idle.sh")
	for _, dsn := range []string{
		"postgres://u@h:5432/db",
		"postgres://u@h:5432/db?sslmode=disable",
		"postgres://u@h:5432/db?options=-ca%3D1&sslmode=disable",
		"postgres://u@h:5432/db?sslmode=disable&options=-c+a%3D1#frag",
		"host=h dbname=db",
		"host=h options=-cwork_mem=4MB dbname=db",
		"host=h options='-c work_mem=4MB' dbname=db",
		"options=-ca=1 host=h options='-c b=2'",
		`host=h password='a b\'c' dbname=db`,
		`host=h options='-ca=1 -csearch_path=sched'`,
		"host = h  options = '-ca=1'",
		// #2055 review round 3: top-level search_path, other encodings,
		// several options parameters, a switch to replace.
		"postgres://u@h/db?search_path=other&sslmode=disable",
		"postgres://u@h/db?search_path=a&options=-ca%3D1&search_path=b",
		"postgres://u@h/db?options=-c%20a%3D1&sslmode=disable",
		"postgres://u@h/db?options=-csearch_path%3Dsched&sslmode=disable",
		"postgres://u@h/db?options=-ca%3D1&options=-cb%3D2",
		"postgres://u@h/db?options=%ZZ&options=-cb%3D2",
		"  postgres://u@h/db  ",
		"search_path=other host=h dbname=db",
		"host=h search_path='a, b' options='-ca=1' dbname=x",
		"  host=h dbname=db  ",
	} {
		for _, schema := range []string{config.ChatSchema, config.SchedSchema} {
			want := config.WithSchema(dsn, schema)
			out, err := exec.Command("bash", "-c", `set -euo pipefail; . "$1"; fleet_dsn_with_schema "$2" "$3"`, "_", lib, dsn, schema).CombinedOutput()
			if err != nil {
				t.Fatalf("fleet_dsn_with_schema %q: %v\n%s", dsn, err, out)
			}
			if got := string(out); got != want {
				t.Errorf("fleet_dsn_with_schema(%q, %s) = %q, want config.WithSchema's %q", dsn, schema, got, want)
			}
			if config.DSNSchema(want) != schema {
				t.Errorf("config.WithSchema(%q, %s) = %q names schema %q", dsn, schema, want, config.DSNSchema(want))
			}
		}
	}
}
