// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/migrations"
	"github.com/ElcanoTek/fleet/internal/release"
	"github.com/ElcanoTek/fleet/internal/version"
)

// cmdRelease is `fleet release` — the signed-artifact half of `fleet update`
// (docs/RELEASE-ARTIFACTS.md). scripts/update.sh calls these steps in order;
// they are verbs rather than one opaque call so each runs at the point of the
// update where it belongs (the sandbox image before the install, the install
// inside the drain fence) and so an operator can run any of them by hand:
//
//	fleet release latest                    newest published release tag
//	fleet release fetch    --tag T --dir D  download + verify into D
//	fleet release sandbox  --tag T --dir D  verify the release's sandbox image, print its digest ref
//	fleet release install  --tag T --dir D  swap the verified binary in, restart, gate, roll back on failure
//	fleet release rollback                  put fleet.prev back, restart, gate
//
// Nothing here has an unverified mode. A missing signature, a signer other than
// fleet's release workflow, a checksum mismatch, a binary for another
// architecture or (without --allow-downgrade) an older release all stop the
// update before the binary is used.
func cmdRelease(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, releaseUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	switch argv[0] {
	case "latest":
		return releaseLatest(ctx, argv[1:])
	case "fetch":
		return releaseFetch(ctx, argv[1:])
	case "sandbox":
		return releaseSandbox(ctx, argv[1:])
	case "install":
		return releaseInstall(ctx, argv[1:])
	case "rollback":
		return releaseRollback(ctx, argv[1:])
	case "-h", "--help", "help":
		fmt.Print(releaseUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "fleet release: unknown subcommand %q\n\n%s", argv[0], releaseUsage)
		return 2
	}
}

const releaseUsage = `fleet release — signed release artifacts (docs/RELEASE-ARTIFACTS.md)

  fleet release latest
      print the newest published release tag
  fleet release fetch --tag <vYYYY.MM.DD.N> --dir <dir> [--allow-downgrade] [--commit <sha>]
      download this box's binary, SHA256SUMS and release.json into <dir> and
      verify them (signature by fleet's release workflow, checksums, tag,
      architecture, no host executor); prints the verified binary's path
  fleet release sandbox --tag <tag> --dir <dir>
      verify the release's sandbox image signature; prints its digest reference
  fleet release install --tag <tag> --dir <dir> [--install-dir /opt/fleet] [--service fleet]
                        [--health-url <url> | --server-addr <host:port>] [--health-timeout 90s] [--no-rollback] [--no-restart]
      atomically swap the verified binary in (the old one is kept as fleet.prev),
      restart the service (it migrates on start), wait for /readyz, and roll back
      automatically when it does not become ready — first undoing, with this
      binary's down migrations, any database migration it applied, so
      fleet.prev can run on the database. Run it with the NEW (staged,
      verified) binary, as fleet update does: an older binary does not ship
      the new migrations' down migrations, and its rollback then keeps the new
      binary. Reinstalling the live binary swaps nothing. --no-restart swaps
      only (fleet not run by systemd).
  fleet release rollback [--install-dir /opt/fleet] [--service fleet] [--health-url <url> | --server-addr <addr>] [--no-restart] [--migrate-down]
      put fleet.prev back as the live binary, restart and wait for /readyz.
      When the database was migrated past the version fleet.prev ran on, it
      refuses unless --migrate-down, which undoes those migrations first
      (dropping what was stored in the tables and columns they added: take
      a backup with fleet backup --db=all before)

Exit codes of install and rollback: 0 done; 2 usage; 4 install rolled back
(the previous binary is live again); 5 failed (read the message); 6 another
install or rollback is running; 7 install NOT rolled back, the new binary was
restarted and is ready; 8 refused before changing the live binary, fleet.prev,
the database or the service (an earlier fleet.failed may be gone); 9 rollback needs
--migrate-down (with --no-restart: a manual fleet migrate down); 10 install NOT
rolled back, the new binary was restarted but is not ready.

Shared: --cache-dir <dir> (the pinned cosign; default /var/cache/fleet as root),
FLEET_RELEASE_BASE_URL (asset mirror), FLEET_SIGSTORE_TRUSTED_ROOT (offline trust root).
`

// releaseCacheDir is where the pinned cosign and its TUF cache live.
func releaseCacheDir(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := envreg.Get("FLEET_RELEASE_CACHE_DIR"); v != "" {
		return v
	}
	if os.Geteuid() == 0 {
		return "/var/cache/fleet"
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "fleet")
	}
	return filepath.Join(os.TempDir(), "fleet-cache")
}

