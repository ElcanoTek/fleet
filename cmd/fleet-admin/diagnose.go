package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/redact"
)

// cmdDiagnose is the `fleet-admin diagnose` support-bundle collector. It gathers
// the read-only health/status report, a REDACTED config summary, the migration
// versions of both databases, and sandbox image info into a single gzipped tar
// archive an operator can attach to an issue. It NEVER uploads anything — it only
// writes a local file — and it NEVER writes a secret VALUE into the bundle: every
// section is run through the centralized scrubber (internal/redact, seeded with
// the values of secret-named env vars) before it is added to the tarball, and the
// config section lists only env-var NAMES (no values) and bundle metadata (app
// name, model hints, MCP server names — never credentials).
//
// The health section REUSES the exact `fleet-admin status` checks (see
// captureHealth) rather than re-implementing them, so the bundle's view of health
// can never drift from `fleet-admin doctor`.
//
// Exit codes: 0 wrote the bundle · 1 usage/IO error. A failed individual section
// is NOT fatal: its file carries an "ERROR collecting …" line and the rest of the
// bundle is still written, so a box with (say) podman missing still produces a
// useful bundle.
func cmdDiagnose(argv []string) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	out := fs.String("output", "", "output tarball path (default fleet-diagnose-<UTC stamp>.tar.gz in the current dir)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	bundleDir := fs.String("client-config", "", "client bundle dir (default FLEET_CLIENT_CONFIG_DIR / config/default)")
	service := fs.String("service", "", "systemd unit to inspect (default FLEET_SERVICE_NAME / fleet)")
	skipSandbox := fs.Bool("no-sandbox", false, "skip the podman sandbox checks (status + sandbox sections)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	dc := &diagnoseCollector{
		chatURL:     strings.TrimSpace(*chatURL),
		schedURL:    strings.TrimSpace(*schedURL),
		bundleDir:   strings.TrimSpace(*bundleDir),
		service:     serviceName(*service),
		skipSandbox: *skipSandbox,
		// Seed the scrubber with the canonical patterns PLUS literal redaction of
		// the values of secret-named env vars (OPENROUTER_API_KEY, connector
		// credentials, …) so a novel key format is still scrubbed by value — the
		// same construction agentcore uses for tool output.
		redactor: func() *redact.Redactor {
			r := redact.NewRedactor(nil)
			r.RegisterEnvLiterals(os.Environ())
			return r
		}(),
	}

	outPath := strings.TrimSpace(*out)
	if outPath == "" {
		outPath = fmt.Sprintf("fleet-diagnose-%s.tar.gz", time.Now().UTC().Format("20060102T150405Z"))
	}

	if err := dc.write(outPath); err != nil {
		return errf(1, "diagnose: %v", err)
	}
	fmt.Fprintf(os.Stderr, "wrote support bundle (secret values redacted) → %s\n", outPath)
	fmt.Println(outPath)
	return 0
}

// diagnoseCollector holds the resolved inputs and the shared scrubber. One method
// per bundle section; each returns the section body as a string (the writer loop
// redacts and adds it).
type diagnoseCollector struct {
	chatURL     string
	schedURL    string
	bundleDir   string
	service     string
	skipSandbox bool
	redactor    *redact.Redactor
}

// section is one file in the bundle: a name and the function that produces its
// (pre-redaction) body.
type section struct {
	name string
	fn   func(ctx context.Context) string
}

func (dc *diagnoseCollector) sections() []section {
	return []section{
		{"status.txt", dc.collectStatus},
		{"config.txt", dc.collectConfig},
		{"db.txt", dc.collectDB},
		{"sandbox.txt", dc.collectSandbox},
	}
}

// write runs every section, scrubs each body, and writes them into a gzipped tar
// at outPath. A failing section is captured as an "ERROR collecting …" line (via
// each collector returning that text) so the bundle is always complete.
func (dc *diagnoseCollector) write(outPath string) error {
	f, err := os.Create(outPath) //nolint:gosec // G304: operator-supplied output path for a support bundle, not request/LLM input.
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	// Close errors matter for an archive (a truncated tar is a corrupt bundle), so
	// surface them rather than deferring a bare Close.
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	now := time.Now().UTC()
	for _, sec := range dc.sections() {
		body := dc.redactor.Redact(sec.fn(ctx))
		if err := writeTarFile(tw, sec.name, body, now); err != nil {
			_ = tw.Close()
			_ = gw.Close()
			_ = f.Close()
			return fmt.Errorf("write %s: %w", sec.name, err)
		}
	}

	if err := tw.Close(); err != nil {
		_ = gw.Close()
		_ = f.Close()
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		_ = f.Close()
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", outPath, err)
	}
	return nil
}

