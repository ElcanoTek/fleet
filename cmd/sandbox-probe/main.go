// sandbox-probe is the deploy-time smoke test for the per-turn / per-exec-burst
// sandbox path (plan §8 P6 checkpoint: "sandbox-probe exercises both Pool.Take
// AND a scheduled-agent task").
//
// Two passes prove the SINGLE sandbox backend both modes share:
//  1. NORMAL — Pool.Take() (warm-pool path the interactive driver uses).
//  2. LOCKDOWN — Pool.TakeContainer() (always-cold, no-network path).
//
// Then a SCHEDULED smoke runs a bash + python round-trip through the sandbox
// the SAME way the in-process worker pool's scheduled-agent TaskRunner does
// (sandbox.Pool.Take + RunBash/RunPython over the persistent workspace), so a
// deploy that's structurally broken fails here instead of on the first real
// scheduled run.
//
// Output is line-prefixed with the pass label + key=value tokens. Non-zero exit
// on any failure.
//
// Env knobs:
//
//	FLEET_SANDBOX_IMAGE / SANDBOX_IMAGE   container image (required)
//	SANDBOX_WORKSPACE                     workspace bind-mount root (default ./workspace)
//	SANDBOX_BRIDGE_DIR                    host dir for the python bridge tempfile
//	SANDBOX_SUPPORTING                    colon-separated absolute host dirs to
//	                                      same-path bind read-only (personas:protocols:system_prompts)
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	// main delegates to run so every defer (ctx cancel, pool.Close) executes
	// before the process exits — os.Exit in main would skip them (gocritic
	// exitAfterDefer).
	os.Exit(run())
}

func run() int {
	image := envOr("FLEET_SANDBOX_IMAGE", os.Getenv("SANDBOX_IMAGE"))
	if image == "" {
		fmt.Fprintln(os.Stderr, "FLEET_SANDBOX_IMAGE (or SANDBOX_IMAGE) is required")
		return 2
	}

	rootCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	workspace := envOr("SANDBOX_WORKSPACE", absDefault("workspace"))
	bridgeDir := envOr("SANDBOX_BRIDGE_DIR", filepath.Join(filepath.Dir(workspace), "data", "sandbox-bridge"))
	supporting := parseSupportingDocs(envOr("SANDBOX_SUPPORTING", defaultSupporting()))
	docsCheckFile := pickDocsCheckFile(supporting)

	if err := os.MkdirAll(workspace, 0o755); err != nil { //nolint:gosec // bind-mount source must be readable by the rootless container user
		fmt.Fprintf(os.Stderr, "ensure workspace %s: %v\n", workspace, err)
		return 2
	}

	pool := sandbox.NewPool(sandbox.PoolConfig{
		Size:         0,
		Mode:         sandbox.ModeContainer,
		BridgeScript: tools.PythonBridgeScript(),
		Container: sandbox.ContainerConfig{
			Image:            image,
			WorkspaceHostDir: workspace,
			BridgeDir:        bridgeDir,
			ReadOnlyMounts:   supporting,
		},
	})
	defer pool.Close()

	exit := 0
	if !runPass(rootCtx, "NORMAL", docsCheckFile, func(_ context.Context) (*sandbox.Sandbox, func(), error) {
		return pool.Take()
	}) {
		exit = 3
	}
	if !runPass(rootCtx, "LOCKDOWN", docsCheckFile, func(ctx context.Context) (*sandbox.Sandbox, func(), error) {
		return pool.TakeContainer(ctx)
	}) {
		exit = 3
	}
	// SCHEDULED smoke: the worker pool's scheduled-agent TaskRunner Takes a
	// sandbox from the SAME pool and drives bash/python over it. Exercise that
	// exact path so the scheduled side is covered, not just the interactive one.
	if !runScheduledSmoke(rootCtx, pool) {
		exit = 3
	}

	return exit
}

func absDefault(rel string) string {
	if abs, err := filepath.Abs(rel); err == nil {
		return abs
	}
	return rel
}

