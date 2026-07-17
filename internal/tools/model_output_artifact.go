package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

var (
	// ErrModelOutputArtifactScope means the run did not install a confined,
	// sandbox-backed artifact scope. Fleet fails closed instead of falling back
	// to host /tmp or a process-global workspace.
	ErrModelOutputArtifactScope = errors.New("no sandbox-backed model-output artifact scope")
	// ErrModelOutputArtifactTooLarge means retention was deliberately refused to
	// preserve the documented workspace disk bound.
	ErrModelOutputArtifactTooLarge = errors.New("model-output artifact exceeds retention limit")
	// ErrModelOutputArtifactCapacity means every immutable recovery slot in this
	// workspace has already been handed out. Fleet never wraps a live/stale path
	// to different content; later results remain bounded but have no artifact.
	ErrModelOutputArtifactCapacity = errors.New("model-output artifact capacity exhausted")
)

const (
	modelOutputArtifactDir      = ".fleet/tool-output"
	modelOutputArtifactIgnore   = ".fleet/tool-output/.gitignore"
	modelOutputArtifactState    = ".fleet/tool-output/.next-slot"
	modelOutputArtifactSlots    = 16
	modelOutputArtifactMaxBytes = 8 * 1024 * 1024
)

// ModelOutputArtifactStager is the narrow storage seam used by agentcore's
// final model-visible tool-output boundary. content has already passed the
// applicable secret/PII/guardrail governance before this method is called.
// Implementations return a workspace-relative path that the model can pass to
// view_file; they must never return a host-only path.
type ModelOutputArtifactStager interface {
	StageModelOutputArtifact(ctx context.Context, toolName, toolCallID, format, content string) (string, error)
}

type modelOutputArtifactStagerKey struct{}

// WithModelOutputArtifactStager installs a per-run artifact writer. This seam
// remains public for focused tests and alternate confined backends.
func WithModelOutputArtifactStager(ctx context.Context, stager ModelOutputArtifactStager) context.Context {
	if stager == nil {
		return ctx
	}
	return context.WithValue(ctx, modelOutputArtifactStagerKey{}, stager)
}

// artifactRootState coordinates overlapping runs that intentionally share one
// private workspace root (for example two turns in one conversation). Holding
// mu through the write prevents two concurrent artifacts from selecting the
// same immutable slot.
type artifactRootState struct {
	mu          sync.Mutex
	next        uint64
	initialized bool
	refs        int
}

var artifactRootRegistry = struct {
	sync.Mutex
	roots map[string]*artifactRootState
}{roots: make(map[string]*artifactRootState)}

// WithSandboxModelOutputArtifacts installs the production artifact stager for
// one run. Every retained byte is written through Sandbox.RunFileOp, never
// through host filesystem APIs. release must be deferred for the run lifetime;
// it keeps the shared-root coordination registry bounded as workspaces churn.
func WithSandboxModelOutputArtifacts(ctx context.Context, sb *sandbox.Sandbox, workspaceRoot string) (context.Context, func(), error) {
	if sb == nil {
		return ctx, func() {}, fmt.Errorf("install model-output artifact scope: nil sandbox")
	}
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return ctx, func() {}, ErrModelOutputArtifactScope
	}
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("resolve model-output artifact root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	// Bind before registering the stager so every direct/test caller gets the
	// same in-runtime root-identity protection as the production drivers. The
	// operation is idempotent when the driver already bound this exact root.
	if err := sb.BindFileOpRoot(ctx, absRoot); err != nil {
		return ctx, func() {}, fmt.Errorf("bind model-output artifact root: %w", err)
	}

	artifactRootRegistry.Lock()
	state := artifactRootRegistry.roots[absRoot]
	if state == nil {
		state = &artifactRootState{}
		artifactRootRegistry.roots[absRoot] = state
	}
	state.refs++
	artifactRootRegistry.Unlock()

	var once sync.Once
	release := func() {
		once.Do(func() {
			artifactRootRegistry.Lock()
			defer artifactRootRegistry.Unlock()
			current := artifactRootRegistry.roots[absRoot]
			if current != state {
				return
			}
			state.refs--
			if state.refs == 0 {
				delete(artifactRootRegistry.roots, absRoot)
			}
		})
	}

	stager := &sandboxModelOutputArtifactStager{sandbox: sb, root: absRoot, state: state}
	return WithModelOutputArtifactStager(ctx, stager), release, nil
}

// StageModelOutputArtifact stores governed full output in the run's artifact
// scope. There is deliberately no host-side default: drivers must install the
// sandbox-backed scope after acquiring their per-run sandbox.
func StageModelOutputArtifact(ctx context.Context, toolName, toolCallID, format, content string) (string, error) {
	stager, ok := ctx.Value(modelOutputArtifactStagerKey{}).(ModelOutputArtifactStager)
	if !ok || stager == nil {
		return "", ErrModelOutputArtifactScope
	}
	return stager.StageModelOutputArtifact(ctx, toolName, toolCallID, format, content)
}

type sandboxModelOutputArtifactStager struct {
	sandbox *sandbox.Sandbox
	root    string
	state   *artifactRootState
}

