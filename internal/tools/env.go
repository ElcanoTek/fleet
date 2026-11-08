package tools

import "github.com/ElcanoTek/fleet/internal/config/envreg"

// fleetEnv reads the canonical FLEET_<name> environment variable. The
// legacy CHAT_<name> fallback was removed (ADR-0106).
//
// `name` is the suffix after the prefix, e.g. fleetEnv("WORKSPACE_ROOT")
// reads FLEET_WORKSPACE_ROOT.
func fleetEnv(name string) string {
	return envreg.Get("FLEET_" + name)
}
