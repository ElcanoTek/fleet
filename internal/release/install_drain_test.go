// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// drainService models a systemd unit and the update drain marker: the running
// process reads the marker on every /readyz (httpapi.UpdateDrainActive), so it
// is ready only while it runs, its binary is in ready, and the marker is gone.
type drainService struct {
	dir, marker string
	running     bool
	ready       map[string]bool
	events      []string
	// startedDraining records, per start, whether the marker was on disk.
	startedDraining []bool
	onStop          func()
}

func (s *drainService) markerPresent() bool {
	_, err := os.Stat(s.marker)
	return err == nil
}

func (s *drainService) Stop(context.Context, string) error {
	s.events = append(s.events, "stop")
	s.running = false
	if s.onStop != nil {
		s.onStop()
	}
	return nil
}

func (s *drainService) Start(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "fleet"))
	s.events = append(s.events, "start "+string(b))
	s.startedDraining = append(s.startedDraining, s.markerPresent())
	s.running = true
	return nil
}

func (s *drainService) Restart(ctx context.Context, svc string) error {
	_ = s.Stop(ctx, svc)
	return s.Start(ctx, svc)
}

func (s *drainService) WaitReady(_ context.Context, _ string, _ time.Duration) error {
	b, _ := os.ReadFile(filepath.Join(s.dir, "fleet"))
	s.events = append(s.events, "probe "+string(b))
	if !s.running {
		return errors.New("connection refused")
	}
	if s.markerPresent() {
		return errors.New(`HTTP 503 {"checks":{"draining":{"detail":"shutting_down","status":"error"}},"status":"not_ready"}`)
	}
	if !s.ready[string(b)] {
		return errors.New("HTTP 503")
	}
	return nil
}

// clearDrain is the ClearDrain hook: it removes the marker, and fails the test
// if the old process is still running (the marker is what fences it).
func (s *drainService) clearDrain(t *testing.T) func() error {
	return func() error {
		if s.running {
			t.Error("the drain marker was cleared while the old process was still running — it could admit new work")
		}
		s.events = append(s.events, "clear")
		return os.Remove(s.marker)
	}
}

