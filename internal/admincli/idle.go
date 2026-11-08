package admincli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

const (
	defaultIdleApprovalWindow = 2 * time.Hour
)

var (
	drainOwnerToken = fmt.Sprintf("draining pid-%d-%d\n", os.Getpid(), time.Now().UnixNano())
)

// ActiveWorkError is returned by WaitIdle when scheduled tasks, chat turns, or
// detached approval executions remain in flight after the wait window expires.
type ActiveWorkError struct {
	Items []string
}

func (e *ActiveWorkError) Error() string {
	return strings.Join(e.Items, "; ")
}

// drainFilePath resolves the update drain sentinel path from the process
// environment or the deployment env file, matching httpapi.UpdateDrainFile and
// scripts/lib/idle.sh. Relative override paths are anchored under /etc/fleet so
// the daemon and operator CLI agree on one path regardless of working directory.
func drainFilePath() string {
	for _, key := range []string{"FLEET_UPDATE_DRAIN_FILE", "FLEET_DRAIN_FILE"} {
		if p := strings.TrimSpace(envOrFile(key)); p != "" {
			if filepath.IsAbs(p) {
				return filepath.Clean(p)
			}
			return filepath.Join("/etc/fleet", p)
		}
	}
	if envFile := serverEnvFile(""); filepath.IsAbs(envFile) {
		return filepath.Clean(envFile) + ".update.drain"
	}
	return filepath.Join("/etc/fleet", "fleet.env.update.drain")
}

func drainOwnerAlive(content string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(content), "draining pid-")
	if !ok {
		return false
	}
	dash := strings.IndexByte(rest, '-')
	if dash <= 0 {
		return false
	}
	pid, err := strconv.Atoi(rest[:dash])
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func verifyDrainDir(p string) error {
	dir := filepath.Dir(p)
	st, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat drain sentinel dir %s: %w", dir, err)
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("drain sentinel dir %s is not a real directory", dir)
	}
	if os.Geteuid() == 0 {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Uid != 0 {
			return fmt.Errorf("drain sentinel dir %s must be owned by root when running as root", dir)
		}
		if st.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("drain sentinel dir %s must not be group/world-writable when running as root", dir)
		}
	}
	return nil
}

func createDrainFileAtomic(p string) error {
	//nolint:gosec // G304,G306: non-secret drain sentinel inspected by the fleet service user.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(drainOwnerToken)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(p)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(p)
		return cerr
	}
	return nil
}

// drainLockTimeout bounds the wait for the drain sentinel's lock. Holders only
// do a few small file operations under it, so a lock still held after this is
// a stalled process; giving up keeps a caller such as the release install's
// stop → clear → start handoff from blocking forever (the stale marker then
// ages out on its own).
var drainLockTimeout = 30 * time.Second

// flockBounded takes an exclusive flock on f, polling a non-blocking attempt
// until timeout.
func flockBounded(f *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still held by another process after %s", timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func withDrainLock(p string, fn func() error) error {
	if err := verifyDrainDir(p); err != nil {
		return err
	}
	lockPath := p + ".lock"
	//nolint:gosec // G304,G306: companion lock file for drain sentinel serialization.
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open drain lock %s: %w", lockPath, err)
	}
	defer func() { _ = lf.Close() }()
	if err := flockBounded(lf, drainLockTimeout); err != nil {
		return fmt.Errorf("lock drain sentinel %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	return fn()
}

// SetUpdateDrain writes or refreshes the update drain sentinel file so the
// running Fleet server refuses new chat turns/approvals and pauses scheduler
// task claims. If another live lifecycle process already owns a fresh sentinel,
// it refuses rather than stealing ownership.
func SetUpdateDrain() error {
	p := drainFilePath()
	if p == "" {
		return fmt.Errorf("update drain file path is empty")
	}
	//nolint:gosec // G301,G306: non-secret drain sentinel inspected by the fleet service user.
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("create drain sentinel dir: %w", err)
	}
	return withDrainLock(p, func() error {
		if err := createDrainFileAtomic(p); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("write drain sentinel %s: %w", p, err)
		}
		if lst, err := os.Lstat(p); err == nil && lst.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("drain sentinel %s is a symlink", p)
		}
		//nolint:gosec // G304: drainFilePath resolves a local sentinel path from operator env/config.
		data, rerr := os.ReadFile(p)
		cur := string(data)
		if rerr == nil && cur == drainOwnerToken {
			now := time.Now()
			if err := os.Chtimes(p, now, now); err != nil {
				return fmt.Errorf("refresh drain sentinel %s: %w", p, err)
			}
			return nil
		}
		if st, err := os.Lstat(p); err == nil && time.Since(st.ModTime()) <= 15*time.Minute {
			if strings.TrimSpace(cur) == "" || drainOwnerAlive(cur) {
				return fmt.Errorf("another update or service drain is already in progress (%s)", strings.TrimSpace(cur))
			}
		}
		_ = os.Remove(p)
		if err := createDrainFileAtomic(p); err != nil {
			return fmt.Errorf("write drain sentinel %s: %w", p, err)
		}
		return nil
	})
}

