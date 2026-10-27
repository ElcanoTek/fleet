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
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
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
	if truthy(os.Getenv("FLEET_ASSUME_SERVICE_ACTIVE")) {
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
	for _, k := range []string{"FLEET_TLS_MODE", "CHAT_TLS_MODE", "CUTLASS_TLS_MODE"} {
		if v := strings.ToLower(strings.TrimSpace(envOrFile(k))); v != "" {
			return v
		}
	}
	return "off"
}

func localHealthTLSConfig() *tls.Config {
	rootCAs, err := x509.SystemCertPool()
	if err != nil || rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}
	var serverName string
	for _, k := range []string{"FLEET_TLS_DOMAIN", "CHAT_TLS_DOMAIN", "CUTLASS_TLS_DOMAIN"} {
		if v := strings.TrimSpace(envOrFile(k)); v != "" {
			serverName = v
			break
		}
	}
	for _, k := range []string{"FLEET_TLS_CERT_FILE", "CHAT_TLS_CERT_FILE", "CUTLASS_TLS_CERT_FILE"} {
		certFile := strings.TrimSpace(envOrFile(k))
		if certFile == "" {
			continue
		}
		//nolint:gosec // G304: operator-configured TLS certificate path from fleet.env.
		pemBytes, err := os.ReadFile(certFile)
		if err != nil {
			break
		}
		_ = rootCAs.AppendCertsFromPEM(pemBytes)
		if serverName == "" {
			if block, _ := pem.Decode(pemBytes); block != nil {
				if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
					if len(cert.DNSNames) > 0 {
						serverName = cert.DNSNames[0]
					} else if cert.Subject.CommonName != "" {
						serverName = cert.Subject.CommonName
					}
				}
			}
		}
		break
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    rootCAs,
		ServerName: serverName,
	}
}

func probeInMemoryActiveCounts(ctx context.Context, svcState string) (turns, approvals int, hasApprovalsHeader bool) {
	addr := strings.TrimSpace(envOrFile("FLEET_SERVER_ADDR"))
	if addr == "" {
		addr = strings.TrimSpace(envOrFile("CHAT_SERVER_ADDR"))
	}
	if addr == "" {
		addr = strings.TrimSpace(envOrFile("CUTLASS_SERVER_ADDR"))
	}
	if addr == "" && (svcState == "active" || svcState == "unknown") && !truthy(os.Getenv("FLEET_ASSUME_SERVICE_ACTIVE")) {
		addr = "127.0.0.1:8080"
	}
	if addr == "" {
		return 0, 0, false
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if rest, ok := strings.CutPrefix(addr, "0.0.0.0:"); ok {
		addr = "127.0.0.1:" + rest
	} else if rest, ok := strings.CutPrefix(addr, "[::]:"); ok {
		addr = "[::1]:" + rest
	}
	url := addr
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		tlsMode := localHealthTLSMode()
		if tlsMode == "manual" || tlsMode == "auto" {
			url = "https://" + url
		} else {
			url = "http://" + url
		}
	}
	url = strings.TrimRight(url, "/") + "/healthz"

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, false
	}
	tlsCfg := localHealthTLSConfig()
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
		return 0, 0, false
	}
	_ = resp.Body.Close()
	turns, _ = strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Fleet-Active-Turns")))
	rawApprovals := strings.TrimSpace(resp.Header.Get("X-Fleet-Active-Approvals"))
	approvals, aerr := strconv.Atoi(rawApprovals)
	hasApprovalsHeader = rawApprovals != "" && aerr == nil
	return turns, approvals, hasApprovalsHeader
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
	memTurns, memApprovals, hasMemApprovals := probeInMemoryActiveCounts(ctx, svcState)
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

	var hasDatasets bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('datasets') IS NOT NULL").Scan(&hasDatasets); err != nil {
		return out, err
	}
	if hasDatasets {
		drows, err := db.QueryContext(qctx, `
			SELECT id::text,
			       GREATEST(0, $1 - EXTRACT(EPOCH FROM COALESCE(updated_at, created_at))::bigint)
			  FROM datasets
			 WHERE status = 'running'
			   AND ($2 <= 0 OR COALESCE(updated_at, created_at) >= to_timestamp($2))
			 ORDER BY COALESCE(updated_at, created_at) ASC
			 LIMIT 5`, nowUnix, svcStartUnix)
		if err != nil {
			return out, err
		}
		for drows.Next() {
			var id string
			var ageSec int64
			if err := drows.Scan(&id, &ageSec); err != nil {
				_ = drows.Close()
				return out, err
			}
			out = append(out, fmt.Sprintf("dataset %s (running, %ds)", sanitizeSummaryText(id), ageSec))
		}
		if err := drows.Err(); err != nil {
			_ = drows.Close()
			return out, err
		}
		_ = drows.Close()
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
