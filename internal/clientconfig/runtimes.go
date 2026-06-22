package clientconfig

import (
	"fmt"
	"strings"
)

// Runtime flavors. fleet drives each flavor as an ACP agent (Plan v4); the
// native-inprocess flavor is the fast in-process path kept as the default + the
// parity oracle.
const (
	// RuntimeNativeInprocess runs today's direct in-process agentcore.Run loop.
	// The fast path (dev/test/trusted-local) and the parity oracle.
	RuntimeNativeInprocess = "native-inprocess"
	// RuntimeNativeACP runs fleet's native fantasy loop wrapped AS a sandboxed
	// ACP agent (cmd/fleet-native-agent), fully governed by the host client. The
	// recommended sandboxed production flavor.
	RuntimeNativeACP = "native-acp"
	// RuntimeACP drives an EXTERNAL ACP agent (Claude Code / Goose) — the shape
	// is declared here in P-ACP-1; the external flavor is wired in P-ACP-2.
	RuntimeACP = "acp"
)

// RuntimeType is the kind of runtime a flavor uses.
type RuntimeType string

const (
	RuntimeTypeNativeInprocess RuntimeType = "native-inprocess"
	RuntimeTypeNativeACP       RuntimeType = "native-acp"
	RuntimeTypeACP             RuntimeType = "acp"
)

// RuntimeNetwork is the egress posture for a flavor's agent container.
type RuntimeNetwork string

const (
	// RuntimeNetworkRestricted leaves the default rootless egress in place (the
	// native flavor needs model-endpoint egress; the loop runs in the agent).
	RuntimeNetworkRestricted RuntimeNetwork = "restricted"
	// RuntimeNetworkNone seals the agent container's network namespace.
	RuntimeNetworkNone RuntimeNetwork = "none"
	// RuntimeNetworkModelOnly restricts egress to model endpoints (external
	// self-executing flavors; enforced in P-ACP-2).
	RuntimeNetworkModelOnly RuntimeNetwork = "model_only"
)

// Runtime is one runtime flavor declared in the manifest's `runtimes:` block.
type Runtime struct {
	// Name is the flavor key (e.g. "native-acp"). Set from the map key.
	Name string `yaml:"-"`
	// Type selects the driver (native-inprocess | native-acp | acp).
	Type RuntimeType `yaml:"type"`
	// Image is the agent container image ref (native-acp / acp). Digest-pinned
	// in production. Resolves ${VAR} via the manifest interpolation pass.
	Image string `yaml:"image"`
	// Network is the agent container's egress posture.
	Network RuntimeNetwork `yaml:"network"`
	// DelegatedPolicy stamps governance: delegated for self-executing external
	// flavors (containment tier, not full governance). Native flavors leave it
	// false (fully governed).
	DelegatedPolicy bool `yaml:"delegated_policy"`
	// DisplayName / Description drive the flavor-picker UI; Beta marks it.
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description"`
	Beta        bool   `yaml:"beta"`
	// Default marks the flavor selected when none is chosen. Exactly one flavor
	// may be the default; when none is marked the loader defaults to
	// native-inprocess.
	Default bool `yaml:"default"`
}

// runtimesManifest is the on-disk YAML shape of the `runtimes:` block: a map of
// flavor name → descriptor.
type runtimesManifest map[string]Runtime

// defaultRuntimes is the built-in flavor set used when the manifest declares no
// `runtimes:` block: native-inprocess (default + parity oracle) and native-acp
// (sandboxed, image resolved from the sandbox/native-agent lineage). A bundle
// overrides this with its own block.
func defaultRuntimes() []Runtime {
	return []Runtime{
		{
			Name: RuntimeNativeInprocess, Type: RuntimeTypeNativeInprocess,
			DisplayName: "Native (in-process)", Default: true,
			Description: "Fast in-process loop. Dev/test/trusted-local; the parity oracle.",
		},
		{
			Name: RuntimeNativeACP, Type: RuntimeTypeNativeACP,
			Image: "localhost/fleet-native-agent:latest", Network: RuntimeNetworkRestricted,
			DisplayName: "Native (sandboxed)",
			Description: "Native loop wrapped as a sandboxed ACP agent. Fully governed.",
		},
	}
}

