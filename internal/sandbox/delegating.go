package sandbox

import "context"

// delegatingImpl is the ACP backend: instead of running bash/python locally
// (host or container), it FORWARDS each call to a Delegate that ships the work
// elsewhere — in fleet's ACP topology, over the ACP connection to the client,
// where the REAL host-managed hardened sandbox executes it.
//
// This is the seam that lets cmd/fleet-native-agent run the unified
// agentcore.Run loop with the SAME native bash/run_python tools
// (tools.NewTurnTools(sb)) while having NO local executor of its own: the agent
// process inside the sandbox image cannot self-execute — every bash/python call
// rides the Delegate back to the client (no Podman-in-Podman). Reusing the
// sandbox.impl interface (runBash/runPython/close) keeps the entire downstream
// loop byte-identical to the in-process native path, which is what guarantees
// governance + behavioral parity between native-inprocess and native-acp.
type delegatingImpl struct {
	delegate Delegate
}

// Delegate forwards a sandbox bash/python invocation to a remote executor. The
// native ACP agent supplies an implementation that marshals the request onto an
// ACP `_fleet/tool` extension call; the client side runs it against the real
// host *sandbox.Sandbox and returns the result.
type Delegate interface {
	RunBash(ctx context.Context, req BashRequest) (BashResult, error)
	RunPython(ctx context.Context, req PythonRequest) (PythonResult, error)
}

// NewDelegating wraps a Delegate as a *Sandbox. The returned sandbox owns no
// local process or container — RunBash/RunPython forward to the delegate, and
// Close is a no-op (the delegate's lifecycle is the client's responsibility).
func NewDelegating(d Delegate) *Sandbox {
	return &Sandbox{mode: ModeDelegating, impl: &delegatingImpl{delegate: d}}
}

func (d *delegatingImpl) runBash(ctx context.Context, req BashRequest) (BashResult, error) {
	return d.delegate.RunBash(ctx, req)
}

func (d *delegatingImpl) runPython(ctx context.Context, req PythonRequest) (PythonResult, error) {
	return d.delegate.RunPython(ctx, req)
}

// close is a no-op: the delegate (the ACP connection + the client's host
// sandbox) is torn down by the client, not by this thin forwarder.
func (d *delegatingImpl) close() {}