func drainSetup(t *testing.T) (dir, bin string, s *drainService) {
	t.Helper()
	dir, bin, _, _ = setup(t, "OLD")
	s = &drainService{dir: dir, marker: filepath.Join(t.TempDir(), "fleet.env.update.drain"), running: true, ready: map[string]bool{}}
	if err := os.WriteFile(s.marker, []byte("draining pid-1-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, bin, s
}

func noneTrue(bs []bool) bool {
	for _, b := range bs {
		if b {
			return false
		}
	}
	return true
}

// TestInstallClearsTheDrainBeforeTheNewProcessStarts is the v2026.11.07.21
// rollout bug: update.sh holds the drain marker through `release install`,
// the new process started draining, /readyz answered 503 for 90s and every
// non --force update rolled back. The install must stop the old process
// (still fenced), clear the marker, and only then start the new one.
func TestInstallClearsTheDrainBeforeTheNewProcessStarts(t *testing.T) {
	dir, bin, s := drainSetup(t)
	s.ready["NEW"] = true
	o := opts(dir, bin, s, s)
	o.ClearDrain = s.clearDrain(t)
	res, err := Install(context.Background(), o)
	if err != nil {
		t.Fatalf("install: %v (events %v)", err, s.events)
	}
	if !res.Restarted || res.RolledBack {
		t.Fatalf("result %+v", res)
	}
	if got, want := strings.Join(s.events, ","), "stop,clear,start NEW,probe NEW"; got != want {
		t.Errorf("events %q, want %q", got, want)
	}
	if !noneTrue(s.startedDraining) {
		t.Errorf("the new process started with the drain marker on disk: %v", s.startedDraining)
	}
}

// TestInstallRollbackStartsTheOldBinaryServing: when the new binary really is
// broken, the rollback must also bring the previous one back without the
// drain, so the box ends up serving rather than "did not become ready either".
func TestInstallRollbackStartsTheOldBinaryServing(t *testing.T) {
	dir, bin, s := drainSetup(t)
	s.ready["OLD"] = true
	o := opts(dir, bin, s, s)
	o.ClearDrain = func() error {
		s.events = append(s.events, "clear")
		if err := os.Remove(s.marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	_, err := Install(context.Background(), o)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("want ErrRolledBack, got %v (events %v)", err, s.events)
	}
	if got, want := strings.Join(s.events, ","), "stop,clear,start NEW,probe NEW,stop,clear,start OLD,probe OLD"; got != want {
		t.Errorf("events %q, want %q", got, want)
	}
	if !noneTrue(s.startedDraining) || !s.running {
		t.Errorf("drain at starts %v, running %v", s.startedDraining, s.running)
	}
}

// TestRollbackClearsTheDrainBeforeStarting: `fleet release rollback` run
// inside an update's drain fence has the same ordering requirement.
func TestRollbackClearsTheDrainBeforeStarting(t *testing.T) {
	dir, _, s := drainSetup(t)
	s.ready["OLD"] = true
	_ = os.WriteFile(filepath.Join(dir, "fleet.prev"), []byte("OLD"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "fleet"), []byte("NEW"), 0o755)
	o := opts(dir, "", s, s)
	o.ClearDrain = s.clearDrain(t)
	if err := Rollback(context.Background(), o); err != nil {
		t.Fatalf("rollback: %v (events %v)", err, s.events)
	}
	if got, want := strings.Join(s.events, ","), "stop,clear,start OLD,probe OLD"; got != want {
		t.Errorf("events %q, want %q", got, want)
	}
}

// TestInstallStartsEvenWhenInterruptedAfterTheStop: once the old process is
// stopped the start must be issued — a SIGINT that lands during the stop must
// not leave the service down — and the drain is still cleared first.
func TestInstallStartsEvenWhenInterruptedAfterTheStop(t *testing.T) {
	dir, bin, s := drainSetup(t)
	s.ready["NEW"], s.ready["OLD"] = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.onStop = cancel
	o := opts(dir, bin, s, s)
	o.ClearDrain = func() error {
		_ = os.Remove(s.marker)
		return nil
	}
	_, _ = Install(ctx, o)
	if !s.running {
		t.Fatalf("the service was left stopped (events %v)", s.events)
	}
	if !noneTrue(s.startedDraining) {
		t.Errorf("started with the drain marker on disk: %v", s.startedDraining)
	}
}

// TestInstallStillStartsWhenTheDrainCannotBeCleared: a marker the install may
// not remove is reported, never a reason to leave the service stopped.
func TestInstallStillStartsWhenTheDrainCannotBeCleared(t *testing.T) {
	dir, bin, s := drainSetup(t)
	s.ready["NEW"], s.ready["OLD"] = true, true
	var log bytes.Buffer
	o := opts(dir, bin, s, s)
	o.Log = &log
	o.ClearDrain = func() error { return errors.New("held by pid 42") }
	_, err := Install(context.Background(), o)
	if err == nil {
		t.Fatal("install passed its gate with the drain still on disk")
	}
	if !s.running {
		t.Fatalf("the service was left stopped (events %v)", s.events)
	}
	if !strings.Contains(log.String(), "held by pid 42") || !strings.Contains(err.Error(), "held by pid 42") {
		t.Errorf("the drain error is not reported: log %q, err %v", log.String(), err)
	}
}

// restartOnly is a Restarter that cannot stop and start separately.
type restartOnly struct{ s *drainService }

func (r restartOnly) Restart(ctx context.Context, svc string) error { return r.s.Restart(ctx, svc) }

// TestInstallClearsTheDrainBeforeARestartOnlyRestarter: without a separate
// stop, the marker is cleared immediately before the restart.
func TestInstallClearsTheDrainBeforeARestartOnlyRestarter(t *testing.T) {
	dir, bin, s := drainSetup(t)
	s.ready["NEW"] = true
	o := opts(dir, bin, restartOnly{s}, s)
	o.ClearDrain = func() error {
		s.events = append(s.events, "clear")
		return os.Remove(s.marker)
	}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatalf("install: %v (events %v)", err, s.events)
	}
	if got, want := strings.Join(s.events, ","), "clear,stop,start NEW,probe NEW"; got != want {
		t.Errorf("events %q, want %q", got, want)
	}
}

// TestInstallWithoutClearDrainKeepsRestart: callers that do not hand the
// drain over get the plain restart they always had.
func TestInstallWithoutClearDrainKeepsRestart(t *testing.T) {
	dir, bin, svc, p := setup(t, "OLD")
	p.ready["NEW"] = true
	if _, err := Install(context.Background(), opts(dir, bin, svc, p)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(svc.restarts, ",") != "NEW" {
		t.Errorf("restarts %v", svc.restarts)
	}
}

// interruptibleStop models `systemctl stop` run through exec.CommandContext:
// cancelling its context kills the systemctl client (Stop returns an error),
// but the stop job systemd already queued still runs to completion, so the
// old process exits either way.
type interruptibleStop struct {
	*drainService
	started chan struct{}
	stopErr error // returned even though the stop completed (a failing client)
}

func (s *interruptibleStop) Stop(ctx context.Context, _ string) error {
	s.events = append(s.events, "stop")
	if s.started != nil {
		close(s.started)
		s.started = nil
	}
	var err error
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-time.After(200 * time.Millisecond):
	}
	s.running = false // systemd finishes the queued stop regardless
	if err != nil {
		return err
	}
	return s.stopErr
}

// TestInstallStartsWhenInterruptedDuringTheStop: a SIGINT/SIGTERM that lands
// while `systemctl stop` is still waiting must not abort the stop halfway and
// skip the start — systemd completes the queued stop and fleet stays down.
// The stop runs to completion, the new binary starts and passes its gate,
// and nothing is rolled back. The reinstall case (live already is this
// binary) has no rollback to fall back on, so there a skipped start would
// leave the box down.
func TestInstallStartsWhenInterruptedDuringTheStop(t *testing.T) {
	for _, reinstall := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "reinstall"}[reinstall], func(t *testing.T) {
			dir, bin, ds := drainSetup(t)
			if reinstall {
				_ = os.WriteFile(filepath.Join(dir, "fleet"), []byte("NEW"), 0o755)
			}
			ds.ready["NEW"], ds.ready["OLD"] = true, true
			started := make(chan struct{})
			s := &interruptibleStop{drainService: ds, started: started}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-started
				cancel()
			}()
			o := opts(dir, bin, s, s)
			o.ClearDrain = func() error {
				_ = os.Remove(ds.marker)
				return nil
			}
			res, err := Install(ctx, o)
			if !ds.running {
				t.Fatalf("the service was left stopped after an interrupt during the stop (err %v, events %v)", err, ds.events)
			}
			if err != nil || res.RolledBack {
				t.Fatalf("install: %v (rolled back %v, events %v) — the interrupted stop should still complete and the new binary start", err, res.RolledBack, ds.events)
			}
			if got, want := strings.Join(ds.events, ","), "stop,start NEW,probe NEW"; got != want {
				t.Errorf("events %q, want %q", got, want)
			}
			if !noneTrue(ds.startedDraining) {
				t.Errorf("started under the drain marker: %v", ds.startedDraining)
			}
		})
	}
}

// TestInstallStartsWhenTheStopFails: whatever `systemctl stop` reports, the
// drain is cleared and the service is started, and the stop error is reported.
func TestInstallStartsWhenTheStopFails(t *testing.T) {
	dir, bin, ds := drainSetup(t)
	ds.ready["NEW"] = true
	s := &interruptibleStop{drainService: ds, stopErr: errors.New("Job for fleet.service canceled")}
	var log bytes.Buffer
	o := opts(dir, bin, s, s)
	o.Log = &log
	o.ClearDrain = func() error {
		_ = os.Remove(ds.marker)
		return nil
	}
	_, err := Install(context.Background(), o)
	if !ds.running || len(ds.startedDraining) == 0 || !noneTrue(ds.startedDraining) {
		t.Fatalf("running %v, starts %v (events %v): want started without the drain", ds.running, ds.startedDraining, ds.events)
	}
	if err != nil {
		t.Fatalf("install: %v — the new binary came up ready; a stop error alone must not roll it back", err)
	}
	if !strings.Contains(log.String(), "Job for fleet.service canceled") {
		t.Errorf("the stop error is not reported: log %q", log.String())
	}
}
