package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// PruneOrphanedContainers removes any leftover sandbox containers (the
// containerNamePrefix family) using podmanBinary. It is a best-effort boot-time
// backstop: sandboxes run `podman run --detach --rm ... sleep infinity`, so they
// are owned by conmon and `--rm` only fires when PID 1 exits — which only the
// graceful close path triggers. After a process CRASH that close never runs, so
// every in-flight + warm-pool container is orphaned and keeps consuming host
// RAM/CPU/PIDs across systemd restarts. Calling this on startup (before the pool
// is built) reclaims a prior crash's orphans so a crash-loop cannot accumulate
// them. Returns the number of containers removed; never returns an error for the
// "nothing to prune" case.
func PruneOrphanedContainers(ctx context.Context, podmanBinary string) (int, error) {
	if podmanBinary == "" {
		podmanBinary = "podman"
	}
	out, err := exec.CommandContext(ctx, podmanBinary, "ps", "-aq", "--filter", "name="+containerNamePrefix).Output()
	if err != nil {
		return 0, fmt.Errorf("list orphaned sandbox containers: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if err := exec.CommandContext(ctx, podmanBinary, args...).Run(); err != nil { //nolint:gosec // podmanBinary is operator-configured; args are container IDs from podman's own output
		return 0, fmt.Errorf("remove %d orphaned sandbox container(s): %w", len(ids), err)
	}
	return len(ids), nil
}
