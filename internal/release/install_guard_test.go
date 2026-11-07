// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ctxService is a fakeService whose restart honours its context, the way
// `systemctl restart` under exec.CommandContext does.
type ctxService struct{ fakeService }

func (c *ctxService) Restart(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		b, _ := os.ReadFile(filepath.Join(c.dir, "fleet"))
		c.restarts = append(c.restarts, string(b)+"(cancelled)")
		return err
	}
	return c.fakeService.Restart(ctx, name)
}

func TestInstallRollsBackEvenWhenInterrupted(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["OLD"], p.ready["NEW"] = true, true
	cs := &ctxService{fakeService: *svc}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGINT arrived while the new binary was being gated
	_, err := Install(ctx, opts(dir, bin, cs, p))
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want a completed rollback despite the cancelled context, got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" {
		t.Fatal("the previous binary is not live")
	}
	if strings.Join(cs.restarts, ",") != "NEW(cancelled),OLD" {
		t.Errorf("restarts %v: the rollback restart must run under a fresh context", cs.restarts)
	}
}

func TestRollbackRestartsEvenWhenInterrupted(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["OLD"], p.ready["NEW"] = true, true
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err != nil {
		t.Fatal(err)
	}
	cs := &ctxService{fakeService: fakeService{dir: dir, fail: map[string]error{}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Rollback(ctx, opts(dir, "", cs, p)); err != nil {
		t.Fatalf("rollback under a cancelled context: %v", err)
	}
	if strings.Join(cs.restarts, ",") != "OLD" {
		t.Errorf("restarts %v, want the restored binary restarted", cs.restarts)
	}
}

func TestReinstallingTheLiveBinaryKeepsPrev(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["NEW"] = true
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err != nil {
		t.Fatal(err)
	}
	res, err := Install(context.Background(), opts(dir, bin, svc, p)) // the same release again
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unchanged || !res.Restarted {
		t.Errorf("result %+v, want Unchanged and Restarted", res)
	}
	if read(t, filepath.Join(dir, "fleet.prev")) != "OLD" {
		t.Fatal("a reinstall overwrote fleet.prev with the live binary — rollback would lead nowhere")
	}
	if read(t, filepath.Join(dir, "fleet")) != "NEW" {
		t.Fatal("live binary changed")
	}
}

func TestInstallNoRestartSwapsOnly(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	o := opts(dir, bin, svc, p)
	o.NoRestart = true
	res, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Restarted || len(svc.restarts) != 0 {
		t.Errorf("--no-restart restarted the service: %v", svc.restarts)
	}
	if read(t, filepath.Join(dir, "fleet")) != "NEW" || read(t, filepath.Join(dir, "fleet.prev")) != "OLD" {
		t.Fatal("--no-restart must still swap atomically and keep fleet.prev")
	}
	if err := Rollback(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" || len(svc.restarts) != 0 {
		t.Fatal("rollback --no-restart must swap back without restarting")
	}
}

func TestInstallIsExclusive(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	unlock, err := lockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy while another install holds the dir, got %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "fleet.prev"), []byte("PREV"), 0o755)
	if err := Rollback(context.Background(), opts(dir, bin, svc, p)); !errors.Is(err, ErrBusy) {
		t.Fatalf("rollback: want ErrBusy, got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" {
		t.Fatal("a refused install changed the live binary")
	}
}

func TestInstallRefusesWithoutSpace(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	saved := checkFreeSpace
	checkFreeSpace = func(string, string) error { return errors.New("1 MiB free") }
	defer func() { checkFreeSpace = saved }()
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err == nil {
		t.Fatal("installed with no space")
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" || len(svc.restarts) != 0 {
		t.Fatal("a refused install touched the box")
	}
	if _, err := os.Stat(filepath.Join(dir, "fleet.prev")); !os.IsNotExist(err) {
		t.Fatal("fleet.prev written by a refused install")
	}
}

func TestCheckFreeSpaceOnARealFilesystem(t *testing.T) {
	dir, bin, _, _ := setup(t, "OLD")
	if err := checkFreeSpace(dir, bin); err != nil {
		t.Fatalf("a tiny binary on a test filesystem must fit: %v", err)
	}
}

func TestHealthURLFollowsTLSMode(t *testing.T) {
	for _, tc := range []struct{ addr, mode, want string }{
		{":8443", "manual", "https://127.0.0.1:8443/readyz"},
		{"0.0.0.0:443", "auto", "https://127.0.0.1:443/readyz"},
		{"127.0.0.1:8080", "off", "http://127.0.0.1:8080/readyz"},
		{"[::1]:9000", "", "http://[::1]:9000/readyz"},
	} {
		if got := HealthURL(tc.addr, tc.mode); got != tc.want {
			t.Errorf("HealthURL(%q,%q)=%q want %q", tc.addr, tc.mode, got, tc.want)
		}
	}
	for u, want := range map[string]bool{
		"https://127.0.0.1:8443/readyz": true, "https://[::1]/readyz": true, "https://localhost/readyz": true,
		"https://10.0.0.5/readyz": false, "http://127.0.0.1/readyz": false, "https://fleet.example.com/readyz": false,
	} {
		if loopbackHTTPS(u) != want {
			t.Errorf("loopbackHTTPS(%q) != %v", u, want)
		}
	}
}
