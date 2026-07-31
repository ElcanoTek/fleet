package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// instanceLabelKey is the podman label every sandbox container carries to
// record WHICH fleet process created it (set in start(), container.go). The
// value is thisInstanceLabel — "<pid>@<process start unix seconds>". The pid
// is what PruneOrphanedContainers uses to decide whether the owner is still
// alive; the start time disambiguates pid reuse across boots and makes the
// label unique per process incarnation.
const instanceLabelKey = "fleet.instance"

// thisInstanceLabel identifies the current fleet process for container
// ownership. Computed once at startup so every container this process creates
// carries the same value.
var thisInstanceLabel = fmt.Sprintf("%d@%d", os.Getpid(), time.Now().Unix())

// pidAlive reports whether pid belongs to a currently-running process.
// Signal 0 performs the existence check without delivering anything; EPERM
// still means "exists" (just owned by someone we can't signal). A package
// variable so the prune tests can substitute deterministic liveness.
var pidAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// instanceLabelPID extracts the pid from an instance label value
// ("<pid>@<start>"). ok is false for empty or malformed values.
func instanceLabelPID(label string) (pid int, ok bool) {
	pidStr, _, found := strings.Cut(label, "@")
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// PruneOrphanedContainers removes leftover sandbox containers (the
// containerNamePrefix family) using podmanBinary. It is a best-effort boot-time
// backstop: sandboxes run `podman run --detach --rm ... sleep infinity`, so they
// are owned by conmon and `--rm` only fires when PID 1 exits — which only the
// graceful close path triggers. After a process CRASH that close never runs, so
// every in-flight + warm-pool container is orphaned and keeps consuming host
// RAM/CPU/PIDs across systemd restarts. Calling this on startup (before the pool
// is built) reclaims a prior crash's orphans so a crash-loop cannot accumulate
// them.
//
// The name prefix alone is NOT proof of orphanhood: multiple fleet instances
// can share one podman user, and a blanket `rm -f` on the prefix would kill a
// sibling instance's LIVE containers at every boot. So the sweep is scoped by
// the fleet.instance ownership label (see instanceLabelKey): a RUNNING
// container is removed only when its label names a process that no longer
// exists (a crashed prior run). Running containers whose labeled owner is
// still alive — or that carry no parseable label, so ownership cannot be
// established — are skipped: leaking a container is recoverable, killing a
// live sibling's sandbox mid-turn is not. Exited/created containers are
// always removed regardless of label.
//
// Returns the number of containers removed; never returns an error for the
// "nothing to prune" case.
func PruneOrphanedContainers(ctx context.Context, podmanBinary string) (int, error) {
	if podmanBinary == "" {
		podmanBinary = "podman"
	}
	// "|" is a safe delimiter: ids are hex, states are single words, and the
	// only label value we ever set is "<pid>@<unix seconds>".
	format := `{{.ID}}|{{.State}}|{{index .Labels "` + instanceLabelKey + `"}}`
	out, err := exec.CommandContext(ctx, podmanBinary, "ps", "-a", "--filter", "name="+containerNamePrefix, "--format", format).Output()
	if err != nil {
		return 0, fmt.Errorf("list orphaned sandbox containers: %w", err)
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, rest, _ := strings.Cut(line, "|")
		state, label, _ := strings.Cut(rest, "|")
		if id == "" {
			continue
		}
		if strings.EqualFold(state, "running") {
			pid, ok := instanceLabelPID(label)
			if !ok || pidAlive(pid) {
				// Alive owner, or ownership can't be established (unlabeled /
				// malformed) — skip rather than kill a live sibling's sandbox.
				continue
			}
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if err := exec.CommandContext(ctx, podmanBinary, args...).Run(); err != nil { //nolint:gosec // podmanBinary is operator-configured; args are container IDs from podman's own output
		return 0, fmt.Errorf("remove %d orphaned sandbox container(s): %w", len(ids), err)
	}
	return len(ids), nil
}
