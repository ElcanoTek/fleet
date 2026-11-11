//go:build linux

package playwrightrunner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLiveSessionChromiumRendererIsSandboxed checks the browser a live
// session actually launched, not just the launcher script: its processes
// carry no --no-sandbox, and its renderer runs under a seccomp-bpf filter in
// a user or PID namespace of its own, which Chromium's sandbox (namespace or
// setuid flavour) sets up and a --no-sandbox renderer lacks. It skips when
// Playwright or Chromium is not installed, or when the launch error shows the
// sandbox itself could not start on this host; any other launch failure fails
// the test.
func TestLiveSessionChromiumRendererIsSandboxed(t *testing.T) {
	origDial := DialContext
	DialContext = guardedDialContext(func(_ net.IP) bool { return false })
	defer func() { DialContext = origDial }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><title>Sandbox check</title><p>hello</p>`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	mgr := NewSessionManager()
	info, err := mgr.StartSession(ctx, SessionStartRequest{OwnerEmail: "alice@example.com", URL: srv.URL + "/"})
	if err != nil {
		if reason := liveBrowserSkipReason(err); reason != "" {
			t.Skipf("skipping: %s: %v", reason, err)
		}
		t.Fatalf("StartSession: %v", err)
	}
	defer func() { _ = mgr.CloseSession(info.ID, "alice@example.com", "") }()
	sess, err := mgr.GetSession(info.ID, "alice@example.com", "")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.cmd == nil || sess.cmd.Process == nil {
		t.Fatal("live session has no worker process")
	}

	procs := chromiumProcessesUnder(t, sess.cmd.Process.Pid)
	if len(procs) == 0 {
		t.Fatal("found no Chromium processes under the session worker")
	}
	renderers := 0
	for _, p := range procs {
		if strings.Contains(p.cmdline, "--no-sandbox") {
			t.Errorf("Chromium process %d runs with --no-sandbox: %s", p.pid, p.cmdline)
		}
		if !strings.Contains(p.cmdline, "--type=renderer") {
			continue
		}
		renderers++
		if p.seccomp != "2" {
			t.Errorf("renderer %d has Seccomp mode %q, want 2 (a seccomp-bpf filter)", p.pid, p.seccomp)
		}
		if p.userNS == selfNS(t, "user") && p.pidNS == selfNS(t, "pid") {
			t.Errorf("renderer %d shares the test's user and PID namespaces (%s, %s); Chromium's sandbox gives it at least one of its own", p.pid, p.userNS, p.pidNS)
		}
	}
	if renderers == 0 {
		t.Errorf("found no renderer among the session's Chromium processes: %+v", procs)
	}
}

type chromiumProc struct {
	pid     int
	cmdline string
	seccomp string
	userNS  string
	pidNS   string
}

// chromiumProcessesUnder returns every process descended from root whose
// command line names --type= or --remote-debugging-pipe (Chromium's browser,
// zygote, renderer and helper processes). Chromium rewrites child command
// lines into one space-separated string, so matching is on substrings.
func chromiumProcessesUnder(t *testing.T, root int) []chromiumProc {
	t.Helper()
	parent := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// The comm field is parenthesised and may contain spaces; the ppid
		// is the second field after the closing parenthesis.
		s := string(raw)
		fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			parent[pid] = ppid
		}
	}
	descends := func(pid int) bool {
		for seen := 0; pid > 1 && seen < 64; seen++ {
			pid = parent[pid]
			if pid == root {
				return true
			}
		}
		return false
	}
	var out []chromiumProc
	for pid := range parent {
		if !descends(pid) {
			continue
		}
		dir := filepath.Join("/proc", strconv.Itoa(pid))
		raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(raw), "\x00", " ")
		if !strings.Contains(cmdline, "--type=") && !strings.Contains(cmdline, "--remote-debugging-pipe") {
			continue
		}
		p := chromiumProc{pid: pid, cmdline: cmdline}
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			for _, line := range strings.Split(string(status), "\n") {
				if v, ok := strings.CutPrefix(line, "Seccomp:"); ok {
					p.seccomp = strings.TrimSpace(v)
				}
			}
		}
		p.userNS, _ = os.Readlink(filepath.Join(dir, "ns", "user"))
		p.pidNS, _ = os.Readlink(filepath.Join(dir, "ns", "pid"))
		out = append(out, p)
	}
	return out
}

func selfNS(t *testing.T, kind string) string {
	t.Helper()
	ns, err := os.Readlink("/proc/self/ns/" + kind)
	if err != nil {
		t.Fatalf("read own %s namespace: %v", kind, err)
	}
	return ns
}

// liveBrowserSkipReason says why a live browser test should skip rather than
// fail on err: Playwright or Chromium is missing, or Chromium's sandbox
// cannot start on this host (Playwright's launch error carries the browser's
// stderr). Any other launch error is a real failure and returns "".
func liveBrowserSkipReason(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "not available"), strings.Contains(lower, "neither python3 playwright nor node playwright"),
		strings.Contains(lower, "executable doesn't exist"), strings.Contains(lower, "host system is missing dependencies"):
		return "Playwright or Chromium is not installed"
	case strings.Contains(msg, "No usable sandbox"), strings.Contains(msg, "sys_chroot"),
		strings.Contains(msg, "Zygote process exited prematurely"), strings.Contains(lower, "running as root without --no-sandbox"):
		return "Chromium's sandbox cannot start on this host"
	}
	return ""
}
