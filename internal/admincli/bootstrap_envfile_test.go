package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootstrapEnvFileRecordsReadsLikeTheService: the guard that decides
// whether a role/database was provisioned by an earlier run reads the env
// file as fleet does (fleet_env_value: last assignment, quotes stripped), so a
// quoted DATABASE_URL is still recognised (adversarial review, #2055).
func TestBootstrapEnvFileRecordsReadsLikeTheService(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	body := "DATABASE_URL=postgres://old:x@127.0.0.1:5432/old\nDATABASE_URL=\"postgres://fleet:placeholder-not-a-secret@127.0.0.1:5432/fleet?sslmode=disable\"\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src := bootstrapFunc(t, "env_file_records")
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "envfile.sh")
	run := func(role, db string) bool {
		return exec.Command("bash", "-c", `ENV_FILE="$1"; . "$5"; eval "$2"; env_file_records DATABASE_URL "$3" "$4"`, "_", envFile, src, role, db, lib).Run() == nil
	}
	if !run("fleet", "fleet") {
		t.Error("a quoted DATABASE_URL (the last assignment) is not recognised as recorded")
	}
	if run("old", "old") {
		t.Error("an earlier, overridden assignment counts as recorded")
	}
}

// TestDoctorFindsTheFleetBinaryForTheLayout: doctor.sh asks `fleet db
// layout` to tell a legacy pair from per-store keys naming one database; it
// must find the binary where bootstrap puts it, or on PATH, rather than
// assume one install directory (adversarial review, #2055).
func TestDoctorFindsTheFleetBinaryForTheLayout(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	at := strings.Index(src, `db layout --env-file`)
	if at < 0 {
		t.Fatal("doctor.sh no longer asks `fleet db layout`")
	}
	block := src[max(0, at-400):at]
	if !strings.Contains(block, `command -v fleet`) {
		t.Error("doctor.sh looks for the fleet binary in one directory only")
	}
}
