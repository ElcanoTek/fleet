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
// is the first thing PruneOrphanedContainers checks; the start time is then
// compared against the live process's actual /proc start time, so a pid that has
// merely been REUSED by an unrelated process cannot make a crashed run's
// container immortal (see labeledOwnerStillRunning). The pair also makes the
// label unique per process incarnation, which is what lets the sweep recognize
// its OWN containers and leave them alone.
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

// procUSERHZ is the unit /proc/<pid>/stat reports `starttime` in. Linux fixes
// USER_HZ at 100 for procfs reporting on every architecture, independently of
// the kernel's internal CONFIG_HZ, so this does not need sysconf (and therefore
// no cgo).
const procUSERHZ = 100

// pidStartedAtUnix returns the wall-clock second at which pid started, derived
// from /proc/<pid>/stat field 22 (start time in USER_HZ ticks since boot) plus
// /proc/stat's `btime` (boot time as a unix second). ok is false when either
// cannot be read or parsed — callers must treat that as "unknown", never as
// "mismatch". A package variable so tests can substitute deterministic values.
var pidStartedAtUnix = func(pid int) (int64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// The comm field is parenthesized and may itself contain spaces and
	// parens, so split AFTER its final ')'. starttime is overall field 22,
	// i.e. index 19 of what follows comm.
	commEnd := strings.LastIndexByte(string(raw), ')')
	if commEnd < 0 || commEnd+2 >= len(raw) {
		return 0, false
	}
	fields := strings.Fields(string(raw[commEnd+2:]))
	if len(fields) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || ticks < 0 {
		return 0, false
	}
	btime, ok := bootTimeUnix()
	if !ok {
		return 0, false
	}
	return btime + ticks/procUSERHZ, true
}

// bootTimeUnix reads the boot time from /proc/stat's `btime` line.
func bootTimeUnix() (int64, bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// pidReuseTolerance bounds how much later than its label a live process may
// have started and still be believed to be the labeling process.
//
// thisInstanceLabel is stamped during package init, a hair AFTER exec, so the
// true owner's /proc start time is always <= its label. A process that merely
// INHERITED the pid started strictly later — after the previous owner died —
// so a generous window still separates them, while leaving no chance of
// mistaking a live sibling for a stale one.
const pidReuseTolerance = 120

// instanceLabelOwner parses an instance label value ("<pid>@<unix start>").
// startedAt is 0 when the label carries no parseable start half — older
// containers, or a truncated value — which callers must treat as "unverifiable"
// rather than "mismatched".
func instanceLabelOwner(label string) (pid int, startedAt int64, ok bool) {
	pidStr, startStr, found := strings.Cut(label, "@")
	if !found {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	startedAt, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil || startedAt <= 0 {
		return pid, 0, true
	}
	return pid, startedAt, true
}

// labeledOwnerStillRunning reports whether the process named by an instance
// label is still the process that created the container.
//
// The pid alone is not enough, and relying on it was a real starvation bug: a
// crashed run's container is skipped forever once its pid is REUSED by any
// unrelated live process, so the orphan the sweep exists to reclaim becomes
// immortal. The label has always carried the owner's start time for exactly this
// purpose — prune.go's own comment said the start time "disambiguates pid reuse"
// — but nothing read it.
//
// Fails SAFE in both unknown directions: an unparseable start half, or a
// /proc read we cannot complete, means "assume still running". Leaking a
// container is recoverable; force-removing a live sibling's sandbox mid-turn is
// not.
func labeledOwnerStillRunning(pid int, labeledStart int64) bool {
	if !pidAlive(pid) {
		return false
	}
	if labeledStart <= 0 {
		return true // label predates start-time verification
	}
	actualStart, ok := pidStartedAtUnix(pid)
	if !ok {
		return true // cannot verify — assume alive
	}
	return actualStart <= labeledStart+pidReuseTolerance
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
// live sibling's sandbox mid-turn is not. Exited/created containers from OTHER
// instances are always removed regardless of their label state.
//
// Containers carrying THIS process's own label are skipped in every state. That
// is load-bearing rather than belt-and-braces: the sweep is called after the
// agent manager is built, and building it constructs the sandbox pool, which
// spawns its warm members from a goroutine — so this process's own containers
// can legitimately be in "created" state while the sweep is listing them.
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
		// NEVER touch a container this process owns, in ANY state. The sweep
		// runs after the warm pool has already begun filling (the pool spawns
		// its members from a goroutine in NewPool), so without this a warm
		// container caught mid-creation — state "created", which the branch
		// below removes regardless of label — would be force-removed by its
		// own process at every boot.
		if label != "" && label == thisInstanceLabel {
			continue
		}
		if strings.EqualFold(state, "running") {
			pid, labeledStart, ok := instanceLabelOwner(label)
			if !ok || labeledOwnerStillRunning(pid, labeledStart) {
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
