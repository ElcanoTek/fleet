// Regression tests for #562: an approved bash command must honor the
// conversation's lockdown network seal. Before the fix, runStagedBash always
// pulled a warm network-ENABLED container (Pool.Take), silently dropping the
// --network=none hard seal for lockdown chats — the exact adversarial
// exfiltration case lockdown exists to prevent.

package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

// recordingBashTaker is a fake stagedBashTaker that records which acquisition
// method takeStagedBashSandbox invoked, without spinning a real podman
// container (the same seam pattern as scheduledrun's recordingTaker).
type recordingBashTaker struct {
	took             []string
	takeContainerErr error

	// mode/allowlist are what EgressDefault reports — the fleet-wide
	// FLEET_DEFAULT_NETWORK_MODE the take must honor (ADR-0012/ADR-0031).
	mode      string
	allowlist []string
	// gotAllowlist records the allowlist actually handed to the egress take,
	// so a test can prove the bundle list is threaded through rather than
	// silently dropped.
	gotAllowlist []string
	// takeEgressErr fails the allowlisted cold start.
	takeEgressErr error
}

func (r *recordingBashTaker) Take(context.Context) (*sandbox.Sandbox, func(), error) {
	r.took = append(r.took, "Take")
	return nil, func() {}, nil
}

func (r *recordingBashTaker) TakeContainer(context.Context) (*sandbox.Sandbox, func(), error) {
	r.took = append(r.took, "TakeContainer")
	if r.takeContainerErr != nil {
		return nil, func() {}, r.takeContainerErr
	}
	return nil, func() {}, nil
}

func (r *recordingBashTaker) TakeContainerWithEgress(_ context.Context, _ sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error) {
	r.took = append(r.took, "TakeContainerWithEgress")
	r.gotAllowlist = allowlist
	if r.takeEgressErr != nil {
		return nil, func() {}, r.takeEgressErr
	}
	return nil, func() {}, nil
}

func (r *recordingBashTaker) EgressDefault() (string, []string) {
	return r.mode, r.allowlist
}

// TestTakeStagedBashSandboxMatchesTurnPath asserts the staged-bash take uses
// the SAME pool method — and therefore emits the same podman network arg — as
// the interactive turn path (agent.Manager.takeTurnSandbox) for each lockdown
// state:
//
//   - lockdown → Pool.TakeContainer, which pins NoNetwork=true so networkArgs
//     emits --network=none (the arg itself is pinned by
//     internal/sandbox/network_args_test.go);
//   - non-lockdown → the warm Pool.Take (no --network flag, default rootless
//     slirp4netns egress) — pre-#562 behavior, unchanged.
func TestTakeStagedBashSandboxMatchesTurnPath(t *testing.T) {
	t.Run("lockdown uses the sealed TakeContainer", func(t *testing.T) {
		rt := &recordingBashTaker{}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, true); err != nil {
			t.Fatalf("takeStagedBashSandbox: %v", err)
		}
		if len(rt.took) != 1 || rt.took[0] != "TakeContainer" {
			t.Fatalf("lockdown take = %v, want exactly [TakeContainer] (--network=none, matching takeTurnSandbox)", rt.took)
		}
	})
	t.Run("non-lockdown uses the warm Take", func(t *testing.T) {
		rt := &recordingBashTaker{}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err != nil {
			t.Fatalf("takeStagedBashSandbox: %v", err)
		}
		if len(rt.took) != 1 || rt.took[0] != "Take" {
			t.Fatalf("non-lockdown take = %v, want exactly [Take] (warm pool, matching takeTurnSandbox)", rt.took)
		}
	})
}

