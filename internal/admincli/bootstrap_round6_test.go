package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestShellDSNHelpersReadDSNsAsPgx (#2055 review round 6): the shell helpers
// that read a DSN's role, database, host or password agree with pgx — a
// query parameter overrides the URL's own part, the userinfo ends at the
// first "@", and "#" is not a fragment — or refuse the DSN rather than read
// it differently.
func TestShellDSNHelpersReadDSNsAsPgx(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRootFromTest(t)
	pgp := filepath.Join(root, "scripts", "lib", "pgprovision.sh")
	for dsn, ok := range map[string]bool{
		"postgres://app:x@h/appdb?sslmode=require": true,
		"postgres://app:x@h/appdb?user=other":      false,
		"postgres://app:x@h/appdb?dbname=other":    false,
		"postgres://app:x@h/appdb?database=other":  false,
		"postgres://app:x@h/appdb?%75ser=other":    false,
		"postgres://app:x@h/appdb?service=s":       false,
		"postgres://app:x@h/appdb#frag":            false,
		"postgres://app:x/y@h/appdb":               false, // pgx: no userinfo before a '/'
		"postgres://app:x@h@i/appdb":               false,
	} {
		if err := exec.Command("bash", "-c", `. "$1"; fleet_dsn_role_db "$2"`, "_", pgp, dsn).Run(); (err == nil) != ok {
			t.Errorf("fleet_dsn_role_db(%q) ok=%v, want %v", dsn, err == nil, ok)
		}
	}
	for dsn, want := range map[string]bool{
		"postgres://u:p@127.0.0.1/db":           true,
		"postgres://u@db.example@127.0.0.1/db":  false, // pgx: user "u", host "db.example@127.0.0.1"
		"postgres://u@127.0.0.1/db#x":           false,
		"postgres://u@127.0.0.1,127.0.0.2/db":   false,
		"postgres://u@127.0.0.1/db?host=remote": false,
	} {
		if err := exec.Command("bash", "-c", `. "$1"; fleet_dsn_is_local_default "$2"`, "_", pgp, dsn).Run(); (err == nil) != want {
			t.Errorf("fleet_dsn_is_local_default(%q) = %v, want %v", dsn, err == nil, want)
		}
	}

	// env_dsn_password: the password pgx sends — a ?password= override wins.
	idle := filepath.Join(root, "scripts", "lib", "idle.sh")
	envlib := filepath.Join(root, "scripts", "lib", "envfile.sh")
	fn := bootstrapFunc(t, "env_dsn_password")
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	for _, c := range [][2]string{
		{"postgres://u:placeholder-a@h/db", "placeholder-a"},
		{"postgres://u:placeholder-a@h/db?password=placeholder-b", "placeholder-b"},
		{"postgres://u@h/db?password=placeholder-c&sslmode=disable", "placeholder-c"},
		{"postgres://u:placeholder%2Fd@h/db", "placeholder/d"},
		{"postgres://u:placeholder-a@h/db?password=x&password=last-one", "last-one"},
		{"postgres://u@h/db", ""},
	} {
		dsn, want := c[0], c[1]
		if err := os.WriteFile(envFile, []byte("DATABASE_URL="+dsn+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("bash", "-c", `. "$1"; . "$2"; ENV_FILE="$3"; eval "$4"; env_dsn_password DATABASE_URL`, "_", envlib, idle, envFile, fn).CombinedOutput()
		if err != nil || string(out) != want {
			t.Errorf("env_dsn_password(%q) = %q (%v), want %q", dsn, out, err, want)
		}
	}
}
