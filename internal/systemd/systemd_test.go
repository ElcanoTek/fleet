package systemd

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSystemctl puts a shell-script systemctl first on PATH. It records its
// argv to a file and answers like the real one for the verbs probed here.
func fakeSystemctl(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	body := "#!/bin/sh\necho \"$@\" > " + argv + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv
}

func readArgv(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstalled(t *testing.T) {
	argv := fakeSystemctl(t, `[ "$2" = "fleet.service" ] && exit 0; exit 1`)
	if !Installed(context.Background(), "fleet.service") {
		t.Error("Installed(fleet.service) = false, want true")
	}
	if got := readArgv(t, argv); got != "cat fleet.service\n" {
		t.Errorf("argv = %q, want cat fleet.service", got)
	}
	if Installed(context.Background(), "nope.service") {
		t.Error("Installed(nope.service) = true, want false")
	}
}

// is-active exits 3 for every non-active state but still prints it.
func TestActiveStateIgnoresExitStatus(t *testing.T) {
	argv := fakeSystemctl(t, `printf '  failed \n'; exit 3`)
	if got := ActiveState(context.Background(), "fleet.service"); got != "failed" {
		t.Errorf("ActiveState = %q, want failed", got)
	}
	if got := readArgv(t, argv); got != "is-active fleet.service\n" {
		t.Errorf("argv = %q", got)
	}
}

func TestShowProp(t *testing.T) {
	argv := fakeSystemctl(t, `echo " 3 "`)
	v, err := ShowProp(context.Background(), "fleet.service", "NRestarts")
	if err != nil || v != "3" {
		t.Errorf("ShowProp = (%q, %v), want (3, nil)", v, err)
	}
	if got := readArgv(t, argv); got != "show -p NRestarts --value fleet.service\n" {
		t.Errorf("argv = %q", got)
	}

	fakeSystemctl(t, `echo partial; exit 1`)
	v, err = ShowProp(context.Background(), "fleet.service", "Result")
	if err == nil || v != "partial" {
		t.Errorf("ShowProp on failure = (%q, %v), want (partial, non-nil)", v, err)
	}
}

// A caller's tighter deadline wins over ProbeTimeout.
func TestProbeHonoursCallerDeadline(t *testing.T) {
	fakeSystemctl(t, `exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if Installed(ctx, "fleet.service") {
		t.Error("a killed probe must not read as installed")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("probe took %v, want it bounded by the caller's 200ms", d)
	}
}