// newVerifier provisions the pinned cosign and returns the fleet-policy
// verifier.
var newVerifier = func(ctx context.Context, cacheDir string) (release.Verifier, error) {
	bin := envreg.Get("FLEET_COSIGN_BIN")
	if bin != "" {
		// An operator-supplied cosign must still be the pinned build: the
		// digest is the trust anchor, not the path — and what runs is a
		// private copy made from the bytes that were hashed.
		var err error
		bin, err = release.InstallCosignFrom(bin, cacheDir, release.HostArch())
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		bin, err = release.EnsureCosign(ctx, release.NewDownloader(), cacheDir, release.HostArch())
		if err != nil {
			return nil, err
		}
	}
	return &release.Cosign{Bin: bin, Policy: release.FleetPolicy(), TUFRoot: filepath.Join(cacheDir, "sigstore-tuf")}, nil
}

func releaseLatest(ctx context.Context, argv []string) int {
	if len(argv) > 0 {
		return errf(2, "fleet release latest takes no arguments")
	}
	tag, err := release.NewDownloader().LatestTag(ctx)
	if err != nil {
		return errf(5, "latest release: %v", err)
	}
	fmt.Println(tag)
	return 0
}

type releaseFlags struct {
	fs             *flag.FlagSet
	tag, dir       string
	cacheDir       string
	allowDowngrade bool
	commit         string
}

func newReleaseFlags(name string) *releaseFlags {
	f := &releaseFlags{fs: flag.NewFlagSet("release "+name, flag.ContinueOnError)}
	f.fs.StringVar(&f.tag, "tag", "", "release tag (vYYYY.MM.DD.N)")
	f.fs.StringVar(&f.dir, "dir", "", "staging directory")
	f.fs.StringVar(&f.cacheDir, "cache-dir", "", "cache for the pinned cosign")
	return f
}

func (f *releaseFlags) parse(argv []string) (release.Tag, int) {
	if err := f.fs.Parse(argv); err != nil {
		return release.Tag{}, 2
	}
	if f.fs.NArg() > 0 {
		return release.Tag{}, errf(2, "unexpected argument %q", f.fs.Arg(0))
	}
	tag, err := release.ParseTag(f.tag)
	if err != nil {
		return release.Tag{}, errf(2, "--tag: %v", err)
	}
	if f.dir == "" {
		return release.Tag{}, errf(2, "--dir is required")
	}
	return tag, 0
}

func releaseFetch(ctx context.Context, argv []string) int {
	f := newReleaseFlags("fetch")
	f.fs.BoolVar(&f.allowDowngrade, "allow-downgrade", false, "allow a release older than the running one")
	f.fs.StringVar(&f.commit, "commit", "", "the commit the tag resolves to in this checkout; release.json must name the same")
	tag, rc := f.parse(argv)
	if rc != 0 {
		return rc
	}
	if err := release.EnsurePrivateDir(f.dir); err != nil {
		return errf(5, "staging dir: %v", err)
	}
	v, err := newVerifier(ctx, releaseCacheDir(f.cacheDir))
	if err != nil {
		return errf(5, "verifier: %v", err)
	}
	got, err := release.Fetch(ctx, release.NewDownloader(), v, release.FetchOptions{
		Tag: tag, Arch: release.HostArch(), Dir: f.dir,
		Current: version.Version(), AllowDowngrade: f.allowDowngrade, Commit: f.commit,
	})
	if err != nil {
		if errors.Is(err, release.ErrDowngrade) {
			return errf(3, "%v", err)
		}
		return errf(5, "fetch %s: %v", tag, err)
	}
	if err := checkReleaseBinary(ctx, got.Binary); err != nil {
		return errf(5, "%s: %v", got.Binary, err)
	}
	fmt.Fprintf(os.Stderr, "verified %s: signed by %s, checksums match, built for linux/%s\n", tag, release.SignerIdentity, release.HostArch())
	fmt.Println(got.Binary)
	return 0
}

// checkReleaseBinary runs the VERIFIED binary's self-report and refuses one
// that carries no web bundle or has the host executor compiled in. It runs
// only after Fetch verified the signature and checksum — this is the release
// pipeline's own assertion, checked again on the box.
var checkReleaseBinary = func(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--build-info").Output()
	if err != nil {
		return fmt.Errorf("`version --build-info` failed: %w", err)
	}
	return checkBuildInfo(out, release.HostArch())
}