// writeTarFile adds a single text member to the tar. 0600 because a support
// bundle can still contain non-secret-but-sensitive operational detail (hostnames,
// DSN hosts) the operator should choose to share, not the world.
func writeTarFile(tw *tar.Writer, name, body string, modTime time.Time) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(body)),
		ModTime: modTime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write([]byte(body))
	return err
}

// ── section collectors ───────────────────────────────────────────────────────

// collectStatus reuses the EXACT `fleet-admin status` health checks (captureHealth)
// so the bundle's health view can never drift from `fleet-admin doctor`.
func (dc *diagnoseCollector) collectStatus(ctx context.Context) string {
	return captureHealth(ctx, dc.chatURL, dc.schedURL, dc.bundleDir, dc.service, dc.skipSandbox)
}

// collectConfig summarizes the deployment config WITHOUT any secret value: it
// lists the NAMES of the set FLEET_*/CHAT_* env vars (names only, never values)
// and the loaded bundle's non-credential metadata (app name, model hints, MCP
// server names). Even so the whole body is run through the scrubber by write().
func (dc *diagnoseCollector) collectConfig(_ context.Context) string {
	var sb strings.Builder
	sb.WriteString("# fleet config summary (names + non-credential metadata only — no secret values)\n\n")

	sb.WriteString("## environment variable names set (values intentionally omitted)\n")
	var names []string
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			name := kv[:eq]
			if strings.HasPrefix(name, "FLEET_") || strings.HasPrefix(name, "CHAT_") || name == "DATABASE_URL" || name == "OPENROUTER_API_KEY" {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		sb.WriteString("(none of FLEET_*/CHAT_*/DATABASE_URL/OPENROUTER_API_KEY are set)\n")
	}
	for _, n := range names {
		sb.WriteString(n + "\n")
	}

	sb.WriteString("\n## client bundle\n")
	b, err := clientconfig.Load(dc.bundleDir)
	if err != nil {
		fmt.Fprintf(&sb, "ERROR collecting bundle: %v\n", err)
		return sb.String()
	}
	fmt.Fprintf(&sb, "dir:         %s\n", b.Dir)
	fmt.Fprintf(&sb, "app_name:    %s\n", b.Branding.AppName)
	fmt.Fprintf(&sb, "model_core:  %s\n", b.Models.DefaultCore)
	fmt.Fprintf(&sb, "model_max:   %s\n", b.Models.DefaultMax)
	fmt.Fprintf(&sb, "mcp_servers: %d\n", len(b.MCPCatalog))
	for _, s := range b.MCPCatalog {
		// Name + type only — never the command/url/headers, which can carry tokens.
		fmt.Fprintf(&sb, "  - %s (%s)\n", s.Name, s.Type)
	}
	return sb.String()
}

