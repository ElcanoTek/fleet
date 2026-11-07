// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/release"
)

// drainSeenRestarter records whether the update drain marker was on disk when
// the service was (re)started.
type drainSeenRestarter struct {
	marker  string
	sawLive []bool
}

func (r *drainSeenRestarter) Restart(context.Context, string) error {
	_, err := os.Stat(r.marker)
	r.sawLive = append(r.sawLive, err == nil)
	return nil
}

// drainProber is ready only while the marker is absent — the server's /readyz
// answers 503 "draining" for as long as UpdateDrainActive() is true.
type drainProber struct{ marker string }

func (p drainProber) WaitReady(context.Context, string, time.Duration) error {
	if _, err := os.Stat(p.marker); err == nil {
		return errors.New(`HTTP 503 {"checks":{"draining":{"detail":"shutting_down","status":"error"}}}`)
	}
	return nil
}

// TestReleaseRollbackHandsOffTheUpdateDrain: `fleet release install` and
// `rollback` run inside update.sh's drain fence. The process they start reads
// the same drain marker, so they must remove the marker the update owns
// (FLEET_DRAIN_OWNER_TOKEN) — or one whose owner is gone — before the service
// is probed, or /readyz never answers 200 and the box rolls back. A marker that a
// different live process holds is left alone.
func TestReleaseRollbackHandsOffTheUpdateDrain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		content   func(token string) string
		wantClear bool
	}{
		{"owned by the update", func(tok string) string { return "draining " + tok + "\n" }, true},
		{"owner is gone", func(string) string { return "draining pid-2147483646-1\n" }, true},
		{"another live owner", func(string) string { return fmt.Sprintf("draining pid-%d-999\n", os.Getppid()) }, false},
		{"operator's own marker", func(string) string { return "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drainDir := t.TempDir()
			marker := filepath.Join(drainDir, "fleet.env.update.drain")
			token := fmt.Sprintf("pid-%d-424242", os.Getppid())
			if err := os.WriteFile(marker, []byte(tc.content(token)), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FLEET_UPDATE_DRAIN_FILE", marker)
			t.Setenv("FLEET_DRAIN_OWNER_TOKEN", token)

			installDir := t.TempDir()
			for name, body := range map[string]string{"fleet": "NEW", "fleet.prev": "OLD"} {
				if err := os.WriteFile(filepath.Join(installDir, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			rs := &drainSeenRestarter{marker: marker}
			oldR, oldP := installRestarter, newInstallProber
			installRestarter = rs
			newInstallProber = func(*tls.Config) release.Prober { return drainProber{marker: marker} }
			t.Cleanup(func() { installRestarter, newInstallProber = oldR, oldP })

			rc := releaseRollback(context.Background(), []string{"--install-dir", installDir, "--health-url", "http://127.0.0.1:1/readyz", "--health-timeout", "1s"})
			_, statErr := os.Stat(marker)
			cleared := errors.Is(statErr, os.ErrNotExist)
			if cleared != tc.wantClear {
				t.Fatalf("marker cleared=%v, want %v", cleared, tc.wantClear)
			}
			if !tc.wantClear {
				return
			}
			if rc != 0 {
				t.Fatalf("rollback rc=%d: the restarted service still saw the drain", rc)
			}
			// This Restarter cannot stop and start separately, so the marker
			// stays through its restart (fencing the old process) and comes
			// off before the probe — rc 0 above is the probe seeing it gone.
			if len(rs.sawLive) == 0 {
				t.Fatal("the service was never restarted")
			}
		})
	}
}

// TestDrainLockWaitIsBounded: a stalled holder of the drain sentinel's lock
// must not block the release install's handoff clear (and so the start that
// follows it) forever.
func TestDrainLockWaitIsBounded(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "fleet.env.update.drain")
	t.Setenv("FLEET_UPDATE_DRAIN_FILE", marker)
	if err := os.WriteFile(marker, []byte("draining pid-2147483646-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(marker+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	old := drainLockTimeout
	drainLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { drainLockTimeout = old })

	done := make(chan error, 1)
	go func() { done <- clearHandedOffDrain() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("clearHandedOffDrain succeeded while another process held the drain lock")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("clearHandedOffDrain blocked on a stalled drain-lock holder")
	}
}