// checkBuildInfo validates `fleet version --build-info` output.
func checkBuildInfo(out []byte, arch string) error {
	var bi struct {
		GOARCH       string `json:"goarch"`
		HostExecutor *bool  `json:"host_executor"`
		WebBundle    string `json:"web_bundle"`
		Tags         string `json:"tags"`
	}
	if err := json.Unmarshal(out, &bi); err != nil {
		return fmt.Errorf("unreadable build info: %w", err)
	}
	if bi.HostExecutor == nil || *bi.HostExecutor || strings.Contains(bi.Tags, "fleet_host_executor") {
		return errors.New("the binary has the unsandboxed host executor compiled in (fleet_host_executor) — a release must not; refusing it")
	}
	if bi.WebBundle == "" {
		return errors.New("the binary carries no web bundle — refusing it")
	}
	if bi.GOARCH != arch {
		return fmt.Errorf("the binary reports GOARCH %q, this box is %q", bi.GOARCH, arch)
	}
	return nil
}

func releaseSandbox(ctx context.Context, argv []string) int {
	f := newReleaseFlags("sandbox")
	tag, rc := f.parse(argv)
	if rc != 0 {
		return rc
	}
	v, err := newVerifier(ctx, releaseCacheDir(f.cacheDir))
	if err != nil {
		return errf(5, "verifier: %v", err)
	}
	got, err := release.LoadVerified(ctx, v, f.dir, tag, release.HostArch())
	if err != nil {
		return errf(5, "%s: %v", f.dir, err)
	}
	_ = got.BinaryFile.Close()
	ref, err := got.Manifest.ImageRef(release.SandboxImage)
	if err != nil {
		return errf(5, "%v", err)
	}
	if err := v.VerifyImage(ctx, ref); err != nil {
		return errf(5, "sandbox image %s: %v", ref, err)
	}
	fmt.Println(ref)
	return 0
}

type installFlags struct {
	installDir, service, healthURL, serverAddr string
	healthTimeout                              time.Duration
	noRollback, noRestart, migrateDown         bool
	probeServerName                            string
}

func (i *installFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&i.installDir, "install-dir", "/opt/fleet", "directory holding the live fleet binary")
	fs.StringVar(&i.service, "service", "fleet", "systemd unit to restart")
	fs.StringVar(&i.healthURL, "health-url", "", "readiness URL to gate on (default: derived from --server-addr)")
	fs.StringVar(&i.serverAddr, "server-addr", "", "the chat listener (default: FLEET_SERVER_ADDR from the environment or the server env file)")
	fs.DurationVar(&i.healthTimeout, "health-timeout", 90*time.Second, "how long to wait for readiness")
	fs.BoolVar(&i.noRestart, "no-restart", false, "swap the binary only: no restart, no readiness gate (a fleet not run by systemd)")
	fs.StringVar(&i.probeServerName, "probe-server-name", "", "the domain the chat listener's TLS certificate names (default: FLEET_TLS_DOMAIN)")
}

// healthURL is the readiness URL to gate on: --health-url, else the address
// the deployment ACTUALLY serves on — --server-addr, else FLEET_SERVER_ADDR
// from the environment or the server env file (envOrFile, the lookup every
// other verb uses) — with https when FLEET_TLS_MODE has the chat listener
// terminate TLS. Probing a default 127.0.0.1:8080 on a box that listens
// elsewhere would roll back a healthy release.
func (i *installFlags) resolvedHealthURL() string {
	if i.healthURL != "" {
		return i.healthURL
	}
	addr := i.serverAddr
	if addr == "" {
		addr = envOrFile("FLEET_SERVER_ADDR")
	}
	return release.HealthURL(addr, envOrFile("FLEET_TLS_MODE"))
}

// probeTLS is how the readiness probe verifies a TLS chat listener: the
// deployment's own domain (--probe-server-name, else FLEET_TLS_DOMAIN) and,
// in manual mode, its certificate file — verification is never skipped.
func (i *installFlags) probeTLS(url string) (*tls.Config, error) {
	name := i.probeServerName
	if name == "" {
		name = envOrFile("FLEET_TLS_DOMAIN")
	}
	cfg, err := release.ProbeTLSConfig(url, envOrFile("FLEET_TLS_MODE"), name, envOrFile("FLEET_TLS_CERT_FILE"))
	if errors.Is(err, release.ErrProbeServerName) {
		return nil, fmt.Errorf("%w — set FLEET_TLS_DOMAIN in the env file, or pass --probe-server-name <domain> (or --health-url https://<domain>/readyz)", err)
	}
	return cfg, err
}

