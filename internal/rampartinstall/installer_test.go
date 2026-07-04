package rampartinstall

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Installer job. Load-bearing assertions: the happy path builds, runs,
// health-checks against a real listener, and persists the URL through the
// settings callback; a build failure surfaces in the failed status log;
// concurrent Start is rejected; Uninstall removes the container; and
// EnsureRunning only starts an existing stopped container.

// fakeRunner scripts podman responses per subcommand.
type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error  // subcommand → error
	out   map[string]string // subcommand → stdout
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := args[0]
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if err := f.fail[sub]; err != nil {
		return "podman output", err
	}
	return f.out[sub], nil
}

func (f *fakeRunner) sawPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func waitState(t *testing.T, i *Installer, want State) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := i.Status(context.Background())
		if st.State == want {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("installer never reached state %s (now %s)", want, i.Status(context.Background()).State)
	return Status{}
}

func testInstaller(t *testing.T, fr *fakeRunner) (*Installer, *string) {
	t.Helper()
	// A real listener stands in for the container's health endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	var savedURL string
	i := New("podman", func(_ context.Context, url, updatedBy string) error {
		savedURL = url + "|" + updatedBy
		return nil
	})
	i.run = fr.run
	// Point health/URL at the test listener.
	var port int
	_, _ = fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	i.port = port
	return i, &savedURL
}

func TestInstallHappyPath(t *testing.T) {
	fr := &fakeRunner{fail: map[string]error{}, out: map[string]string{}}
	i, saved := testInstaller(t, fr)

	if err := i.Start("boss@x.com"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A second Start while running is rejected.
	if err := i.Start("boss@x.com"); err == nil {
		t.Fatal("concurrent Start must be rejected")
	}

	st := waitState(t, i, StateDone)
	if !strings.Contains(strings.Join(st.Log, "\n"), "service URL saved") {
		t.Errorf("log = %v", st.Log)
	}
	if !fr.sawPrefix("podman build -t " + ImageRef) {
		t.Error("expected an image build")
	}
	if !fr.sawPrefix("podman run -d --name " + ContainerName) {
		t.Error("expected a container run")
	}
	if !strings.HasPrefix(*saved, i.URL()+"|boss@x.com") {
		t.Errorf("saved URL = %q, want %q by boss@x.com", *saved, i.URL())
	}
}

func TestInstallBuildFailure(t *testing.T) {
	fr := &fakeRunner{fail: map[string]error{"build": fmt.Errorf("no space left")}, out: map[string]string{}}
	i, saved := testInstaller(t, fr)
	if err := i.Start("boss@x.com"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitState(t, i, StateFailed)
	if !strings.Contains(strings.Join(st.Log, "\n"), "image build failed") {
		t.Errorf("failure log = %v", st.Log)
	}
	if *saved != "" {
		t.Error("a failed install must not persist a URL")
	}
	// A failed job can be retried.
	if err := i.Start("boss@x.com"); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	waitState(t, i, StateFailed)
}

// TestInstallHealthTimeout: the image builds and the container "runs" (fake
// podman succeeds) but the service never answers /healthz — the install must
// fail with a clear message and persist no URL, not hang or claim success.
func TestInstallHealthTimeout(t *testing.T) {
	fr := &fakeRunner{fail: map[string]error{}, out: map[string]string{}}
	// A health server that never returns 200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	var saved string
	i := New("podman", func(_ context.Context, url, by string) error { saved = url + "|" + by; return nil })
	i.run = fr.run
	var port int
	_, _ = fmt.Sscanf(srv.URL, "http://127.0.0.1:%d", &port)
	i.port = port
	i.healthBudget = 300 * time.Millisecond // exercise the timeout fast

	if err := i.Start("boss@x.com"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := waitState(t, i, StateFailed)
	if !strings.Contains(strings.Join(st.Log, " "), "did not become healthy") {
		t.Errorf("expected a health-timeout failure, log = %v", st.Log)
	}
	if saved != "" {
		t.Error("an unhealthy install must not persist a URL")
	}
	// The build and run were still attempted.
	if !fr.sawPrefix("podman build") || !fr.sawPrefix("podman run") {
		t.Error("build+run should have been attempted before the health wait")
	}
}

func TestUninstallAndEnsureRunning(t *testing.T) {
	fr := &fakeRunner{fail: map[string]error{}, out: map[string]string{"container": "exited\n"}}
	i, _ := testInstaller(t, fr)

	if err := i.Uninstall(context.Background()); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !fr.sawPrefix("podman rm -f " + ContainerName) {
		t.Error("uninstall must remove the container")
	}

	// EnsureRunning: existing stopped container → podman start.
	i.EnsureRunning(context.Background())
	if !fr.sawPrefix("podman start " + ContainerName) {
		t.Error("EnsureRunning should start a stopped managed container")
	}

	// No container at all → silent no-op (no start call).
	fr2 := &fakeRunner{fail: map[string]error{"container": fmt.Errorf("no such container")}, out: map[string]string{}}
	i2, _ := testInstaller(t, fr2)
	i2.EnsureRunning(context.Background())
	if fr2.sawPrefix("podman start") {
		t.Error("EnsureRunning must not start anything when no managed container exists")
	}
}