// clearHandedOffDrain removes the update drain sentinel on behalf of the
// update that holds it, once the process it fenced has stopped. `fleet
// release install`/`rollback` run inside update.sh's drain fence and start a
// new fleet process that reads the same sentinel: left in place, that process
// answers /readyz 503 "draining" and the health gate rolls the release back.
//
// It removes the sentinel only when it is this process's own, the caller's
// (FLEET_DRAIN_OWNER_TOKEN, exported by update.sh and set by `fleet update`),
// or a lifecycle drain whose owner process is gone. A drain another live
// process holds, or a marker an operator wrote by hand, is left alone and
// reported. A missing sentinel is not an error.
func clearHandedOffDrain() error {
	p := drainFilePath()
	if p == "" {
		return nil
	}
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	callerToken := strings.TrimSpace(envreg.Get("FLEET_DRAIN_OWNER_TOKEN"))
	return withDrainLock(p, func() error {
		lst, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("drain sentinel %s is a symlink — not touching it", p)
		}
		//nolint:gosec // G304: drainFilePath resolves a local sentinel path from operator env/config.
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		cur := strings.TrimSpace(string(data))
		owned := string(data) == drainOwnerToken ||
			(callerToken != "" && cur == "draining "+callerToken) ||
			(strings.HasPrefix(cur, "draining pid-") && !drainOwnerAlive(cur))
		if !owned {
			return fmt.Errorf("drain sentinel %s is held by someone else (%q) — the service reports not-ready until it is removed", p, cur)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// ClearUpdateDrain removes the update drain sentinel file if it is owned by
// this process invocation.
func ClearUpdateDrain() {
	p := drainFilePath()
	if p == "" {
		return
	}
	_ = withDrainLock(p, func() error {
		lst, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		//nolint:gosec // G304: drainFilePath resolves a local sentinel path from operator env/config.
		if data, err := os.ReadFile(p); err == nil && string(data) == drainOwnerToken {
			_ = os.Remove(p)
		}
		return nil
	})
}

func parseServiceTimestamp(startTS string) int64 {
	startTS = strings.TrimSpace(startTS)
	if startTS == "" || startTS == "n/a" {
		return 0
	}
	if epochStr, ok := strings.CutPrefix(startTS, "@"); ok {
		epochStr = strings.TrimSpace(epochStr)
		if dot := strings.IndexByte(epochStr, '.'); dot >= 0 {
			epochStr = epochStr[:dot]
		}
		if secs, err := strconv.ParseInt(epochStr, 10, 64); err == nil && secs > 0 {
			return secs
		}
	}
	if t, err := time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", startTS, time.Local); err == nil {
		return t.Unix()
	}
	return 0
}

// serviceRuntimeState inspects systemd for the unit's current LoadState,
// ActiveState, and ExecMainStartTimestamp. It returns ("active", startUnix),
// ("inactive", 0) when the unit is installed and not active, ("no-unit", 0)
// when systemd or the unit is not installed, or ("unknown", 0) when the
// systemctl probe fails.
func serviceRuntimeState(ctx context.Context, unit string) (string, int64) {
	if truthy(envreg.Get("FLEET_ASSUME_SERVICE_ACTIVE")) {
		return "active", 0
	}
	if strings.TrimSpace(unit) == "" {
		unit = serviceName("")
	}
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "no-unit", 0
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; unit is resolved via serviceName.
	out, err := exec.CommandContext(sctx, "systemctl", "show", "--timestamp=unix", "-p", "LoadState", "-p", "ActiveState", "-p", "ExecMainStartTimestamp", unit).Output()
	if err != nil {
		return "unknown", 0
	}
	var loadState, activeState, startTS string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "LoadState="); ok {
			loadState = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
			activeState = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "ExecMainStartTimestamp="); ok {
			startTS = strings.TrimSpace(v)
		}
	}
	if loadState == "not-found" {
		return "no-unit", 0
	}
	switch activeState {
	case "active", "activating", "reloading":
		return "active", parseServiceTimestamp(startTS)
	case "":
		return "unknown", 0
	default:
		return "inactive", 0
	}
}

