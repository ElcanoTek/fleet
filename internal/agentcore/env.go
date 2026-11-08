package agentcore

import (
	"strconv"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// Env-var prefix handling.
//
// chat and cutlass each baked a compile-time prefix into their env-var
// names (CHAT_* and CUTLASS_*). The unified runtime reads one canonical
// prefix, FLEET_; the CHAT_/CUTLASS_ aliases were removed (ADR-0106).
//
// Every env lookup goes through fleetEnv, which reads FLEET_<suffix> through
// the configuration registry (envreg.Get), so every suffix read here is a
// declared setting. The prefix never varies by run (#1936), so there is no
// per-run configuration to carry.

// CanonicalEnvPrefix is the default env-var prefix for the unified runtime.
const CanonicalEnvPrefix = "FLEET"

// envReader reads agentcore's env knobs. It has no state: it exists so every
// read is spelled fleetEnv.lookup(suffix) — the shape internal/config's knob
// sweep (knobs_sweep_test.go) recognizes as a FLEET_ read.
type envReader struct{}

// fleetEnv is the one envReader.
var fleetEnv envReader

// lookup returns the trimmed value of FLEET_<suffix> ("" when unset).
func (envReader) lookup(suffix string) string {
	return strings.TrimSpace(envreg.Get(CanonicalEnvPrefix + "_" + strings.TrimLeft(suffix, "_")))
}

// Typed knobs read through the two helpers below are ALSO registered as
// scopeExternal rows in internal/config's env-knob registry (#1273), so
// config.Load refuses to boot on a malformed value and `fleet validate-config`
// preflights it. This package keeps its lenient local fallbacks: it holds no
// *Config, runs in embedders and tests that never call config.Load, and must
// not fail a turn over a tuning knob. Note that lookupBool's token set is
// strconv.ParseBool's — narrower than the loader's yes/no/on/off — which is
// exactly why the registry marks those rows kindStrconvBool: it must never
// certify a spelling this reader would resolve to false.

// lookupBool parses the resolved value as a bool (strconv.ParseBool rules).
// Unset / unparseable both report false, matching the original kill-switch
// semantics in both repos.
func (p envReader) lookupBool(suffix string) bool {
	v, _ := strconv.ParseBool(fleetEnv.lookup(suffix))
	return v
}

// lookupBoolDefault is lookupBool for a knob that is on unless turned off:
// def when the var is unset or unparseable, else its strconv.ParseBool value.
// So a typo such as "off" means the default, which for a default-on knob is
// on; config.Load refuses such a value at boot (kindStrconvBool), which is
// where an operator sees it.
func (p envReader) lookupBoolDefault(suffix string, def bool) bool {
	v, err := strconv.ParseBool(fleetEnv.lookup(suffix))
	if err != nil {
		return def
	}
	return v
}

// lookupFloatDefault parses the resolved value as a float64, returning def when
// the var is unset or unparseable. Used for fractional thresholds (e.g. the
// context-pressure ratios) where a bad value must fall back to a safe default
// rather than collapse to zero.
func (p envReader) lookupFloatDefault(suffix string, def float64) float64 {
	raw := fleetEnv.lookup(suffix)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

// hasExplicitFraction reports whether FLEET_<suffix> is explicitly set to a valid fraction in (0,1].
func (p envReader) hasExplicitFraction(suffix string) bool {
	raw := fleetEnv.lookup(suffix)
	if raw == "" {
		return false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return err == nil && v > 0 && v <= 1
}
