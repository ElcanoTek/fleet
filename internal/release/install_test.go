// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeService records restarts and reports which binary was live at each.
type fakeService struct {
	dir      string
	restarts []string // contents of <dir>/fleet at each restart
	fail     map[string]error
}

func (f *fakeService) Restart(_ context.Context, _ string) error {
	b, _ := os.ReadFile(filepath.Join(f.dir, "fleet"))
	f.restarts = append(f.restarts, string(b))
	return f.fail[string(b)]
}

// fakeProber is ready only when the live binary's bytes are in ready.
type fakeProber struct {
	dir   string
	ready map[string]bool
}

func (p *fakeProber) WaitReady(_ context.Context, _ string, _ time.Duration) error {
	b, _ := os.ReadFile(filepath.Join(p.dir, "fleet"))
	if p.ready[string(b)] {
		return nil
	}
	return errors.New("503")
}

func setup(t *testing.T, live string) (dir, newBin string, svc *fakeService, prober *fakeProber) {
	t.Helper()
	dir = t.TempDir()
	if live != "" {
		if err := os.WriteFile(filepath.Join(dir, "fleet"), []byte(live), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	newBin = filepath.Join(t.TempDir(), "fleet-linux-amd64")
	_ = os.WriteFile(newBin, []byte("NEW"), 0o755)
	svc = &fakeService{dir: dir, fail: map[string]error{}}
	prober = &fakeProber{dir: dir, ready: map[string]bool{}}
	return
}

func opts(dir, bin string, svc Restarter, p Prober) InstallOptions {
	return InstallOptions{Binary: bin, InstallDir: dir, Service: "fleet", HealthURL: "http://x/readyz", Restarter: svc, Prober: p}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInstallSwapsAndKeepsThePrevious(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["NEW"] = true
	res, err := Install(context.Background(), opts(dir, bin, svc, p))
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "NEW" || read(t, filepath.Join(dir, "fleet.prev")) != "OLD" {
		t.Fatal("live/prev not swapped")
	}
	if res.RolledBack || res.Previous == "" {
		t.Fatalf("result %+v", res)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "fleet")); fi.Mode().Perm() != 0o755 {
		t.Errorf("live mode %v", fi.Mode())
	}
	if strings.Join(svc.restarts, ",") != "NEW" {
		t.Errorf("restarts %v", svc.restarts)
	}
	if _, err := os.Stat(filepath.Join(dir, ".fleet.new")); !os.IsNotExist(err) {
		t.Error("staging file left behind")
	}
}

func TestInstallRollsBackWhenNotReady(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["OLD"] = true // the new binary never answers ready
	_, err := Install(context.Background(), opts(dir, bin, svc, p))
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" {
		t.Fatal("the previous binary is not live after the rollback")
	}
	if read(t, filepath.Join(dir, "fleet.failed")) != "NEW" {
		t.Error("the failed binary was not kept for debugging")
	}
	if read(t, filepath.Join(dir, "fleet.prev")) != "OLD" {
		t.Error("fleet.prev lost")
	}
	if strings.Join(svc.restarts, ",") != "NEW,OLD" {
		t.Errorf("restarts %v, want NEW then OLD", svc.restarts)
	}
}

func TestInstallRollsBackWhenRestartFails(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["OLD"], p.ready["NEW"] = true, true
	svc.fail["NEW"] = errors.New("start request repeated too quickly")
	_, err := Install(context.Background(), opts(dir, bin, svc, p))
	if !errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "repeated too quickly") {
		t.Fatalf("got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" {
		t.Fatal("not rolled back")
	}
}

func TestInstallReportsWhenTheRollbackFailsToo(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD") // nothing is ever ready (e.g. the DB is down)
	_, err := Install(context.Background(), opts(dir, bin, svc, p))
	if err == nil || errors.Is(err, ErrRolledBack) || !strings.Contains(err.Error(), "did not become ready either") {
		t.Fatalf("got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" {
		t.Fatal("the box should still be left on the previous binary")
	}
}

func TestInstallNoRollbackLeavesTheNewBinary(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	o := opts(dir, bin, svc, p)
	o.NoRollback = true
	_, err := Install(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "--no-rollback") {
		t.Fatalf("got %v", err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "NEW" {
		t.Fatal("--no-rollback rolled back")
	}
}

func TestInstallFreshBoxHasNothingToRollBackTo(t *testing.T) {
	dir, bin, svc, p := setup(t, "")
	_, err := Install(context.Background(), opts(dir, bin, svc, p))
	if err == nil || !strings.Contains(err.Error(), "no previous binary") {
		t.Fatalf("got %v", err)
	}
	p.ready["NEW"] = true
	res, err := Install(context.Background(), opts(dir, bin, svc, p))
	if err != nil {
		t.Fatal(err)
	}
	_ = res
}

func TestRollbackSwapsPrevBack(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["NEW"], p.ready["OLD"] = true, true
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(context.Background(), opts(dir, "", svc, p)); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "OLD" || read(t, filepath.Join(dir, "fleet.prev")) != "NEW" {
		t.Fatal("rollback did not swap live and prev")
	}
	// A second rollback undoes the first.
	if err := Rollback(context.Background(), opts(dir, "", svc, p)); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "fleet")) != "NEW" {
		t.Fatal("second rollback did not undo the first")
	}
	empty := t.TempDir()
	if err := Rollback(context.Background(), opts(empty, "", svc, p)); err == nil {
		t.Fatal("rollback without fleet.prev succeeded")
	}
}

func TestHTTPProber(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := HTTPProber{Interval: 10 * time.Millisecond}
	if err := p.WaitReady(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer never.Close()
	// A deadline long enough for a loaded CI runner to complete at least one
	// request; 100ms was not (the probe's only request was cut off).
	if err := p.WaitReady(context.Background(), never.URL, time.Second); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("got %v", err)
	}
}

// TestHTTPProberKeepsTheLastRealAnswer — an attempt cut off by the overall
// deadline must not replace an earlier HTTP status with "context deadline
// exceeded": the operator needs to see what the server last said.
func TestHTTPProberKeepsTheLastRealAnswer(t *testing.T) {
	var n atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-r.Context().Done() // later attempts hang until the deadline cuts them off
	}))
	defer slow.Close()
	p := HTTPProber{Interval: 10 * time.Millisecond}
	err := p.WaitReady(context.Background(), slow.URL, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("want the last real answer (HTTP 503) in the error, got %v", err)
	}
}
