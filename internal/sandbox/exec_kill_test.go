package sandbox

// #796: a cancelled or timed-out bash call must not leave its process tree
// running. These tests exercise the ACTUAL wrapper/killer bash scripts the
// container backend executes — driven on the host (bash + /proc are the only
// requirements, mirroring the sandbox image's bash+coreutils floor) so the
// script logic is verified on every platform, not only where a container
// image exists. The full in-container path is covered by the podman-gated
// tests in cancel_integration_test.go.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// startWrapped launches the bashExecWrapper exactly as the container backend
// does, but on the host: command via FLEET_EXEC_CMD, pidfile as $1, and its
// own process group (podman/crun give the in-container exec its own session;
// Setpgid mirrors that so the killer's group-kill has the same shape — and so
// it cannot take the test binary down with it).
func startWrapped(t *testing.T, pidFile, command string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("bash", "-c", bashExecWrapper, "fleet-exec", pidFile)
	cmd.Env = append(os.Environ(), "FLEET_EXEC_CMD="+command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	return cmd
}

// runKiller executes bashExecKiller against pidFile and returns its exit code.
func runKiller(t *testing.T, pidFile string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), execReapTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", bashExecKiller, "fleet-exec-reap", pidFile)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("killer did not run: %v", err)
	return -1
}

// waitForFile polls until path exists or the deadline passes.
func waitForFile(t *testing.T, path string, deadline time.Duration) {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file %s never appeared", path)
}

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("kill scripts use /proc; linux only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func TestExecKiller_KillsDelayedMarkerWrite(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "exec.pid")
	marker := filepath.Join(dir, "marker")

	cmd := startWrapped(t, pidFile, "sleep 2; touch "+marker)
	go func() { _ = cmd.Wait() }()
	waitForFile(t, pidFile, 2*time.Second)

	if code := runKiller(t, pidFile); code != 0 {
		t.Fatalf("killer exit = %d, want 0 (proved dead)", code)
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Error("killer must remove the pidfile")
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker appeared after the kill — the command survived cancellation")
	}
}

func TestExecKiller_KillsSigtermIgnoringChild(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "exec.pid")
	marker := filepath.Join(dir, "marker")

	cmd := startWrapped(t, pidFile, `trap '' TERM; sleep 2; touch `+marker)
	go func() { _ = cmd.Wait() }()
	waitForFile(t, pidFile, 2*time.Second)

	if code := runKiller(t, pidFile); code != 0 {
		t.Fatalf("killer exit = %d, want 0", code)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("SIGTERM-ignoring command survived — the killer must use SIGKILL")
	}
}

func TestExecKiller_KillsBackgroundedChild(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "exec.pid")
	marker := filepath.Join(dir, "marker")

	// The command backgrounds a subshell and keeps running; both the
	// foreground shell and the backgrounded child must die.
	cmd := startWrapped(t, pidFile, `(sleep 2; touch `+marker+`) & sleep 30`)
	go func() { _ = cmd.Wait() }()
	waitForFile(t, pidFile, 2*time.Second)

	if code := runKiller(t, pidFile); code != 0 {
		t.Fatalf("killer exit = %d, want 0", code)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("backgrounded child survived the kill")
	}
}

func TestExecKiller_FinishedInvocationIsClean(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "exec.pid")

	cmd := startWrapped(t, pidFile, "true")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wrapper: %v", err)
	}
	// Wrapper removed its pidfile on normal completion → exit 3 = clean.
	if code := runKiller(t, pidFile); code != 3 {
		t.Fatalf("killer exit = %d, want 3 (no pidfile: invocation finished)", code)
	}
}

func TestExecWrapper_PreservesExitCode(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "exec.pid")

	cmd := startWrapped(t, pidFile, "exit 7")
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("wrapper exit = %v, want the command's exit code 7", err)
	}
	if _, statErr := os.Stat(pidFile); !os.IsNotExist(statErr) {
		t.Error("wrapper must remove the pidfile on completion")
	}
}

// poisonableImpl is a backend stub whose poisoned() state the test controls,
// so pool retirement semantics are testable without any real backend.
type poisonableImpl struct {
	poison atomic.Bool
	closed atomic.Bool
}

func (p *poisonableImpl) runBash(context.Context, BashRequest) (BashResult, error) {
	return BashResult{}, nil
}
func (p *poisonableImpl) runPython(context.Context, PythonRequest) (PythonResult, error) {
	return PythonResult{Status: "success"}, nil
}
func (p *poisonableImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}
func (p *poisonableImpl) poisoned() bool { return p.poison.Load() }
func (p *poisonableImpl) close()         { p.closed.Store(true) }

func TestSandbox_PoisonedRefusesWork(t *testing.T) {
	pi := &poisonableImpl{}
	sb := &Sandbox{mode: ModeContainer, impl: pi}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Fatalf("healthy sandbox: %v", err)
	}
	pi.poison.Store(true)
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("RunBash on poisoned sandbox = %v, want ErrPoisoned (fail closed)", err)
	}
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "1"}); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("RunPython on poisoned sandbox = %v, want ErrPoisoned", err)
	}
}

func TestTakePersistent_PoisonedEntryIsRetiredAtRelease(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	pi := &poisonableImpl{}
	entry := &persistentEntry{
		sb:     &Sandbox{mode: ModeContainer, impl: pi},
		convID: "conv-poison",
	}
	p.persistentMu.Lock()
	p.persistent["conv-poison"] = entry
	p.persistentMu.Unlock()

	sb, release, err := p.TakePersistent("conv-poison")
	if err != nil {
		t.Fatalf("TakePersistent: %v", err)
	}
	if sb != entry.sb {
		t.Fatal("expected the seeded healthy entry to be reused")
	}
	// The turn's bash gets cancelled and cleanup cannot be proved → poison.
	pi.poison.Store(true)
	release()

	if !pi.closed.Load() {
		t.Fatal("releasing a poisoned persistent sandbox must close it (podman kill stops the stragglers)")
	}
	p.persistentMu.Lock()
	_, still := p.persistent["conv-poison"]
	p.persistentMu.Unlock()
	if still {
		t.Fatal("poisoned entry must be removed from the pool so the next turn gets a fresh sandbox")
	}
}

func TestTakePersistent_PoisonedEntryIsRetiredAtClaim(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	pi := &poisonableImpl{}
	pi.poison.Store(true)
	entry := &persistentEntry{
		sb:     &Sandbox{mode: ModeContainer, impl: pi},
		convID: "conv-poison",
	}
	p.persistentMu.Lock()
	p.persistent["conv-poison"] = entry
	p.persistentMu.Unlock()

	// Claiming a poisoned entry must retire it and build a fresh one (the
	// fresh create goes through the real host backend in this pool config).
	sb, release, err := p.TakePersistent("conv-poison")
	if err != nil {
		t.Fatalf("TakePersistent: %v", err)
	}
	defer release()
	if sb == entry.sb {
		t.Fatal("a poisoned sandbox must never be lent to another turn")
	}
	if !pi.closed.Load() {
		t.Fatal("the poisoned sandbox must be closed on retirement")
	}
}
