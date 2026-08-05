package sandbox

import "testing"

// TestContainerAlreadyGone pins the teardown-race classifier shared by close()
// and killContainerNow. The routine case it exists for: a container that
// already exited (a #796 kill or natural exit beat us) whose --rm removal has
// not finished yet — podman's kill then fails with its state error, which the
// old matcher missed, so every such teardown logged a spurious "kill
// unconfirmed" error. It must stay NARROW in the other direction: a paused
// container's processes are frozen, not gone, and a wedged daemon says
// nothing at all.
func TestContainerAlreadyGone(t *testing.T) {
	gone := map[string]string{
		// Verbatim from podman 5.8.2 on this host.
		"removed":     `Error: no container with name or ID "chat-sandbox-doesnotexist" found: no such container`,
		"exited":      `Error: can only kill running containers. f954f5fe4f2b42009a9f8520145fe3ea0c31feb92e0b07d33d200a53ff7f0b81 is in state exited: container state improper`,
		"stopped":     `Error: can only kill running containers. f954f5fe4f2b is in state stopped: container state improper`,
		"legacy form": `Error: container chat-sandbox-abc is not running`,
		// The classifier lower-cases before matching.
		"upper-cased": `ERROR: CAN ONLY KILL RUNNING CONTAINERS. ABC IS IN STATE EXITED: CONTAINER STATE IMPROPER`,
	}
	for name, out := range gone {
		if !containerAlreadyGone(out) {
			t.Errorf("%s: containerAlreadyGone = false, want true — routine teardown of an already-dead container must not log an error (%.80q)", name, out)
		}
	}

	notGone := map[string]string{
		"empty (kill timed out)": "",
		"daemon unreachable":     `Error: cannot connect to the podman socket: no such file or directory`,
		// Frozen, not dead: the state error alone must not qualify.
		"paused": `Error: can only kill running containers. f954f5fe4f2b is in state paused: container state improper`,
	}
	for name, out := range notGone {
		if containerAlreadyGone(out) {
			t.Errorf("%s: containerAlreadyGone = true, want false — this container may still be alive or leaked (%.80q)", name, out)
		}
	}
}