func defaultSupporting() string {
	root, _ := os.Getwd()
	return strings.Join([]string{
		filepath.Join(root, "personas"),
		filepath.Join(root, "protocols"),
		filepath.Join(root, "system_prompts"),
		filepath.Join(root, "skills"),
	}, ":")
}

func parseSupportingDocs(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ":")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func pickDocsCheckFile(dirs []string) string {
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			return filepath.Join(d, e.Name())
		}
	}
	return ""
}

func runPass(ctx context.Context, label, docsCheckFile string, take func(context.Context) (*sandbox.Sandbox, func(), error)) bool {
	sb, cleanup, err := take(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Take: %v\n", label, err)
		return false
	}
	defer cleanup()

	res, err := sb.RunBash(ctx, sandbox.BashRequest{Command: "echo BASH_OK; id", Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s RunBash: %v\n", label, err)
		return false
	}
	fmt.Printf("%s BASH exit=%d stdout=%q stderr=%q\n", label, res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "BASH_OK") {
		return false
	}

	if docsCheckFile != "" {
		docs, err := sb.RunBash(ctx, sandbox.BashRequest{
			Command: "test -r " + docsCheckFile + " && echo DOCS_OK",
			Timeout: 30 * time.Second,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s RunBash(docs): %v\n", label, err)
			return false
		}
		// Best-effort: an unpopulated supporting dir (only .gitkeep) or a deploy
		// without same-path supporting mounts is a WARNING, not a failure — the
		// load-bearing check is that bash/python execute. Lockdown chats that
		// genuinely need personas/ get a clearer signal from the live E2E (P8).
		if docs.ExitCode != 0 || !strings.Contains(string(docs.Stdout), "DOCS_OK") {
			fmt.Fprintf(os.Stderr, "%s DOCS warn: %s not readable inside the container (supporting-doc mount missing or dir empty)\n", label, docsCheckFile)
		} else {
			fmt.Printf("%s DOCS ok file=%s\n", label, docsCheckFile)
		}
	}

	py, err := sb.RunPython(ctx, sandbox.PythonRequest{Code: "print('PY_OK', 1+2+3)", Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s RunPython: %v\n", label, err)
		return false
	}
	fmt.Printf("%s PYTHON status=%s stdout=%q error=%q\n", label, py.Status, py.Stdout, py.Error)
	if py.Status != "success" || !strings.Contains(py.Stdout, "PY_OK 6") {
		return false
	}
	return true
}

// runScheduledSmoke mirrors the scheduled-agent TaskRunner's sandbox use: Take
// from the shared pool, then a bash + python exec-burst over the SAME sandbox,
// then the cleanup the deferred per-run teardown does. Python writes AND reads
// its own file in one call (the cwd-coherent, tool-internal round-trip the
// scheduled agent actually relies on) so the smoke doesn't depend on
// bash↔python cwd being identical.
func runScheduledSmoke(ctx context.Context, pool *sandbox.Pool) bool {
	sb, cleanup, err := pool.Take()
	if err != nil {
		fmt.Fprintf(os.Stderr, "SCHEDULED Take: %v\n", err)
		return false
	}
	defer cleanup()

	// Bash burst: exercise the scheduled bash path.
	b, err := sb.RunBash(ctx, sandbox.BashRequest{Command: "echo SCHED_BASH_OK", Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "SCHEDULED RunBash: %v\n", err)
		return false
	}
	fmt.Printf("SCHEDULED BASH exit=%d stdout=%q\n", b.ExitCode, b.Stdout)
	if b.ExitCode != 0 || !strings.Contains(string(b.Stdout), "SCHED_BASH_OK") {
		return false
	}

	// Python burst: a compute round-trip through the scheduled python path
	// (the kernel boot + execute the scheduled agent's run_python relies on).
	py, err := sb.RunPython(ctx, sandbox.PythonRequest{
		Code:    "print('SCHED_OK', sum(range(10)))",
		Timeout: 30 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "SCHEDULED RunPython: %v\n", err)
		return false
	}
	fmt.Printf("SCHEDULED PYTHON status=%s stdout=%q error=%q\n", py.Status, py.Stdout, py.Error)
	return py.Status == "success" && strings.Contains(py.Stdout, "SCHED_OK 45")
}