func (i *installFlags) options(w io.Writer) (release.InstallOptions, error) {
	url := i.resolvedHealthURL()
	o := release.InstallOptions{
		InstallDir: i.installDir, Service: i.service, HealthURL: url,
		HealthTimeout: i.healthTimeout, NoRollback: i.noRollback, NoRestart: i.noRestart,
		MigrateDown: i.migrateDown, Restarter: installRestarter, Log: w,
		// A rollback undoes the migrations the newer binary applied before
		// fleet.prev starts, which refuses a database it does not know.
		Schema: installSchema(),
	}
	if i.noRestart {
		return o, nil // nothing is probed
	}
	// The update drain marker comes off between stopping the old process and
	// starting the new one (release.InstallOptions.ClearDrain); without this
	// the new process starts draining and fails its readiness gate.
	o.ClearDrain = clearHandedOffDrain
	cfg, err := i.probeTLS(url)
	if err != nil {
		return o, err
	}
	o.Prober = newInstallProber(cfg, w)
	return o, nil
}

// Seams for tests.
var (
	installRestarter release.Restarter = release.Systemctl{}
	newInstallProber                   = func(cfg *tls.Config, log io.Writer) release.Prober {
		return release.HTTPProber{TLS: cfg, Log: log}
	}
	installSchema = func() release.Schema { return releaseSchema{} }
)

// releaseSchema is release.Schema over the one database and THIS binary's
// migrations — which, for `fleet release install` run as update.sh runs it,
// are the new release's, so it carries the down migration of every version
// the new release may have applied. dsn, when set, is used as is (tests).
type releaseSchema struct{ dsn string }

// releaseSchemaReadTimeout bounds one read of the ledger;
// releaseDownLockTimeout bounds each lock wait of the down migrations, well
// inside the rollback's budget (FLEET_HEALTH_TIMEOUT + 7 minutes).
const (
	releaseSchemaReadTimeout = 15 * time.Second
	releaseDownLockTimeout   = 60 * time.Second
)

// releaseSchemaDSN is the database `fleet serve` connects to, resolved exactly
// as the server resolves it — DATABASE_URL or the DB_* parts from the
// environment or the env file, else the localhost defaults the server falls
// back to (the update's idle gate does the same) — unlike chatDSN, which
// refuses the bare defaults. A configuration the server would refuse (a
// retired per-store key) is an error.
func releaseSchemaDSN() (string, error) {
	d, err := resolvedDSNs()
	if err != nil {
		return "", err
	}
	return d.OverrideFor("", config.ChatSchema), nil
}