// resolveRuntimes turns the manifest's runtimes block into an ordered, validated
// slice. A nil/empty block yields defaultRuntimes(). The order is deterministic
// (native-inprocess, native-acp, acp, then the rest alphabetically) so the
// picker renders stably.
func resolveRuntimes(rm runtimesManifest) ([]Runtime, error) {
	if len(rm) == 0 {
		return defaultRuntimes(), nil
	}
	out := make([]Runtime, 0, len(rm))
	for name, rt := range rm {
		rt.Name = name
		if rt.Type == "" {
			// Infer from the well-known names so a terse block still validates.
			switch name {
			case RuntimeNativeInprocess:
				rt.Type = RuntimeTypeNativeInprocess
			case RuntimeNativeACP:
				rt.Type = RuntimeTypeNativeACP
			default:
				rt.Type = RuntimeTypeACP
			}
		}
		out = append(out, rt)
	}
	sortRuntimes(out)
	if err := validateRuntimes(out); err != nil {
		return nil, err
	}
	return out, nil
}

// sortRuntimes orders the canonical flavors first, then the rest alphabetically.
func sortRuntimes(rts []Runtime) {
	rank := func(name string) int {
		switch name {
		case RuntimeNativeInprocess:
			return 0
		case RuntimeNativeACP:
			return 1
		case RuntimeACP:
			return 2
		default:
			return 3
		}
	}
	// Simple insertion sort — the list is tiny.
	for i := 1; i < len(rts); i++ {
		for j := i; j > 0; j-- {
			a, b := rts[j-1], rts[j]
			if rank(a.Name) > rank(b.Name) || (rank(a.Name) == rank(b.Name) && a.Name > b.Name) {
				rts[j-1], rts[j] = b, a
			} else {
				break
			}
		}
	}
}

// validateRuntimes enforces the structural invariants: known types, an image for
// the container-backed flavors, and at most one default.
func validateRuntimes(rts []Runtime) error {
	defaults := 0
	for _, rt := range rts {
		switch rt.Type {
		case RuntimeTypeNativeInprocess:
			// no image needed
		case RuntimeTypeNativeACP, RuntimeTypeACP:
			if strings.TrimSpace(rt.Image) == "" {
				return fmt.Errorf("runtimes[%q]: type %q requires an image", rt.Name, rt.Type)
			}
		default:
			return fmt.Errorf("runtimes[%q]: unknown type %q (want native-inprocess|native-acp|acp)", rt.Name, rt.Type)
		}
		if rt.Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("runtimes: at most one flavor may be the default (got %d)", defaults)
	}
	return nil
}

// Runtimes returns the bundle's runtime flavors (defensively copied).
func (b *Bundle) Runtimes() []Runtime {
	out := make([]Runtime, len(b.RuntimesConfig))
	copy(out, b.RuntimesConfig)
	return out
}

// DefaultRuntime returns the name of the default flavor: the one marked default,
// else native-inprocess if present, else the first flavor, else
// native-inprocess.
func (b *Bundle) DefaultRuntime() string {
	for _, rt := range b.RuntimesConfig {
		if rt.Default {
			return rt.Name
		}
	}
	for _, rt := range b.RuntimesConfig {
		if rt.Name == RuntimeNativeInprocess {
			return rt.Name
		}
	}
	if len(b.RuntimesConfig) > 0 {
		return b.RuntimesConfig[0].Name
	}
	return RuntimeNativeInprocess
}

// Runtime returns the flavor descriptor by name and whether it exists.
func (b *Bundle) Runtime(name string) (Runtime, bool) {
	for _, rt := range b.RuntimesConfig {
		if rt.Name == name {
			return rt, true
		}
	}
	return Runtime{}, false
}
