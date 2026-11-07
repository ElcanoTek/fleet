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

// TestDoctorRefusesRetiredDSNKeys: doctor.sh fails a box whose env file
// still sets a per-store DSN key of the removed two-database layout (#2049
// phase 2) — fleet refuses to boot with one — and names DATABASE_URL.
func TestDoctorRefusesRetiredDSNKeys(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootFromTest(t), "scripts", "doctor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	at := strings.Index(src, `for _k in FLEET_CHAT_DATABASE_URL FLEET_SCHED_DATABASE_URL SCHED_DATABASE_URL; do`)
	if at < 0 {
		t.Fatal("doctor.sh no longer checks every retired per-store DSN key")
	}
	block := src[at:min(len(src), at+600)]
	if !strings.Contains(block, `fail "$_retired in $ENV_FILE: no longer supported`) || !strings.Contains(block, "set DATABASE_URL") {
		t.Errorf("doctor.sh does not fail a retired per-store key naming DATABASE_URL:\n%s", block)
	}
	if strings.Contains(src, "db layout") || strings.Contains(src, "db consolidate") {
		t.Error("doctor.sh still calls the removed `fleet db` verbs")
	}
}