func (s releaseSchema) open() (*sql.DB, error) {
	dsn := s.dsn
	if dsn == "" {
		var err error
		if dsn, err = releaseSchemaDSN(); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	return db, nil
}

func (s releaseSchema) Applied(ctx context.Context) ([]int, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(ctx, releaseSchemaReadTimeout)
	defer cancel()
	return migrations.Applied(ctx, db)
}

func (s releaseSchema) DownTo(ctx context.Context, v int) ([]int, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	plan, err := migrations.DownBounded(ctx, db, v, releaseDownLockTimeout)
	if err != nil {
		return nil, err
	}
	undone := make([]int, 0, len(plan.Steps))
	for _, m := range plan.Steps {
		undone = append(undone, m.Version)
	}
	return undone, nil
}

// Exit codes of `fleet release install` and `rollback` (update.sh reads them;
// docs/RELEASE-ARTIFACTS.md lists them).
const (
	releaseExitRolledBack      = 4  // install: the previous binary is live again
	releaseExitFailed          = 5  // anything else: read the message
	releaseExitBusy            = 6  // another install or rollback holds the install dir
	releaseExitKeptNewReady    = 7  // install: not rolled back; the new binary was restarted and is ready
	releaseExitNothingChanged  = 8  // refused before changing anything
	releaseExitMigrateDown     = 9  // rollback: needs --migrate-down (or, with --no-restart, a manual down)
	releaseExitKeptNewNotReady = 10 // install: not rolled back; the new binary was restarted but is not ready
)

func releaseInstall(ctx context.Context, argv []string) int {
	f := newReleaseFlags("install")
	var inf installFlags
	inf.register(f.fs)
	f.fs.BoolVar(&inf.noRollback, "no-rollback", false, "leave a failed binary in place")
	tag, rc := f.parse(argv)
	if rc != 0 {
		return rc
	}
	v, err := newVerifier(ctx, releaseCacheDir(f.cacheDir))
	if err != nil {
		return errf(releaseExitNothingChanged, "verifier: %v — nothing was changed", err)
	}
	// Re-verify the staged files: they were verified by `fetch`, possibly by a
	// different process, and the install must not trust that on its word.
	got, err := release.LoadVerified(ctx, v, f.dir, tag, release.HostArch())
	if err != nil {
		return errf(releaseExitNothingChanged, "%s: %v — nothing was changed", f.dir, err)
	}
	o, err := inf.options(os.Stderr)
	if err != nil {
		return errf(releaseExitNothingChanged, "install %s: %v — nothing was changed", tag, err)
	}
	defer got.BinaryFile.Close()
	o.Binary = got.Binary
	o.Source = got.BinaryFile
	o.SHA256 = got.BinarySHA256
	res, err := release.Install(ctx, o)
	if err != nil {
		switch {
		case errors.Is(err, release.ErrRolledBack):
			undone := ""
			if len(res.UndoneMigrations) > 0 {
				undone = fmt.Sprintf("; the database migrations it applied (%s) were undone first", joinIntsCSV(res.UndoneMigrations))
			}
			return errf(releaseExitRolledBack, "%v — the previous binary is live again%s; the failed one is kept as %s", err, undone, filepath.Join(inf.installDir, "fleet.failed"))
		case errors.Is(err, release.ErrKeptNew):
			code := releaseExitKeptNewNotReady
			if res.Restarted {
				code = releaseExitKeptNewReady
			}
			return errf(code, "install %s: %v — %s is still the new binary; journalctl -u %s -n 100, and see docs/RELEASE-ARTIFACTS.md (Roll back)", tag, err, res.Live, inf.service)
		case errors.Is(err, release.ErrBusy):
			return errf(releaseExitBusy, "install %s: %v", tag, err)
		case errors.Is(err, release.ErrNothingChanged):
			return errf(releaseExitNothingChanged, "install %s: %v", tag, err)
		}
		return errf(releaseExitFailed, "install %s: %v", tag, err)
	}
	switch {
	case inf.noRestart:
		fmt.Fprintf(os.Stderr, "fleet %s is installed at %s (previous binary: %s) but NOT restarted (--no-restart): restart it with your supervisor; if it does not come up, `fleet release rollback --no-restart` puts the previous one back\n", tag, res.Live, orNone(res.Previous))
	case res.Unchanged:
		fmt.Fprintf(os.Stderr, "fleet %s was already live at %s; restarted and ready (%s); previous binary unchanged (%s)\n", tag, res.Live, o.HealthURL, orNone(res.Previous))
	default:
		fmt.Fprintf(os.Stderr, "fleet %s is live at %s and ready (%s); previous binary kept as %s\n", tag, res.Live, o.HealthURL, orNone(res.Previous))
	}
	return 0
}

func releaseRollback(ctx context.Context, argv []string) int {
	fs := flag.NewFlagSet("release rollback", flag.ContinueOnError)
	var inf installFlags
	inf.register(fs)
	fs.BoolVar(&inf.migrateDown, "migrate-down", false, "undo the database migrations above the version fleet.prev ran on (drops what was stored in what they added; back up first)")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return errf(2, "unexpected argument %q", fs.Arg(0))
	}
	o, err := inf.options(os.Stderr)
	if err != nil {
		return errf(releaseExitNothingChanged, "rollback: %v — nothing was changed", err)
	}
	if err := release.Rollback(ctx, o); err != nil {
		switch {
		case errors.Is(err, release.ErrBusy):
			return errf(releaseExitBusy, "rollback: %v", err)
		case errors.Is(err, release.ErrMigrateDownNeeded):
			return errf(releaseExitMigrateDown, "rollback: %v", err)
		case errors.Is(err, release.ErrNothingChanged):
			return errf(releaseExitNothingChanged, "rollback: %v", err)
		}
		return errf(releaseExitFailed, "rollback: %v", err)
	}
	if inf.noRestart {
		fmt.Fprintf(os.Stderr, "rolled back: %s is the previous binary again — NOT restarted (--no-restart); restart it with your supervisor\n", filepath.Join(inf.installDir, "fleet"))
		return 0
	}
	fmt.Fprintf(os.Stderr, "rolled back: %s is the previous binary again and ready (%s); the one it replaced is now fleet.prev\n", filepath.Join(inf.installDir, "fleet"), o.HealthURL)
	return 0
}

func joinIntsCSV(vs []int) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprint(v)
	}
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
