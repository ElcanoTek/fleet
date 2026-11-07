// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package release

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	Binary string // the verified new binary (ignored by Rollback)
	// Source, when set, is the verified binary held OPEN since it was hashed
	// (LoadVerified's BinaryFile): the install copies from this descriptor,
	// never re-opening Binary by name, and SHA256 must match the copy.
	Source        *os.File
	SHA256        string
	InstallDir    string
	Service       string
	HealthURL     string
	HealthTimeout time.Duration
	// NoRollback leaves a failed binary in place for debugging.
	NoRollback bool
	// NoRestart swaps the binary and stops there: no restart, no readiness
	// gate, no automatic rollback. For a box whose fleet is not a systemd
	// unit (a custom supervisor) — its operator restarts it, and rolls back
	// with `fleet release rollback --no-restart` if it does not come up.
	NoRestart bool
	// RollbackTimeout bounds the rollback's restart + readiness wait. The
	// rollback runs under its OWN context, derived from the caller's but not
	// cancelled with it: a SIGINT/SIGTERM that interrupted the health gate must
	// not also abort the restore. Default: HealthTimeout + 2 minutes.
	RollbackTimeout time.Duration
	Restarter       Restarter
	Prober          Prober
	Log             io.Writer
}

// InstallResult says what happened.
type InstallResult struct {
	Live       string
	Previous   string // empty when there was no binary to keep
	RolledBack bool
	// Unchanged: the live binary already was this exact binary, so nothing
	// was swapped and fleet.prev was left alone.
	Unchanged bool
	// Restarted: the service was restarted and passed its readiness gate.
	Restarted bool
}

// ErrRolledBack wraps a failed install that was rolled back successfully: the
// box is running the previous binary again.
var ErrRolledBack = errors.New("the new binary did not become ready; rolled back to the previous one")

// ErrBusy is returned when another install or rollback holds the install dir.
var ErrBusy = errors.New("another fleet install or rollback is running on this install dir")

// freeSpaceMargin is kept free on the install filesystem beyond the staged
// copy, so an install never fills the disk the service writes to.
const freeSpaceMargin = 64 << 20

func (o *InstallOptions) logf(format string, a ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", a...)
	}
}

