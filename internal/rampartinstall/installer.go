// Package rampartinstall is the one-click, web-UI-driven install of the
// Rampart PII detection service (docs/PII-REDACTION.md): the admin clicks
// Install, fleet builds the reference service's container image from the
// build context embedded in the binary (scripts/rampart-service), runs it as
// a podman container pinned to loopback, health-checks it, and writes the
// service URL into the pii_rampart_url workspace setting — after which the
// admin just flips the engine to rampart and clicks Test detection.
//
// fleet already runs rootless podman for every sandbox, so hosting one more
// container is squarely inside its operational model. The container runs with
// --restart=always for daemon-side recovery, and EnsureRunning is called at
// every fleet boot to `podman start` it again after a box reboot (rootless
// restart policies don't survive reboots without systemd) — fleet supervises
// what fleet installed. scripts/rampart-service/install.sh remains the
// systemd-managed alternative for operators who prefer their own unit.
//
// The install is a long job (npm install + the ~15 MB ONNX model download
// happen at image build, so the SERVICE needs no network at runtime): it runs
// async with a polled, key-free status log.
package rampartinstall

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	rampartservice "github.com/ElcanoTek/fleet/scripts/rampart-service"
)

const (
	// ContainerName is the podman container fleet manages.
	ContainerName = "fleet-rampart"
	// ImageRef is the locally-built image tag (same as install.sh's, so the
	// two install paths share a rebuild).
	ImageRef = "localhost/fleet-rampart-service:latest"
	// DefaultPort is the loopback host port the service is published on.
	DefaultPort = 8787
)

// State is the install job's lifecycle.
type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// Status is the admin-facing job snapshot: state, a bounded key-free log, and
// whether the managed container is currently serving.
type Status struct {
	State            State    `json:"state"`
	Log              []string `json:"log"`
	ContainerRunning bool     `json:"container_running"`
	URL              string   `json:"url,omitempty"`
	UpdatedAt        int64    `json:"updated_at"`
}

// runFunc executes one command and returns its combined output — the exec
// seam, injectable in tests.
type runFunc func(ctx context.Context, name string, args ...string) (string, error)

func defaultRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // podman binary + fixed args; nothing user-controlled
	return string(out), err
}

// Installer builds, runs, and supervises the managed service container.
type Installer struct {
	podman string
	port   int
	run    runFunc
	// setURL persists the service URL into the pii_rampart_url workspace
	// setting on success (wired by cmd/fleet to the settings service).
	setURL func(ctx context.Context, url, updatedBy string) error
	client *http.Client
	// healthBudget bounds the post-start health wait (default 90s; small in
	// tests to exercise the never-healthy failure path).
	healthBudget time.Duration

	mu    sync.Mutex
	state State
	log   []string
}

// New builds an Installer. setURL is called with the ready service URL after
// a successful install.
func New(podmanBinary string, setURL func(ctx context.Context, url, updatedBy string) error) *Installer {
	if strings.TrimSpace(podmanBinary) == "" {
		podmanBinary = "podman"
	}
	return &Installer{
		podman:       podmanBinary,
		port:         DefaultPort,
		run:          defaultRun,
		setURL:       setURL,
		client:       &http.Client{Timeout: 3 * time.Second},
		state:        StateIdle,
		healthBudget: 90 * time.Second,
	}
}

// URL is the service endpoint the managed container serves.
func (i *Installer) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/v1/redact", i.port)
}

func (i *Installer) healthURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/healthz", i.port)
}

// Status returns the current job snapshot plus a live container probe.
func (i *Installer) Status(ctx context.Context) Status {
	i.mu.Lock()
	st := Status{State: i.state, Log: append([]string(nil), i.log...), UpdatedAt: time.Now().Unix()}
	i.mu.Unlock()
	st.ContainerRunning = i.containerRunning(ctx)
	if st.ContainerRunning {
		st.URL = i.URL()
	}
	return st
}

// Start kicks the async install. A second Start while one runs is rejected.
// updatedBy is the requesting admin, recorded on the resulting setting write.
func (i *Installer) Start(updatedBy string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state == StateRunning {
		return fmt.Errorf("an install is already running")
	}
	i.state = StateRunning
	i.log = nil
	go i.install(updatedBy)
	return nil
}

