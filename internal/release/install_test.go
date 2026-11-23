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

// degraded207 is fleet's /readyz answer when only non-critical checks fail.
const degraded207 = `{"status":"degraded","checks":{"chat_db":{"status":"ok"},"sched_db":{"status":"ok"},` +
	`"sandbox":{"status":"error","detail":"podman: exec: \"podman\": executable file not found in $PATH"},` +
	`"disk":{"status":"degraded","detail":"3.1% free on /var/lib/fleet is below the 5% floor"}}}`

// readyzServer answers every request with code and body.
func readyzServer(t *testing.T, code int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/readyz"
}

// TestHTTPProberCountsA207AsReady — /readyz answers 207 when only a
// non-critical check (sandbox, disk) fails: the process serves, so it is
// ready, and the degraded checks are printed as a warning. 503 is still not
// ready.
func TestHTTPProberCountsA207AsReady(t *testing.T) {
	var log strings.Builder
	p := HTTPProber{Interval: 10 * time.Millisecond, Log: &log}
	if err := p.WaitReady(context.Background(), readyzServer(t, http.StatusMultiStatus, degraded207), time.Second); err != nil {
		t.Fatalf("a 207 was not ready: %v", err)
	}
	got := log.String()
	for _, want := range []string{"warning:", "207", "disk: degraded (3.1% free", "sandbox: error (podman:"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "chat_db") {
		t.Errorf("warning names a check that is ok: %q", got)
	}
	if strings.Index(got, "disk:") > strings.Index(got, "sandbox:") {
		t.Errorf("degraded checks not in name order: %q", got)
	}

	notReady := `{"status":"not_ready","checks":{"chat_db":{"status":"error","detail":"connection refused"}}}`
	if err := p.WaitReady(context.Background(), readyzServer(t, http.StatusServiceUnavailable, notReady), 300*time.Millisecond); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("a 503 was ready, or the error does not say so: %v", err)
	}
}

// TestDegradedChecksPrintsOnlyPrintableText — the 207 warning quotes the
// server's check names and details; an escape sequence (here, clear screen)
// or a bidirectional override (U+202E) in them must not reach the terminal.
func TestDegradedChecksPrintsOnlyPrintableText(t *testing.T) {
	body := `{"status":"degraded","checks":{"disk\u202e":{"status":"degraded","detail":"\u001b[2Jfull\u202e\u0000 disk"}}}`
	got := degradedChecks([]byte(body))
	if strings.ContainsAny(got, "\x1b\u202e\x00") {
		t.Errorf("warning %q carries a control or format character", got)
	}
	if !strings.Contains(got, "[2Jfull") || !strings.Contains(got, "disk") {
		t.Errorf("warning %q lost the printable text", got)
	}
}

// TestInstallKeepsAReleaseThatAnswers207 — end to end with the real prober: a
// new binary whose /readyz answers 207 stays installed (no rollback), and one
// whose /readyz answers 503 is rolled back.
func TestInstallKeepsAReleaseThatAnswers207(t *testing.T) {
	for _, tc := range []struct {
		code         int
		body         string
		wantRollback bool
	}{
		{http.StatusMultiStatus, degraded207, false},
		{http.StatusServiceUnavailable, `{"status":"not_ready","checks":{"draining":{"status":"error","detail":"shutting_down"}}}`, true},
	} {
		dir, bin, svc, _ := setup(t, "OLD")
		var log strings.Builder
		o := opts(dir, bin, svc, HTTPProber{Interval: 10 * time.Millisecond, Log: &log})
		o.HealthURL = readyzServer(t, tc.code, tc.body)
		o.HealthTimeout = 300 * time.Millisecond
		o.RollbackTimeout = 300 * time.Millisecond
		o.Log = &log
		_, err := Install(context.Background(), o)
		live := read(t, filepath.Join(dir, "fleet"))
		if tc.wantRollback {
			// The fake old binary answers the same 503, so the rollback's
			// own wait fails too; what matters is that the old binary is
			// back in place.
			if err == nil || live != "OLD" {
				t.Errorf("HTTP %d: err %v, live %q; want the install rolled back to OLD", tc.code, err, live)
			}
			continue
		}
		if err != nil || live != "NEW" {
			t.Errorf("HTTP %d: err %v, live %q; want NEW kept", tc.code, err, live)
		}
		if !strings.Contains(log.String(), "answered 207") {
			t.Errorf("HTTP %d: install log lacks the 207 warning:\n%s", tc.code, log.String())
		}
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