// TestTakeStagedBashSandboxHonorsFleetWideMode pins the second posture input:
// FLEET_DEFAULT_NETWORK_MODE (ADR-0012, extended to the chat path by
// ADR-0031). Before this was wired up, an approved command from a
// non-lockdown conversation always took a warm, fully-open container, so a
// fleet-wide `lockdown` deployment still leaked open egress through the
// approval path and `allowlisted` skipped the proxy entirely — while the ADRs
// claimed the setting "genuinely applies fleet-wide".
func TestTakeStagedBashSandboxHonorsFleetWideMode(t *testing.T) {
	t.Run("fleet-wide lockdown seals a non-lockdown conversation", func(t *testing.T) {
		rt := &recordingBashTaker{mode: sandbox.NetworkModeLockdown}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err != nil {
			t.Fatalf("takeStagedBashSandbox: %v", err)
		}
		if len(rt.took) != 1 || rt.took[0] != "TakeContainer" {
			t.Fatalf("fleet-wide lockdown take = %v, want exactly [TakeContainer] (--network=none), matching takeTurnSandboxFrom", rt.took)
		}
	})

	t.Run("fleet-wide allowlisted routes through the egress proxy", func(t *testing.T) {
		rt := &recordingBashTaker{mode: sandbox.NetworkModeAllowlisted, allowlist: []string{"api.example.com"}}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err != nil {
			t.Fatalf("takeStagedBashSandbox: %v", err)
		}
		if len(rt.took) != 1 || rt.took[0] != "TakeContainerWithEgress" {
			t.Fatalf("fleet-wide allowlisted take = %v, want exactly [TakeContainerWithEgress], matching takeTurnSandboxFrom", rt.took)
		}
		if len(rt.gotAllowlist) != 1 || rt.gotAllowlist[0] != "api.example.com" {
			t.Fatalf("allowlist handed to the take = %v, want the bundle allowlist threaded through", rt.gotAllowlist)
		}
	})

	t.Run("open mode keeps the warm Take", func(t *testing.T) {
		for _, mode := range []string{"", sandbox.NetworkModeOpen} {
			rt := &recordingBashTaker{mode: mode}
			if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err != nil {
				t.Fatalf("takeStagedBashSandbox(mode=%q): %v", mode, err)
			}
			if len(rt.took) != 1 || rt.took[0] != "Take" {
				t.Fatalf("mode %q take = %v, want exactly [Take] — open mode behavior is unchanged", mode, rt.took)
			}
		}
	})

	// The seal must be checked BEFORE the fleet-wide switch, under every
	// mode. The allowlisted case is the one that matters: hoisting the switch
	// above the lockdown branch would silently downgrade a sealed
	// conversation from --network=none to proxied-with-network, and a test
	// that only covers open mode would stay green through that refactor.
	t.Run("per-conversation lockdown wins over every fleet-wide mode", func(t *testing.T) {
		for _, mode := range []string{"", sandbox.NetworkModeOpen, sandbox.NetworkModeAllowlisted, sandbox.NetworkModeLockdown} {
			rt := &recordingBashTaker{mode: mode, allowlist: []string{"api.example.com"}}
			if _, _, err := takeStagedBashSandbox(context.Background(), rt, true); err != nil {
				t.Fatalf("takeStagedBashSandbox(mode=%q): %v", mode, err)
			}
			if len(rt.took) != 1 || rt.took[0] != "TakeContainer" {
				t.Fatalf("lockdown conversation on a %q fleet took %v, want [TakeContainer] — the seal must precede the fleet-wide switch", mode, rt.took)
			}
		}
	})
}