func sanitizeSummaryText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func localHealthTLSMode() string {
	if v := strings.ToLower(strings.TrimSpace(envOrFile("FLEET_TLS_MODE"))); v != "" {
		return v
	}
	return "off"
}

func localHealthTLSConfig(requireCert bool) (*tls.Config, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	serverName := strings.TrimSpace(envOrFile("FLEET_TLS_DOMAIN"))
	if certFile := strings.TrimSpace(envOrFile("FLEET_TLS_CERT_FILE")); certFile != "" {
		pemBytes, err := readLocalHealthCert(certFile, requireCert)
		if err != nil {
			return nil, err
		}
		if pemBytes != nil {
			if !rootCAs.AppendCertsFromPEM(pemBytes) && requireCert {
				return nil, fmt.Errorf("parse TLS cert PEM %s", certFile)
			}
			if serverName == "" {
				serverName = certServerName(pemBytes)
			}
		}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		ServerName: serverName,
	}, nil
}

// readLocalHealthCert reads the operator's TLS certificate. A read failure
// is an error only when requireCert; otherwise the probe goes on without the
// certificate (nil bytes).
func readLocalHealthCert(certFile string, requireCert bool) ([]byte, error) {
	//nolint:gosec // G304: operator-configured TLS certificate path from fleet.env.
	pemBytes, err := os.ReadFile(certFile)
	if err == nil {
		return pemBytes, nil
	}
	if requireCert {
		return nil, fmt.Errorf("read TLS cert %s: %w", certFile, err)
	}
	return nil, nil // without requireCert the probe runs without the certificate
}

// certServerName is the first DNS name (else the common name) of the first
// certificate in pemBytes, or "".
func certServerName(pemBytes []byte) string {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	if len(cert.DNSNames) > 0 {
		return cert.DNSNames[0]
	}
	return cert.Subject.CommonName
}

