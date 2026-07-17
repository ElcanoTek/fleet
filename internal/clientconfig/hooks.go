// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package clientconfig

import (
	"fmt"
	"strings"
)

// hooks.go carries the bundle's optional governed lifecycle hooks (#788):
// operator-declared commands that run at fixed agent-run lifecycle points
// (prompt submit, before/after a tool, turn end) INSIDE the per-turn sandbox.
// Like AgentPolicy, this package holds only the plain-data schema + validation;
// cmd/fleet translates it into agentcore.LifecycleHook at startup so the
// clientconfig package stays free of an agentcore import (bundle = data,
// engine = code).

// Hook lifecycle event names. Kept as strings (not an enum) so an unknown value
// is a clear validation error rather than a silent zero.
const (
	HookEventUserPromptSubmit = "user_prompt_submit"
	HookEventPreToolUse       = "pre_tool_use"
	HookEventPostToolUse      = "post_tool_use"
	HookEventTurnEnd          = "turn_end"
)

// hookTimeoutDefaultSeconds / bounds. A hook adds a sandbox exec to the run;
// keep the ceiling modest so a misbehaving hook can't wedge a turn.
const (
	hookTimeoutDefaultSeconds = 30
	hookTimeoutMinSeconds     = 1
	hookTimeoutMaxSeconds     = 120
)

// HookDef is one operator-declared lifecycle hook.
type HookDef struct {
	// ID is a stable, unique, human-readable identifier used in audit events.
	ID string `yaml:"id"`
	// Event is one of the HookEvent* names.
	Event string `yaml:"event"`
	// Matcher selects which tools a pre/post_tool_use hook fires for: "" or "*"
	// = all tools; a trailing "*" is a prefix glob (e.g. "mcp_*"); otherwise an
	// exact tool-name match. Meaningful only for the tool events.
	Matcher string `yaml:"matcher"`
	// Command is the shell command run inside the sandbox. It receives the
	// bounded JSON payload on stdin and prints a JSON decision on stdout.
	Command string `yaml:"command"`
	// TimeoutSeconds bounds the hook; defaults to 30, clamped to [1,120].
	TimeoutSeconds int `yaml:"timeout_seconds"`
	// Enforce makes a hook failure (nonzero exit / timeout / malformed output)
	// BLOCK the operation. Advisory (the default) hooks fail-observable: a
	// failure is audited and the operation proceeds.
	Enforce bool `yaml:"enforce"`
}

// HooksConfig is the manifest `hooks:` block.
type HooksConfig struct {
	// Version pins the hook contract; must be 1 when any entry is present.
	Version int `yaml:"version"`
	// Entries are the declared hooks, executed in listed order per event.
	Entries []HookDef `yaml:"entries"`
}

// EffectiveTimeoutSeconds returns the clamped timeout for a hook.
func (h HookDef) EffectiveTimeoutSeconds() int {
	t := h.TimeoutSeconds
	if t <= 0 {
		return hookTimeoutDefaultSeconds
	}
	if t < hookTimeoutMinSeconds {
		return hookTimeoutMinSeconds
	}
	if t > hookTimeoutMaxSeconds {
		return hookTimeoutMaxSeconds
	}
	return t
}

// validateHooks checks the hooks block. Empty/absent is valid (zero hooks).
func validateHooks(h *HooksConfig) error {
	if h == nil || len(h.Entries) == 0 {
		return nil
	}
	if h.Version != 1 {
		return fmt.Errorf("hooks.version must be 1 (got %d)", h.Version)
	}
	seenID := make(map[string]bool, len(h.Entries))
	seenSig := make(map[string]bool, len(h.Entries))
	for i, e := range h.Entries {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			return fmt.Errorf("hooks.entries[%d]: id is required", i)
		}
		if seenID[id] {
			return fmt.Errorf("hooks.entries[%d]: duplicate id %q", i, id)
		}
		seenID[id] = true

		switch e.Event {
		case HookEventUserPromptSubmit, HookEventPreToolUse, HookEventPostToolUse, HookEventTurnEnd:
		default:
			return fmt.Errorf("hooks.entries[%d] (%s): unknown event %q (want %s, %s, %s, or %s)",
				i, id, e.Event, HookEventUserPromptSubmit, HookEventPreToolUse, HookEventPostToolUse, HookEventTurnEnd)
		}
		if strings.TrimSpace(e.Command) == "" {
			return fmt.Errorf("hooks.entries[%d] (%s): command is required", i, id)
		}
		if strings.TrimSpace(e.Matcher) != "" &&
			e.Event != HookEventPreToolUse && e.Event != HookEventPostToolUse {
			return fmt.Errorf("hooks.entries[%d] (%s): matcher is only valid on %s/%s events",
				i, id, HookEventPreToolUse, HookEventPostToolUse)
		}
		if e.TimeoutSeconds != 0 && (e.TimeoutSeconds < hookTimeoutMinSeconds || e.TimeoutSeconds > hookTimeoutMaxSeconds) {
			return fmt.Errorf("hooks.entries[%d] (%s): timeout_seconds %d out of range [%d,%d]",
				i, id, e.TimeoutSeconds, hookTimeoutMinSeconds, hookTimeoutMaxSeconds)
		}
		sig := e.Event + "\x00" + e.Matcher + "\x00" + e.Command
		if seenSig[sig] {
			return fmt.Errorf("hooks.entries[%d] (%s): exact-duplicate (event,matcher,command) of an earlier entry", i, id)
		}
		seenSig[sig] = true
	}
	return nil
}

// Hooks returns a copy of the bundle's lifecycle-hook entries (nil when none),
// mirroring Bundle.AgentPolicy(): callers get data, not a shared pointer.
func (b *Bundle) Hooks() []HookDef {
	if b == nil || b.HooksConfig == nil {
		return nil
	}
	out := make([]HookDef, len(b.HooksConfig.Entries))
	copy(out, b.HooksConfig.Entries)
	return out
}