// TestTakeStagedBashSandboxFleetWideFailsClosed asserts the fleet-wide
// branches never downgrade to open egress on a real failure. The one
// deliberate degrade is ErrContainerUnavailable — the signal that there is no
// container backend at all (a test/dev ModeHost pool; a release build has no
// host executor to fall back to, #159) — which mirrors
// takeTurnSandboxFrom exactly, so approvals and turns behave identically.
func TestTakeStagedBashSandboxFleetWideFailsClosed(t *testing.T) {
	t.Run("lockdown mode surfaces a real cold-start error", func(t *testing.T) {
		rt := &recordingBashTaker{mode: sandbox.NetworkModeLockdown, takeContainerErr: errors.New("podman exploded")}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err == nil {
			t.Fatal("want the cold-start error surfaced, got nil")
		}
		for _, m := range rt.took {
			if m == "Take" {
				t.Fatalf("fleet-wide lockdown fell back to the open warm Take, took = %v", rt.took)
			}
		}
	})

	t.Run("allowlisted mode surfaces a real cold-start error", func(t *testing.T) {
		rt := &recordingBashTaker{mode: sandbox.NetworkModeAllowlisted, takeEgressErr: errors.New("no egress proxy configured")}
		if _, _, err := takeStagedBashSandbox(context.Background(), rt, false); err == nil {
			t.Fatal("want the cold-start error surfaced, got nil")
		}
		for _, m := range rt.took {
			if m == "Take" {
				t.Fatalf("fleet-wide allowlisted fell back to the unproxied warm Take, took = %v", rt.took)
			}
		}
	})

	t.Run("no container backend degrades to the host take, as the turn path does", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			rt   *recordingBashTaker
			want string
		}{
			{"lockdown", &recordingBashTaker{mode: sandbox.NetworkModeLockdown, takeContainerErr: sandbox.ErrContainerUnavailable}, "TakeContainer"},
			{"allowlisted", &recordingBashTaker{mode: sandbox.NetworkModeAllowlisted, takeEgressErr: sandbox.ErrContainerUnavailable}, "TakeContainerWithEgress"},
		} {
			if _, _, err := takeStagedBashSandbox(context.Background(), tc.rt, false); err != nil {
				t.Fatalf("%s: takeStagedBashSandbox: %v", tc.name, err)
			}
			if len(tc.rt.took) != 2 || tc.rt.took[0] != tc.want || tc.rt.took[1] != "Take" {
				t.Fatalf("%s take = %v, want [%s Take]", tc.name, tc.rt.took, tc.want)
			}
		}
	})
}

// TestTakeStagedBashSandboxLockdownFailsClosed asserts the lockdown take never
// downgrades: when the sealed cold-start fails (here with
// ErrContainerUnavailable, the image-less test/mock pool signal), the error is
// surfaced and the network-enabled warm Take is NOT attempted as a fallback.
func TestTakeStagedBashSandboxLockdownFailsClosed(t *testing.T) {
	rt := &recordingBashTaker{takeContainerErr: sandbox.ErrContainerUnavailable}
	_, _, err := takeStagedBashSandbox(context.Background(), rt, true)
	if !errors.Is(err, sandbox.ErrContainerUnavailable) {
		t.Fatalf("err = %v, want ErrContainerUnavailable surfaced (fail closed)", err)
	}
	for _, m := range rt.took {
		if m == "Take" {
			t.Fatalf("lockdown take fell back to the network-enabled warm Take on error — the seal must fail closed, took = %v", rt.took)
		}
	}
}

// TestStagedBashLockdownResolution pins how the approved-bash path resolves
// the seal: the conversation's own Lockdown flag OR'd with the server-wide
// CHAT_LOCKDOWN_ONLY switch (the same disjunction postChat applies), failing
// CLOSED when the conversation can't be resolved.
func TestStagedBashLockdownResolution(t *testing.T) {
	cases := []struct {
		name         string
		convLockdown bool
		lockdownOnly bool
		missingConv  bool
		want         bool
		wantErr      bool
	}{
		{name: "lockdown conversation", convLockdown: true, want: true},
		{name: "LockdownOnly server seals a non-lockdown conversation", lockdownOnly: true, want: true},
		{name: "neither → warm pool unchanged", want: false},
		{name: "unresolvable conversation fails closed", missingConv: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeChatStore()
			approval := &store.Approval{UserEmail: "u@x.com", ConversationID: "conv-missing", ToolName: "bash"}
			if !tc.missingConv {
				conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "p", "", tc.convLockdown)
				if err != nil {
					t.Fatalf("CreateConversation: %v", err)
				}
				approval.ConversationID = conv.ID
			}
			srv := New(&config.Config{LockdownOnly: tc.lockdownOnly}, &fakeEngine{}, st)
			got, err := srv.stagedBashLockdown(context.Background(), approval)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error (fail closed on unresolvable conversation), got nil")
				}
				if !strings.Contains(err.Error(), "not found") {
					t.Fatalf("err = %v, want the not-found fail-closed error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("stagedBashLockdown: %v", err)
			}
			if got != tc.want {
				t.Fatalf("stagedBashLockdown = %v, want %v", got, tc.want)
			}
		})
	}
}
