package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultProcSelfCgroup = "/proc/self/cgroup"
	defaultCgroupV2Root   = "/sys/fs/cgroup"
	delegatedSubgroupName = "supervisor"
)

var cgroupSubgroupMu sync.Mutex

// cgroupProcMover moves pid from relSelf/cgroup.procs into subRel/cgroup.procs.
// On real cgroupfs the kernel populates subRel/cgroup.procs automatically on
// mkdir and removes pid from relSelf/cgroup.procs when written to subRel. Unit
// tests on tmpfs override moveFn in delegatedCgroupParentWithMover.
type cgroupProcMover func(root *os.Root, relSelf, subRel, pid string) error

// EnsureDelegatedCgroupSubgroup prepares the cgroup v2 subtree when Fleet runs
// inside a delegated systemd service (`Delegate=yes`) and returns the cgroup
// parent path (for example "/system.slice/fleet.service") to pass via
// `--cgroup-parent`, or "" when not applicable.
//
// On systemd >= 254 (`DelegateSubgroup=supervisor`), systemd places `fleet` in
// `fleet.service/supervisor` at exec time, keeping `fleet.service/cgroup.procs`
// empty so `crun` can enable `+memory +cpu +pids` in `cgroup.subtree_control`.
// On older systemd releases such as RHEL 9 (`systemd 252`), `DelegateSubgroup=`
// is ignored by systemd and `fleet` starts directly inside `fleet.service`;
// this helper creates `fleet.service/supervisor` and moves the service's own
// PIDs into it so `fleet.service/cgroup.procs` is empty on pre-254 hosts too.
func EnsureDelegatedCgroupSubgroup() string {
	return resolveDelegatedCgroupParent()
}

func resolveDelegatedCgroupParent() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	cgroupSubgroupMu.Lock()
	defer cgroupSubgroupMu.Unlock()

	procCgroup, err := os.ReadFile(defaultProcSelfCgroup)
	if err != nil {
		return ""
	}
	return delegatedCgroupParentFrom(procCgroup, defaultCgroupV2Root, os.Geteuid(), os.Getpid())
}

func delegatedCgroupParentFrom(procCgroup []byte, cgroupRoot string, euid, pid int) string {
	return delegatedCgroupParentWithMover(procCgroup, cgroupRoot, euid, pid, writeKernelCgroupProc)
}

func delegatedCgroupParentWithMover(procCgroup []byte, cgroupRoot string, euid, pid int, movePID cgroupProcMover) string {
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
	// Interactive user sessions (/user.slice/...) run under a systemd user D-Bus
	// session where rootless Podman's default systemd cgroup manager handles
	// transient scopes directly; never rewrite user-session cgroups or pass a
	// cgroupfs slash path to Podman's systemd manager.
	if cleaned == "/" || cleaned == "." || cleaned == "/user.slice" || strings.HasPrefix(cleaned, "/user.slice/") {
		return ""
	}

	root, err := os.OpenRoot(cgroupRoot)
	if err != nil {
		return ""
	}
	defer root.Close()

	// Case 1: already inside a subgroup (systemd >= 254 DelegateSubgroup=supervisor
	// or a prior self-evacuation), so path.Dir(cleaned) is the delegated unit root.
	parent := path.Dir(cleaned)
	if parent != "/" && parent != "." && strings.HasPrefix(parent, "/") {
		relParent := strings.TrimPrefix(parent, "/")
		if relParent != "" && !strings.Contains(relParent, "..") && isReadyDelegatedParent(root, relParent, euid) {
			return parent
		}
	}

	// Case 2: pre-systemd-254 host (such as RHEL 9 / systemd 252) or un-migrated
	// unit with Delegate=yes where the daemon started directly in the delegated
	// unit cgroup (for example "/system.slice/fleet.service"). Create the
	// "supervisor" subgroup and move our PIDs into it so the unit root has no
	// internal processes and crun can activate subtree controllers.
	if pid <= 0 || movePID == nil || parent == "/" || parent == "." {
		return ""
	}
	relSelf := strings.TrimPrefix(cleaned, "/")
	if relSelf == "" || strings.Contains(relSelf, "..") {
		return ""
	}
	if evacuateDelegatedUnitToSubgroup(root, relSelf, euid, pid, movePID) {
		return cleaned
	}
	return ""
}

func isReadyDelegatedParent(root *os.Root, relDir string, euid int) bool {
	info, err := root.Stat(relDir)
	if err != nil || !info.IsDir() || !dirOwnedAndWritable(info, euid) {
		return false
	}
	procs, err := root.ReadFile(path.Join(relDir, "cgroup.procs"))
	if err != nil || strings.TrimSpace(string(procs)) != "" {
		return false
	}
	return exposesMemoryController(root, relDir)
}

func exposesMemoryController(root *os.Root, relDir string) bool {
	controllers, err := root.ReadFile(path.Join(relDir, "cgroup.controllers"))
	if err != nil {
		return false
	}
	return slices.Contains(strings.Fields(string(controllers)), "memory")
}

func evacuateDelegatedUnitToSubgroup(root *os.Root, relSelf string, euid, pid int, movePID cgroupProcMover) bool {
	info, err := root.Stat(relSelf)
	if err != nil || !info.IsDir() || !dirOwnedAndWritable(info, euid) {
		return false
	}
	if !exposesMemoryController(root, relSelf) {
		return false
	}
	procsRaw, err := root.ReadFile(path.Join(relSelf, "cgroup.procs"))
	if err != nil {
		return false
	}
	selfPID := strconv.Itoa(pid)
	pids := strings.Fields(string(procsRaw))
	if !slices.Contains(pids, selfPID) {
		return false
	}

	subRel := path.Join(relSelf, delegatedSubgroupName)
	if err := root.Mkdir(subRel, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return false
	}

	// Move our own PID first: on cgroup v2, writing a TGID to cgroup.procs moves
	// all threads of the process atomically, so any subsequent fork/clone from
	// another goroutine inherits the subgroup rather than repopulating relSelf.
	if err := movePID(root, relSelf, subRel, selfPID); err != nil {
		return false
	}

	for range 3 {
		remRaw, err := root.ReadFile(path.Join(relSelf, "cgroup.procs"))
		if err != nil {
			return false
		}
		rem := strings.Fields(string(remRaw))
		if len(rem) == 0 {
			return true
		}
		for _, p := range rem {
			// Best-effort for short-lived child processes that may exit mid-drain;
			// the final cgroup.procs check below is authoritative.
			_ = movePID(root, relSelf, subRel, p)
		}
	}

	after, err := root.ReadFile(path.Join(relSelf, "cgroup.procs"))
	return err == nil && strings.TrimSpace(string(after)) == ""
}

func writeKernelCgroupProc(root *os.Root, _, subRel, pid string) error {
	f, err := root.OpenFile(path.Join(subRel, "cgroup.procs"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(pid + "\n")
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