// collectDB reports the migration version of both databases via READ-ONLY SQL
// against the same `schema_migrations` table each migration system maintains
// (chat: MAX(version); sched/golang-migrate: the single version + dirty row). It
// opens a bare pgx connection — it never runs migrations and never writes — and
// echoes only the host of the DSN (userinfo stripped by redactDSN), never the
// password.
func (dc *diagnoseCollector) collectDB(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("# database migration versions (read-only; no migrations run, no secrets)\n\n")
	sb.WriteString(dc.dbVersion(ctx, "chat", dc.chatURL, chatDSN, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations"))
	sb.WriteString("\n")
	sb.WriteString(dc.dbVersion(ctx, "sched", dc.schedURL, schedDSN, "SELECT version, dirty FROM schema_migrations LIMIT 1"))
	return sb.String()
}

// dbVersion runs query against the named DB and renders a one-line version
// summary. chat returns a single version int; sched (golang-migrate) returns
// (version, dirty). The query is detected by its column count so one helper serves
// both. Any failure (DSN unresolved, DB unreachable, table absent) becomes an
// "ERROR" line, never a fatal — a box with one DB down still yields a bundle.
func (dc *diagnoseCollector) dbVersion(ctx context.Context, label, flagURL string, resolve func(string) (string, error), query string) string {
	dsn, err := resolve(flagURL)
	if err != nil {
		return fmt.Sprintf("%s: ERROR DSN unresolved: %v", label, err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Sprintf("%s: ERROR open: %v (dsn=%s)", label, err, redactDSN(dsn))
	}
	defer func() { _ = db.Close() }()
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cols := strings.Count(query, ",") + 1
	if cols == 2 {
		var version uint64
		var dirty bool
		if err := db.QueryRowContext(qctx, query).Scan(&version, &dirty); err != nil {
			return fmt.Sprintf("%s: ERROR query: %v (dsn=%s)", label, err, redactDSN(dsn))
		}
		state := "clean"
		if dirty {
			state = "DIRTY (a migration failed mid-run)"
		}
		return fmt.Sprintf("%s: migration version %d (%s) [%s]", label, version, state, redactDSN(dsn))
	}
	var version uint64
	if err := db.QueryRowContext(qctx, query).Scan(&version); err != nil {
		return fmt.Sprintf("%s: ERROR query: %v (dsn=%s)", label, err, redactDSN(dsn))
	}
	return fmt.Sprintf("%s: migration version %d [%s]", label, version, redactDSN(dsn))
}

// collectSandbox records the resolved sandbox image ref the running process would
// consume and, when podman is present, `podman images` for that ref (digest +
// size). It does NOT run a container (that is the status section's job) — this is
// a metadata snapshot. podman absent or the image missing is reported, not fatal.
func (dc *diagnoseCollector) collectSandbox(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("# sandbox image info\n\n")

	ref := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_IMAGE"))
	if ref == "" {
		ref = strings.TrimSpace(os.Getenv("CHAT_SANDBOX_IMAGE"))
	}
	if ref == "" {
		if b, err := clientconfig.Load(dc.bundleDir); err == nil {
			ref = strings.TrimSpace(b.Sandbox().ResolvedImageRef())
		}
	}
	if ref == "" {
		sb.WriteString("resolved image ref: (none — set FLEET_SANDBOX_IMAGE or the bundle's sandbox.tag/image)\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "resolved image ref: %s\n\n", ref)

	if dc.skipSandbox {
		sb.WriteString("(podman inspection skipped: --no-sandbox)\n")
		return sb.String()
	}
	if _, err := exec.LookPath("podman"); err != nil {
		sb.WriteString("podman not on PATH — cannot inspect the image (install podman)\n")
		return sb.String()
	}

	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "podman" binary; ref is the operator's resolved bundle image, not request/LLM input. --format is a literal template.
	cmd := exec.CommandContext(ictx, "podman", "images", "--format", "{{.Repository}}:{{.Tag}} {{.ID}} {{.Size}} (created {{.CreatedAt}})", ref)
	outBytes, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(outBytes))
	switch {
	case err != nil:
		fmt.Fprintf(&sb, "podman images %s: ERROR %v: %s\n", ref, err, firstLine(out))
	case out == "":
		fmt.Fprintf(&sb, "image %s not present locally (build it: scripts/build-sandbox-image.sh)\n", ref)
	default:
		sb.WriteString(out + "\n")
	}
	return sb.String()
}

// captureHealth runs the EXACT `fleet-admin status` checks and returns their
// report as a string, so the diagnose bundle's status.txt is byte-for-byte the
// same set of ✓/✗ lines `fleet-admin doctor` prints — no duplicated logic. It
// points both report writers at one buffer (the header/summary that status sends
// to stderr is captured here too, for a self-contained section). The DSN values
// echoed by the checks are already userinfo-stripped (redactDSN); the whole body
// is additionally scrubbed by write().
func captureHealth(ctx context.Context, chatURL, schedURL, bundleDir, service string, skipSandbox bool) string {
	var buf bytes.Buffer
	r := &report{out: &buf, summary: &buf}
	r.head()

	bundle := checkBundle(r, bundleDir)
	checkEnv(r)
	checkDB(r, "chat database", chatURL, chatDSN)
	checkDB(r, "sched database", schedURL, schedDSN)
	if skipSandbox {
		r.skip("sandbox image", "skipped (--no-sandbox)")
	} else {
		checkSandbox(r, bundle)
	}
	checkService(r, serviceName(service))
	r.finish()

	_ = ctx // checks use their own short per-call timeouts; ctx reserved for parity with other collectors.
	return buf.String()
}
