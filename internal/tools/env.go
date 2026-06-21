package tools

import "os"

// fleetEnv reads the canonical FLEET_<name> environment variable,
// falling back to the legacy CHAT_<name> alias for back-compat. During
// the fleet monorepo migration the chat/cutlass tools converged on the
// FLEET_ prefix; the old names keep working so existing deployments and
// .env files aren't broken.
//
// `name` is the suffix after the prefix, e.g. fleetEnv("WORKSPACE_ROOT")
// reads FLEET_WORKSPACE_ROOT then CHAT_WORKSPACE_ROOT.
func fleetEnv(name string) string {
	if v := os.Getenv("FLEET_" + name); v != "" {
		return v
	}
	return os.Getenv("CHAT_" + name)
}