// install is the job body. Detached from any request context: the admin can
// close the tab and the install finishes; the panel re-polls status.
func (i *Installer) install(updatedBy string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	err := i.doInstall(ctx, updatedBy)
	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		i.state = StateFailed
		i.log = append(i.log, "FAILED: "+err.Error())
		return
	}
	i.state = StateDone
	i.log = append(i.log, "done — service URL saved; switch the PII detection engine to Rampart and click Test detection")
}

func (i *Installer) doInstall(ctx context.Context, updatedBy string) error {
	i.appendLog("writing the embedded build context")
	dir, err := i.writeBuildContext()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	i.appendLog("building the service image (downloads npm deps + the ~15 MB ONNX model; takes a few minutes)")
	if out, err := i.run(ctx, i.podman, "build", "-t", ImageRef, dir); err != nil {
		return fmt.Errorf("image build failed: %w — %s", err, tail(out, 400))
	}

	i.appendLog("starting the container (loopback only)")
	_, _ = i.run(ctx, i.podman, "rm", "-f", ContainerName) // idempotent
	if out, err := i.run(ctx, i.podman, "run", "-d", "--name", ContainerName,
		"--restart=always",
		"-p", fmt.Sprintf("127.0.0.1:%d:8787", i.port),
		"-e", "RAMPART_ADDR=0.0.0.0:8787",
		ImageRef); err != nil {
		return fmt.Errorf("container start failed: %w — %s", err, tail(out, 400))
	}

	i.appendLog("waiting for the service to answer")
	if err := i.waitHealthy(ctx, i.healthBudget); err != nil {
		return err
	}

	i.appendLog("saving the service URL setting")
	if i.setURL != nil {
		if err := i.setURL(ctx, i.URL(), updatedBy); err != nil {
			return fmt.Errorf("service is running but saving pii_rampart_url failed: %w", err)
		}
	}
	return nil
}

// Uninstall stops and removes the managed container (the image is kept for a
// fast reinstall). It does NOT touch the settings — the caller decides whether
// to reset pii_rampart_url/engine, keeping settings changes on their audited
// path.
func (i *Installer) Uninstall(ctx context.Context) error {
	i.mu.Lock()
	if i.state == StateRunning {
		i.mu.Unlock()
		return fmt.Errorf("an install is running; wait for it to finish")
	}
	i.state = StateIdle
	i.log = nil
	i.mu.Unlock()
	if out, err := i.run(ctx, i.podman, "rm", "-f", ContainerName); err != nil {
		return fmt.Errorf("container remove failed: %w — %s", err, tail(out, 200))
	}
	return nil
}

// EnsureRunning starts the managed container if it exists but is stopped —
// called at fleet boot, because rootless --restart=always does not survive a
// box reboot without systemd. A box without the container (never installed,
// or operator uses install.sh/systemd instead) is a silent no-op.
func (i *Installer) EnsureRunning(ctx context.Context) {
	out, err := i.run(ctx, i.podman, "container", "inspect", "--format", "{{.State.Status}}", ContainerName)
	if err != nil {
		return // no managed container — nothing to supervise
	}
	if strings.TrimSpace(out) == "running" {
		return
	}
	if _, err := i.run(ctx, i.podman, "start", ContainerName); err == nil {
		i.appendLog("restarted the managed service container after fleet boot")
	}
}

// containerRunning reports whether the managed container is up.
func (i *Installer) containerRunning(ctx context.Context) bool {
	out, err := i.run(ctx, i.podman, "container", "inspect", "--format", "{{.State.Status}}", ContainerName)
	return err == nil && strings.TrimSpace(out) == "running"
}

// waitHealthy polls the health endpoint until it answers or the budget ends.
func (i *Installer) waitHealthy(ctx context.Context, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.healthURL(), nil)
		if err != nil {
			return err
		}
		resp, err := i.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("service did not become healthy within %s (podman logs %s)", budget, ContainerName)
}

// writeBuildContext materializes the embedded service files into a temp dir
// for podman build.
func (i *Installer) writeBuildContext() (string, error) {
	dir, err := os.MkdirTemp("", "fleet-rampart-build-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(rampartservice.Files, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(rampartservice.Files, path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, filepath.Base(path)), b, 0o600)
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func (i *Installer) appendLog(line string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.log = append(i.log, line)
	if len(i.log) > 50 {
		i.log = i.log[len(i.log)-50:]
	}
}

// tail clamps command output for an error message (never logs remain key-free
// by construction: the build context and podman args contain no secrets).
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