// Install swaps the verified binary in and gates on readiness:
//
//  0. take the install dir's lock (one install or rollback at a time) and
//     check there is room for the staged copy;
//  1. if the live binary already IS this binary (a reinstall), swap nothing
//     and keep fleet.prev — it still names the release before this one;
//  2. copy it to <dir>/.fleet.new (same filesystem) and fsync it;
//  3. keep the running binary as <dir>/fleet.prev (a hard link, so it is the
//     exact inode that was running, not a copy that could differ);
//  4. rename .fleet.new over <dir>/fleet — atomic: the path names the old or
//     the new binary, never a partial one;
//  5. restart the service (which migrates on start) and wait for HealthURL;
//  6. on a failed restart or health gate, rename fleet.prev's bytes back,
//     restart, and re-check — under a fresh context, so an interrupted gate
//     still rolls back — and the box ends on a binary that answered ready, or
//     the error says plainly that neither did.
func Install(ctx context.Context, o InstallOptions) (InstallResult, error) {
	live := filepath.Join(o.InstallDir, liveName)
	prev := filepath.Join(o.InstallDir, prevName)
	res := InstallResult{Live: live}
	if err := os.MkdirAll(o.InstallDir, 0o755); err != nil { //nolint:gosec // G301: the install dir holds a world-executable binary.
		return res, err
	}
	unlock, err := lockDir(o.InstallDir)
	if err != nil {
		return res, err
	}
	defer unlock()

	same, err := o.sameAsLive(live)
	if err != nil {
		return res, err
	}
	if same {
		res.Unchanged = true
		if _, err := os.Stat(prev); err == nil {
			res.Previous = prev
		}
		o.logf("%s already is this binary — nothing to swap; %s left as it was", live, prev)
		if o.NoRestart {
			return res, nil
		}
		if err := restartAndProbe(ctx, o); err != nil {
			return res, fmt.Errorf("%w (the live binary already was this release, so there is nothing to roll back to here — see `fleet release rollback`)", err)
		}
		res.Restarted = true
		return res, nil
	}

	if err := checkFreeSpace(o.InstallDir, o.Binary); err != nil {
		return res, err
	}
	staging := filepath.Join(o.InstallDir, stagingName)
	if err := o.stage(staging); err != nil {
		_ = os.Remove(staging)
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
	if o.NoRestart {
		return res, nil
	}

	gateErr := restartAndProbe(ctx, o)
	if gateErr == nil {
		res.Restarted = true
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
	rbCtx, cancel := o.rollbackContext(ctx)
	defer cancel()
	_ = replaceLink(live, filepath.Join(o.InstallDir, failedName))
	if err := restorePrevious(o.InstallDir); err != nil {
		return res, fmt.Errorf("%w; and the rollback could not restore %s: %w", gateErr, prev, err)
	}
	if err := restartAndProbe(rbCtx, o); err != nil {
		return res, fmt.Errorf("%w; rolled back to %s, but it did not become ready either: %w", gateErr, prev, err)
	}
	res.RolledBack = true
	return res, fmt.Errorf("%w: %w", ErrRolledBack, gateErr)
}

// rollbackContext is the bounded context a rollback runs under: it keeps the
// caller's values but not its cancellation (context.WithoutCancel), because
// the usual reason the gate failed early IS that cancellation.
func (o *InstallOptions) rollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := o.RollbackTimeout
	if budget <= 0 {
		budget = o.HealthTimeout
		if budget <= 0 {
			budget = 90 * time.Second
		}
		budget += 2 * time.Minute
	}
	return context.WithTimeout(context.WithoutCancel(ctx), budget)
}

// Rollback puts fleet.prev back as the live binary on purpose (`fleet release
// rollback`), restarts and gates on readiness. The binary it replaces becomes
// the new fleet.prev, so a second rollback undoes the first. The restart runs
// under its own bounded context: interrupting the command after the swap must
// not leave the service unrestarted on a binary nobody checked.
func Rollback(ctx context.Context, o InstallOptions) error {
	live := filepath.Join(o.InstallDir, liveName)
	prev := filepath.Join(o.InstallDir, prevName)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary at %s to roll back to", prev)
	}
	unlock, err := lockDir(o.InstallDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkFreeSpace(o.InstallDir, prev); err != nil {
		return err
	}
	staging := filepath.Join(o.InstallDir, rollbackName)
	if err := copyFileSync(prev, staging); err != nil {
		_ = os.Remove(staging)
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
	if o.NoRestart {
		return nil
	}
	rbCtx, cancel := o.rollbackContext(ctx)
	defer cancel()
	return restartAndProbe(rbCtx, o)
}

// lockDir takes an exclusive, non-blocking flock on <dir>/.fleet.lock. Two
// installs racing on one dir would each keep the other's binary as fleet.prev.
func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".fleet.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: a fixed name in the install dir.
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrBusy
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// sameAsLive reports whether the binary to install has the same bytes as
// live: by the verified digest when there is one, else by hashing Binary.
func (o *InstallOptions) sameAsLive(live string) (bool, error) {
	if o.SHA256 == "" {
		return sameFile(o.Binary, live)
	}
	if _, err := os.Stat(live); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	h, err := fileSHA256(live)
	if err != nil {
		return false, err
	}
	return h == o.SHA256, nil
}

// stage writes the new binary to the install dir's staging name: from the
// open verified descriptor when there is one (hashing as it copies, so the
// bytes renamed into place are exactly the bytes that were verified), else
// from Binary by name.
func (o *InstallOptions) stage(staging string) error {
	if o.Source == nil {
		if err := copyFileSync(o.Binary, staging); err != nil {
			return err
		}
		if o.SHA256 != "" {
			got, err := fileSHA256(staging)
			if err != nil {
				return err
			}
			if got != o.SHA256 {
				_ = os.Remove(staging)
				return fmt.Errorf("the staged binary hashes to %s, not the verified %s", got, o.SHA256)
			}
		}
		return nil
	}
	_ = os.Remove(staging)
	if _, err := o.Source.Seek(0, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(staging, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755) //nolint:gosec // G302/G304: the install dir's executable.
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), o.Source)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && o.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != o.SHA256 {
		err = fmt.Errorf("the binary changed after it was verified (copy hashes to %s, verified %s) — refusing it", hex.EncodeToString(h.Sum(nil)), o.SHA256)
	}
	if err != nil {
		_ = os.Remove(staging)
		return err
	}
	return os.Chmod(staging, 0o755) //nolint:gosec // G302: an executable
}

// sameFile reports whether a and b exist and have identical bytes.
func sameFile(a, b string) (bool, error) {
	if _, err := os.Stat(b); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	ha, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

// checkFreeSpace refuses to stage src into dir unless its filesystem has room
// for the copy plus freeSpaceMargin. (fleet.prev is a hard link: free.)
var checkFreeSpace = func(dir, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("free space on %s: %w", dir, err)
	}
	free := st.Bavail * uint64(st.Bsize)
	need := uint64(fi.Size()) + freeSpaceMargin
	if free < need {
		return fmt.Errorf("%s has %d MiB free; staging the binary needs %d MiB — nothing was changed", dir, free>>20, need>>20)
	}
	return nil
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
	Client *http.Client
	// TLS verifies an https readiness URL (see ProbeTLSConfig). Nil uses the
	// default verification: the system roots and the URL's own host name.
	TLS      *tls.Config
	Interval time.Duration
}

// WaitReady implements Prober.
func (p HTTPProber) WaitReady(ctx context.Context, url string, timeout time.Duration) error {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
		if p.TLS != nil {
			client.Transport = &http.Transport{TLSClientConfig: p.TLS.Clone()}
		}
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
func HealthURLFromAddr(addr string) string { return HealthURL(addr, "") }

// HealthURL is HealthURLFromAddr for a chat listener that may terminate TLS
// itself (FLEET_TLS_MODE=manual|auto → https).
func HealthURL(addr, tlsMode string) string {
	scheme := "http"
	if m := strings.ToLower(strings.TrimSpace(tlsMode)); m == "manual" || m == "auto" {
		scheme = "https"
	}
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return scheme + "://" + addr + "/readyz"
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/readyz"
}

// loopbackHTTPS reports whether raw is an https URL to a loopback address.
func loopbackHTTPS(raw string) bool {
	u, err := neturl.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ErrProbeServerName is returned when an https readiness probe to a loopback
// address has no domain to verify the certificate against.
var ErrProbeServerName = errors.New("the readiness probe dials the chat listener on loopback over TLS, and its certificate names a domain this command does not know")

// ProbeTLSConfig is how a readiness probe verifies the deployment's own TLS
// listener — fully, never with verification switched off.
//
// The probe dials the listener on a loopback address, but the certificate
// names the public domain (FLEET_TLS_DOMAIN: the ACME domain in auto mode,
// the certificate's name in manual mode). So ServerName is set to that domain
// and the chain is verified against the system roots — which is all an ACME
// certificate needs — plus, in manual mode, the configured certificate file
// itself (FLEET_TLS_CERT_FILE), so a self-signed or internal-CA certificate
// verifies too.
//
// It returns nil for a plain-http URL, and for an https URL to a non-loopback
// host (that URL's own name is what the certificate is checked against). A
// loopback https URL with no serverName is ErrProbeServerName: set
// FLEET_TLS_DOMAIN, or name the domain explicitly.
func ProbeTLSConfig(url, mode, serverName, certFile string) (*tls.Config, error) {
	if !loopbackHTTPS(url) {
		return nil, nil
	}
	serverName = strings.TrimSpace(serverName)
	if serverName == "" {
		return nil, ErrProbeServerName
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if strings.EqualFold(strings.TrimSpace(mode), "manual") && certFile != "" {
		pemBytes, err := os.ReadFile(certFile) //nolint:gosec // G304: the operator's FLEET_TLS_CERT_FILE.
		if err != nil {
			return nil, fmt.Errorf("read FLEET_TLS_CERT_FILE %s: %w", certFile, err)
		}
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("FLEET_TLS_CERT_FILE %s holds no PEM certificate", certFile)
		}
	}
	return &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
}
