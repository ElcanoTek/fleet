// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// File names inside the install dir. The live binary is always <dir>/fleet so
// the unit's ExecStart never changes; the others sit beside it on the same
// filesystem, which is what makes every swap a rename(2).
const (
	liveName     = "fleet"
	prevName     = "fleet.prev"   // the binary that ran before the last install
	failedName   = "fleet.failed" // a binary that was rolled back, kept to debug
	stagingName  = ".fleet.new"
	rollbackName = ".fleet.rollback"
)

// Restarter restarts the service. Under the shipped Type=notify unit
// `systemctl restart` returns once the new process signalled READY=1, and the
// process runs its database migrations before that (services self-migrate on
// start; the updater never runs migrations itself).
type Restarter interface {
	Restart(ctx context.Context, service string) error
}

// Prober waits for the service to answer ready.
type Prober interface {
	WaitReady(ctx context.Context, url string, timeout time.Duration) error
}

// InstallOptions configures an install or a rollback.
type InstallOptions struct {
	Binary        string // the verified new binary (ignored by Rollback)
	InstallDir    string
	Service       string
	HealthURL     string
	HealthTimeout time.Duration
	// NoRollback leaves a failed binary in place for debugging.
	NoRollback bool
	Restarter  Restarter
	Prober     Prober
	Log        io.Writer
}

// InstallResult says what happened.
type InstallResult struct {
	Live       string
	Previous   string // empty when there was no binary to keep
	RolledBack bool
}

// ErrRolledBack wraps a failed install that was rolled back successfully: the
// box is running the previous binary again.
var ErrRolledBack = errors.New("the new binary did not become ready; rolled back to the previous one")

func (o *InstallOptions) logf(format string, a ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", a...)
	}
}

// Install swaps the verified binary in and gates on readiness:
//
//  1. copy it to <dir>/.fleet.new (same filesystem) and fsync it;
//  2. keep the running binary as <dir>/fleet.prev (a hard link, so it is the
//     exact inode that was running, not a copy that could differ);
//  3. rename .fleet.new over <dir>/fleet — atomic: the path names the old or
//     the new binary, never a partial one;
//  4. restart the service (which migrates on start) and wait for HealthURL;
//  5. on a failed restart or health gate, rename fleet.prev's bytes back,
//     restart, and re-check — the box ends on a binary that answered ready,
//     or the error says plainly that neither did.
func Install(ctx context.Context, o InstallOptions) (InstallResult, error) {
	live := filepath.Join(o.InstallDir, liveName)
	prev := filepath.Join(o.InstallDir, prevName)
	res := InstallResult{Live: live}
	if err := os.MkdirAll(o.InstallDir, 0o755); err != nil { //nolint:gosec // G301: the install dir holds a world-executable binary.
		return res, err
	}
	staging := filepath.Join(o.InstallDir, stagingName)
	if err := copyFileSync(o.Binary, staging); err != nil {
		return res, fmt.Errorf("stage the new binary: %w", err)
	}
	if _, err := os.Stat(live); err == nil {
		if err := replaceLink(live, prev); err != nil {
			_ = os.Remove(staging)
			return res, fmt.Errorf("keep the previous binary: %w", err)
		}
		res.Previous = prev
	}
	if err := os.Rename(staging, live); err != nil {
		_ = os.Remove(staging)
		return res, fmt.Errorf("swap in the new binary: %w", err)
	}
	syncDir(o.InstallDir)
	o.logf("installed %s (previous kept as %s)", live, prev)

	gateErr := restartAndProbe(ctx, o)
	if gateErr == nil {
		return res, nil
	}
	if o.NoRollback || res.Previous == "" {
		why := "--no-rollback"
		if res.Previous == "" {
			why = "there was no previous binary to restore"
		}
		return res, fmt.Errorf("%w (not rolled back: %s)", gateErr, why)
	}
	o.logf("the new binary failed its gate (%v) — rolling back to %s", gateErr, prev)
	_ = replaceLink(live, filepath.Join(o.InstallDir, failedName))
	if err := restorePrevious(o.InstallDir); err != nil {
		return res, fmt.Errorf("%w; and the rollback could not restore %s: %w", gateErr, prev, err)
	}
	if err := restartAndProbe(ctx, o); err != nil {
		return res, fmt.Errorf("%w; rolled back to %s, but it did not become ready either: %w", gateErr, prev, err)
	}
	res.RolledBack = true
	return res, fmt.Errorf("%w: %w", ErrRolledBack, gateErr)
}