func probeInMemoryActiveCounts(ctx context.Context, svcState string) (turns, approvals int, hasApprovalsHeader bool, err error) {
	addr := strings.TrimSpace(envOrFile("FLEET_SERVER_ADDR"))
	if addr == "" && (svcState == "active" || svcState == "unknown") && !truthy(envreg.Get("FLEET_ASSUME_SERVICE_ACTIVE")) {
		addr = "127.0.0.1:8080"
	}
	if addr == "" {
		return 0, 0, false, nil
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if rest, ok := strings.CutPrefix(addr, "0.0.0.0:"); ok {
		addr = "127.0.0.1:" + rest
	} else if rest, ok := strings.CutPrefix(addr, "[::]:"); ok {
		addr = "[::1]:" + rest
	}
	url := addr
	useTLS := strings.HasPrefix(url, "https://")
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		tlsMode := localHealthTLSMode()
		if tlsMode == "manual" || tlsMode == "auto" {
			url = "https://" + url
			useTLS = true
		} else {
			url = "http://" + url
		}
	}
	url = strings.TrimRight(url, "/") + "/healthz"

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, false, err
	}
	tlsCfg, err := localHealthTLSConfig(useTLS)
	if err != nil {
		return 0, 0, false, err
	}
	if tlsCfg.ServerName != "" {
		req.Host = tlsCfg.ServerName
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, false, err
	}
	_ = resp.Body.Close()
	rawTurns := strings.TrimSpace(resp.Header.Get("X-Fleet-Active-Turns"))
	turns, terr := strconv.Atoi(rawTurns)
	hasTurnsHeader := rawTurns != "" && terr == nil && turns >= 0
	rawApprovals := strings.TrimSpace(resp.Header.Get("X-Fleet-Active-Approvals"))
	approvals, aerr := strconv.Atoi(rawApprovals)
	hasApprovalsHeader = rawApprovals != "" && aerr == nil && approvals >= 0
	// /healthz returns 503 with X-Fleet-Active-Turns and X-Fleet-Active-Approvals
	// when the update drain sentinel is active; accept 503 only when both Fleet
	// headers are present so an unrelated 503 proxy error still surfaces.
	draining503 := resp.StatusCode == http.StatusServiceUnavailable && hasTurnsHeader && hasApprovalsHeader
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && !draining503 {
		return 0, 0, false, fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return turns, approvals, hasApprovalsHeader, nil
}

// ActiveWork inspects the sched and chat databases for in-flight work on the
// default service unit.
func ActiveWork(ctx context.Context, schedDSNVal, chatDSNVal string) ([]string, error) {
	return ActiveWorkForService(ctx, serviceName(""), schedDSNVal, chatDSNVal)
}

// ActiveWorkForService inspects the sched and chat databases (plus the running
// server's in-memory counters on /healthz) for in-flight work that would be
// interrupted by stopping or restarting the given systemd unit:
//   - sched.tasks with status IN ('running', 'leased', 'assigned') that hold an
//     unexpired lease or were started by the current service process
//   - chat.turns with status = 'running' AND finished_at IS NULL in the current service instance
//   - chat.approvals with status = 'approved' AND is_err IS NULL AND
//     result_text LIKE 'Approved%executing%' within 2h in the current service instance
func ActiveWorkForService(ctx context.Context, unit, schedDSNVal, chatDSNVal string) ([]string, error) {
	svcState, svcStartUnix := serviceRuntimeState(ctx, unit)
	if svcState == "inactive" {
		// When the service unit is installed and not active, any running/leased rows
		// in Postgres are crash-stranded and will be recovered on boot.
		return nil, nil
	}

	var items []string
	var firstErr error

	if strings.TrimSpace(schedDSNVal) != "" {
		found, err := querySchedActiveWork(ctx, schedDSNVal, svcStartUnix, svcState == "unknown")
		if err != nil && firstErr == nil {
			firstErr = err
		}
		items = append(items, found...)
	}

	// Consult the running server's in-memory counters via /healthz so a live
	// turn whose initial CreateTurn failed during a transient database hiccup is
	// still protected even if it has no row in turns, and so a completed approval
	// whose SetApprovalResult failed does not wedge the idle gate when the live
	// daemon reports X-Fleet-Active-Approvals: 0.
	memTurns, memApprovals, hasMemApprovals, memErr := probeInMemoryActiveCounts(ctx, svcState)
	if memErr != nil && (svcState == "active" || svcState == "unknown") && firstErr == nil {
		firstErr = fmt.Errorf("in-memory healthz probe failed: %w", memErr)
	}
	skipApprovalsQuery := hasMemApprovals && memApprovals == 0

	var chatItems []string
	if strings.TrimSpace(chatDSNVal) != "" {
		found, err := queryChatActiveWork(ctx, chatDSNVal, svcStartUnix, skipApprovalsQuery)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		chatItems = append(chatItems, found...)
		items = append(items, found...)
	}

	if memTurns > 0 {
		hasTurnItem := false
		for _, it := range chatItems {
			if strings.HasPrefix(it, "chat turn ") {
				hasTurnItem = true
				break
			}
		}
		if !hasTurnItem {
			items = append(items, fmt.Sprintf("%d in-memory chat turn(s) active", memTurns))
		}
	}
	if memApprovals > 0 {
		hasApprovalItem := false
		for _, it := range chatItems {
			if strings.HasPrefix(it, "approval execution ") {
				hasApprovalItem = true
				break
			}
		}
		if !hasApprovalItem {
			items = append(items, fmt.Sprintf("%d in-memory approval execution(s) active", memApprovals))
		}
	}

	return items, firstErr
}