func (s *sandboxModelOutputArtifactStager) StageModelOutputArtifact(ctx context.Context, _, _, _, content string) (string, error) {
	if len(content) > modelOutputArtifactMaxBytes {
		return "", fmt.Errorf("%w: %d bytes exceeds %d", ErrModelOutputArtifactTooLarge, len(content), modelOutputArtifactMaxBytes)
	}
	if s == nil || s.sandbox == nil || s.state == nil || s.root == "" {
		return "", ErrModelOutputArtifactScope
	}

	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if !s.state.initialized {
		// Keep generated recovery bytes out of `git add -A` in any consumer
		// repository without mutating host Git metadata. The ignore file itself is
		// written through the same confined sandbox seam and is idempotently
		// replaced with fixed Fleet-owned content.
		ignorePath := filepath.Join(s.root, filepath.FromSlash(modelOutputArtifactIgnore))
		if _, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
			Op:   sandbox.FileOpWrite,
			Path: ignorePath,
			Root: s.root,
			Data: []byte("*\n"),
		}); err != nil {
			return "", fmt.Errorf("write model-output artifact git exclusion through sandbox: %w", err)
		}
		statePath := filepath.Join(s.root, filepath.FromSlash(modelOutputArtifactState))
		result, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
			Op:    sandbox.FileOpRead,
			Path:  statePath,
			Root:  s.root,
			Limit: 64,
		})
		if err != nil && !errors.Is(err, sandbox.ErrFileOpNotFound) {
			return "", fmt.Errorf("read model-output artifact capacity cursor through sandbox: %w", err)
		}
		if err == nil {
			if next, parseErr := strconv.ParseUint(strings.TrimSpace(string(result.Data)), 10, 64); parseErr == nil {
				s.state.next = next
			}
		}
		s.state.initialized = true
	}
	if s.state.next >= modelOutputArtifactSlots {
		return "", ErrModelOutputArtifactCapacity
	}
	// Treat the cursor as an optimization, not as authority. A prior crash or a
	// workspace tool can delete/reset/corrupt it; never let that make an already
	// advertised path alias new content. Probe fixed per-slot directories and
	// skip every issued slot before selecting one. The directory is the durable
	// tombstone: it cannot be removed while leaving its advertised child artifact
	// behind, so allocator resets cannot accumulate more than 16 retained files.
	// The advertised path also includes a full content digest, so deleting the
	// whole directory still cannot make different bytes reuse a historical path.
	for s.state.next < modelOutputArtifactSlots {
		slotDir := modelOutputArtifactSlotDir(s.state.next)
		abs := filepath.Join(s.root, filepath.FromSlash(slotDir))
		_, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
			Op:    sandbox.FileOpRead,
			Path:  abs,
			Root:  s.root,
			Limit: 1,
		})
		if errors.Is(err, sandbox.ErrFileOpNotFound) {
			break
		}
		// A directory is the expected used-slot result. A regular file at the
		// reserved name is also treated as consumed; symlinks and other unsafe
		// traversal errors fail the entire retention attempt closed.
		if err != nil && !errors.Is(err, sandbox.ErrFileOpIsDirectory) {
			return "", fmt.Errorf("probe model-output artifact slot through sandbox: %w", err)
		}
		s.state.next++
	}
	if s.state.next >= modelOutputArtifactSlots {
		return "", ErrModelOutputArtifactCapacity
	}
	slot := s.state.next
	next := s.state.next + 1
	statePath := filepath.Join(s.root, filepath.FromSlash(modelOutputArtifactState))
	if _, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: statePath,
		Root: s.root,
		Data: []byte(strconv.FormatUint(next, 10)),
	}); err != nil {
		return "", fmt.Errorf("advance model-output artifact capacity cursor through sandbox: %w", err)
	}
	s.state.next = next
	rel := modelOutputArtifactPath(slot, content)
	marker := modelOutputArtifactSlotMarker(slot)
	markerAbs := filepath.Join(s.root, filepath.FromSlash(marker))
	if _, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: markerAbs,
		Root: s.root,
		Data: []byte(filepath.Base(rel) + "\n"),
	}); err != nil {
		return "", fmt.Errorf("write model-output artifact slot tombstone through sandbox: %w", err)
	}
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if _, err := s.sandbox.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: abs,
		Root: s.root,
		// Conversion happens only after the 8 MiB preflight above, preventing a
		// rejected giant model string from being copied just to discover it is
		// outside the bounded retention contract.
		Data: []byte(content),
	}); err != nil {
		return "", fmt.Errorf("write governed model-output artifact through sandbox: %w", err)
	}
	return rel, nil
}

func modelOutputArtifactPath(slot uint64, content string) string {
	digest := sha256.Sum256([]byte(content))
	return filepath.ToSlash(filepath.Join(modelOutputArtifactSlotDir(slot),
		fmt.Sprintf("artifact-%x.txt", digest)))
}

func modelOutputArtifactSlotDir(slot uint64) string {
	return filepath.ToSlash(filepath.Join(modelOutputArtifactDir, fmt.Sprintf("slot-%02d", slot)))
}

func modelOutputArtifactSlotMarker(slot uint64) string {
	return filepath.ToSlash(filepath.Join(modelOutputArtifactSlotDir(slot), ".used"))
}
