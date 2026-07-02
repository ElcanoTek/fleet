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
}

func (r *recordingBashTaker) Take() (*sandbox.Sandbox, func(), error) {
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