func querySchedActiveWork(ctx context.Context, dsn string, svcStartUnix int64, failClosedUnknown bool) ([]string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	nowUnix := time.Now().Unix()

	var hasTasks bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('tasks') IS NOT NULL").Scan(&hasTasks); err != nil || !hasTasks {
		return nil, err
	}

	rows, err := db.QueryContext(qctx, `
		SELECT id::text,
		       COALESCE(NULLIF(regexp_replace(regexp_replace(title, '[[:cntrl:]]+', ' ', 'g'), '[[:space:]]+', ' ', 'g'), ''), 'untitled'),
		       status,
		       GREATEST(0, $1 - EXTRACT(EPOCH FROM COALESCE(started_at, lease_expires_at - interval '5 minutes', created_at))::bigint)
		  FROM tasks
		 WHERE status IN ('running', 'leased', 'assigned')
		   AND (
		     $3
		     OR (lease_expires_at IS NOT NULL AND lease_expires_at >= to_timestamp($1))
		     OR ($2 > 0 AND COALESCE(started_at, lease_expires_at - interval '5 minutes', created_at) >= to_timestamp($2))
		   )
		 ORDER BY COALESCE(started_at, lease_expires_at - interval '5 minutes', created_at) ASC
		 LIMIT 5`, nowUnix, svcStartUnix, failClosedUnknown)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id, title, status string
		var ageSec int64
		if err := rows.Scan(&id, &title, &status, &ageSec); err != nil {
			return nil, err
		}
		title = strings.TrimSpace(sanitizeSummaryText(title))
		if title == "" {
			title = "untitled"
		}
		out = append(out, fmt.Sprintf("scheduled task %s (%q, %s, %ds)", sanitizeSummaryText(id), title, sanitizeSummaryText(status), ageSec))
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	return out, nil
}

func queryChatActiveWork(ctx context.Context, dsn string, svcStartUnix int64, skipApprovalsQuery bool) ([]string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	nowUnix := time.Now().Unix()
	var out []string

	var hasTurns bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('turns') IS NOT NULL").Scan(&hasTurns); err != nil {
		return nil, err
	}
	if hasTurns {
		rows, err := db.QueryContext(qctx, `
			SELECT turn_id,
			       conversation_id,
			       GREATEST(0, $1 - started_at)
			  FROM turns
			 WHERE status = 'running'
			   AND finished_at IS NULL
			   AND started_at >= $2
			 ORDER BY started_at ASC
			 LIMIT 5`, nowUnix, svcStartUnix)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var turnID, convID string
			var ageSec int64
			if err := rows.Scan(&turnID, &convID, &ageSec); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, fmt.Sprintf("chat turn %s (conv %s, %ds)", sanitizeSummaryText(turnID), sanitizeSummaryText(convID), ageSec))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}

	if skipApprovalsQuery {
		return out, nil
	}

	var approvalsMode string
	if err := db.QueryRowContext(qctx, `
		SELECT CASE
		  WHEN to_regclass('approvals') IS NULL THEN 'none'
		  WHEN EXISTS (
		    SELECT 1
		      FROM pg_attribute
		     WHERE attrelid = to_regclass('approvals')
		       AND attname = 'is_err'
		       AND NOT attisdropped
		  ) THEN 'with_is_err'
		  ELSE 'without_is_err'
		END`).Scan(&approvalsMode); err != nil {
		return out, err
	}
	if approvalsMode != "none" {
		approvalCutoff := max(svcStartUnix, nowUnix-int64(defaultIdleApprovalWindow/time.Second))
		approvalSQL := `
			SELECT id,
			       tool_name,
			       GREATEST(0, $1 - COALESCE(resolved_at, created_at))
			  FROM approvals
			 WHERE status = 'approved'
			   AND is_err IS NULL
			   AND result_text LIKE 'Approved%executing%'
			   AND COALESCE(resolved_at, created_at) >= $2
			 ORDER BY COALESCE(resolved_at, created_at) ASC
			 LIMIT 5`
		if approvalsMode == "without_is_err" {
			approvalSQL = `
				SELECT id,
				       tool_name,
				       GREATEST(0, $1 - COALESCE(resolved_at, created_at))
				  FROM approvals
				 WHERE status = 'approved'
				   AND result_text LIKE 'Approved%executing%'
				   AND COALESCE(resolved_at, created_at) >= $2
				 ORDER BY COALESCE(resolved_at, created_at) ASC
				 LIMIT 5`
		}
		rows, err := db.QueryContext(qctx, approvalSQL, nowUnix, approvalCutoff)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id, toolName string
			var ageSec int64
			if err := rows.Scan(&id, &toolName, &ageSec); err != nil {
				_ = rows.Close()
				return out, err
			}
			out = append(out, fmt.Sprintf("approval execution %s (%s, %ds)", sanitizeSummaryText(id), sanitizeSummaryText(toolName), ageSec))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, err
		}
		_ = rows.Close()
	}

	return out, nil
}