// Rollback puts fleet.prev back as the live binary on purpose (`fleet release
// rollback`), restarts and gates on readiness. The binary it replaces becomes
// the new fleet.prev, so a second rollback undoes the first.
func Rollback(ctx context.Context, o InstallOptions) error {
	live := filepath.Join(o.InstallDir, liveName)
	prev := filepath.Join(o.InstallDir, prevName)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary at %s to roll back to", prev)
	}
	staging := filepath.Join(o.InstallDir, rollbackName)
	if err := copyFileSync(prev, staging); err != nil {
		return err
	}
	if _, err := os.Stat(live); err == nil {
		if err := replaceLink(live, prev); err != nil {
			_ = os.Remove(staging)
			return err
		}
	}
	if err := os.Rename(staging, live); err != nil {
		_ = os.Remove(staging)
		return err
	}
	syncDir(o.InstallDir)
	return restartAndProbe(ctx, o)
}

func restorePrevious(dir string) error {
	staging := filepath.Join(dir, rollbackName)
	if err := copyFileSync(filepath.Join(dir, prevName), staging); err != nil {
		return err
	}
	if err := os.Rename(staging, filepath.Join(dir, liveName)); err != nil {
		_ = os.Remove(staging)
		return err
	}
	syncDir(dir)
	return nil
}

func restartAndProbe(ctx context.Context, o InstallOptions) error {
	if err := o.Restarter.Restart(ctx, o.Service); err != nil {
		return fmt.Errorf("restart %s: %w", o.Service, err)
	}
	if o.HealthURL == "" {
		return nil
	}
	timeout := o.HealthTimeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if err := o.Prober.WaitReady(ctx, o.HealthURL, timeout); err != nil {
		return fmt.Errorf("%s not ready: %w", o.HealthURL, err)
	}
	return nil
}

// replaceLink makes dst a hard link to src, replacing dst. A filesystem that
// cannot link falls back to a synced copy.
func replaceLink(src, dst string) error {
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Link(src, tmp); err != nil {
		if err := copyFileSync(src, tmp); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// copyFileSync copies src to dst as an executable (0755) and fsyncs it.
func copyFileSync(src, dst string) error {
	const mode os.FileMode = 0o755
	in, err := os.Open(src) //nolint:gosec // G304: the verified binary / our own install-dir files.
	if err != nil {
		return err
	}
	defer in.Close()
	_ = os.Remove(dst)
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // G304: install-dir file we name.
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: the install dir.
		_ = d.Sync()
		_ = d.Close()
	}
}

// Systemctl restarts units with systemctl.
type Systemctl struct{}

// Restart implements Restarter.
func (Systemctl) Restart(ctx context.Context, service string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", service).CombinedOutput() //nolint:gosec // G204: fixed binary; the unit name is operator config.
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// HTTPProber polls a readiness URL until it answers 200.
type HTTPProber struct {
	Client   *http.Client
	Interval time.Duration
}

// WaitReady implements Prober.
func (p HTTPProber) WaitReady(ctx context.Context, url string, timeout time.Duration) error {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	interval := p.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req) // #nosec G704 — the operator-configured loopback readiness URL
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not ready after %s (last: %w)", timeout, last)
		case <-time.After(interval):
		}
	}
}

// HealthURLFromAddr derives the readiness URL from FLEET_SERVER_ADDR the way
// the server binds it: the chat listener serves /readyz (default
// 127.0.0.1:8080), and an all-interfaces bind is probed on loopback.
func HealthURLFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/readyz"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/readyz"
}
