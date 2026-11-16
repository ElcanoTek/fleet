package sandbox

import (
	"os"
	"path"
	"runtime"
	"slices"
	"strings"
)

const (
	defaultProcSelfCgroup = "/proc/self/cgroup"
	defaultCgroupV2Root   = "/sys/fs/cgroup"
)

// resolveDelegatedCgroupParent returns the cgroup v2 parent path (for example
// "/system.slice/fleet.service") to pass via `--cgroup-parent` when Fleet runs
// inside a systemd `DelegateSubgroup=` leaf (such as
// "/system.slice/fleet.service/supervisor").
//
// On Linux cgroup v2, `Delegate=yes` alone places the `fleet` daemon PID
// directly inside `/sys/fs/cgroup/system.slice/fleet.service/cgroup.procs`.
// The kernel's no-internal-processes rule then blocks enabling `+memory +cpu
// +pids` in `fleet.service/cgroup.subtree_control` (EBUSY), so rootless Podman
// (`--cgroup-manager=cgroupfs`) drops every sandbox container into
// `fleet.service` itself (`memory.max=max`), leaving `--memory`,
// `--memory-swap`, `--cpus`, and `--pids-limit` unenforced.
//
// With `DelegateSubgroup=supervisor`, systemd places `fleet` in
// `fleet.service/supervisor`, keeping `fleet.service/cgroup.procs` empty.
// Passing `--cgroup-parent=/system.slice/fleet.service` tells `crun` to create
// each container cgroup (`libpod-<cid>`) as a sibling of `supervisor` under the
// delegated `fleet.service` parent, where `crun` can activate subtree
// controllers and enforce per-container limits.
//
// Returns "" outside Linux, outside cgroup v2, when the parent is "/", not
// writable by euid, lacks the memory controller, or still holds internal
// processes (for example an un-migrated unit without `DelegateSubgroup=`), so
// container starts degrade safely instead of failing.
func resolveDelegatedCgroupParent() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	procCgroup, err := os.ReadFile(defaultProcSelfCgroup)
	if err != nil {
		return ""
	}
	return delegatedCgroupParentFrom(procCgroup, defaultCgroupV2Root, os.Geteuid())
}

func delegatedCgroupParentFrom(procCgroup []byte, cgroupRoot string, euid int) string {
	var selfPath string
	for line := range strings.SplitSeq(string(procCgroup), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			selfPath = strings.TrimSpace(rest)
			break
		}
	}
	if !strings.HasPrefix(selfPath, "/") {
		return ""
	}
	cleaned := path.Clean(selfPath)
	if cleaned == "/" || cleaned == "." {
		return ""
	}
	parent := path.Dir(cleaned)
	if parent == "/" || parent == "." || !strings.HasPrefix(parent, "/") {
		return ""
	}
	relParent := strings.TrimPrefix(parent, "/")
	if relParent == "" || strings.Contains(relParent, "..") {
		return ""
	}

	root, err := os.OpenRoot(cgroupRoot)
	if err != nil {
		return ""
	}
	defer root.Close()

	info, err := root.Stat(relParent)
	if err != nil || !info.IsDir() || !dirOwnedAndWritable(info, euid) {
		return ""
	}
	procs, err := root.ReadFile(path.Join(relParent, "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) != "" {
		return ""
	}
	controllers, err := root.ReadFile(path.Join(relParent, "cgroup.controllers"))
	if err != nil || !slices.Contains(strings.Fields(string(controllers)), "memory") {
		return ""
	}
	return parent
}