// WaitIdle sets the update drain sentinel file to quiesce new work admission on
// the default service unit, then polls ActiveWork until idle or maxWait elapses.
func WaitIdle(ctx context.Context, schedDSNVal, chatDSNVal string, maxWait, pollInterval time.Duration, out io.Writer) error {
	return WaitIdleForService(ctx, serviceName(""), schedDSNVal, chatDSNVal, maxWait, pollInterval, out)
}

// WaitIdleForService sets and refreshes the update drain sentinel file to
// quiesce new work admission, then polls ActiveWorkForService until no tasks,
// turns, or approval executions are in flight, or until maxWait elapses. When
// work remains in flight at the deadline, it clears the drain sentinel and
// returns *ActiveWorkError. When it succeeds, the drain sentinel remains set
// until the caller clears it after restarting or stopping the service.
func WaitIdleForService(ctx context.Context, unit, schedDSNVal, chatDSNVal string, maxWait, pollInterval time.Duration, out io.Writer) error {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	} else if pollInterval > time.Minute {
		pollInterval = time.Minute
	}
	start := time.Now()
	deadline := start.Add(max(maxWait, 0))
	for {
		if err := SetUpdateDrain(); err != nil {
			return &ActiveWorkError{Items: []string{err.Error()}}
		}
		probeCtx := ctx
		var probeCancel context.CancelFunc
		if maxWait > 0 {
			probeCtx, probeCancel = context.WithDeadline(ctx, deadline)
		}
		items, err := ActiveWorkForService(probeCtx, unit, schedDSNVal, chatDSNVal)
		if probeCancel != nil {
			probeCancel()
		}
		if err != nil {
			items = append(items, fmt.Sprintf("active-work query failed: %v", err))
		}
		if len(items) == 0 {
			if err := SetUpdateDrain(); err != nil {
				return &ActiveWorkError{Items: []string{err.Error()}}
			}
			return nil
		}
		if maxWait <= 0 || !time.Now().Before(deadline) {
			ClearUpdateDrain()
			return &ActiveWorkError{Items: items}
		}
		if out != nil {
			elapsed := int(time.Since(start).Seconds())
			total := int(maxWait.Seconds())
			fmt.Fprintf(out, "waiting for active work to finish (%ds/%ds): %s\n", elapsed, total, strings.Join(items, "; "))
		}
		waitDur := pollInterval
		if rem := time.Until(deadline); rem < waitDur {
			waitDur = max(rem, time.Millisecond)
		}
		timer := time.NewTimer(waitDur)
		select {
		case <-ctx.Done():
			timer.Stop()
			ClearUpdateDrain()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
