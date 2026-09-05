package config

import (
	"os"
	"strings"
)

// The deployment env file — the ONE 0600 file that carries every credential
// and connection string (docs/OPERATORS.md "The env file"). Three places can
// name it, and every verb that reads or writes it must agree on the order, or
// an operator ends up with two half-populated files.
const (
	// SystemEnvDir is the systemd deployment's config directory. Its
	// EXISTENCE is the signal that this is a provisioned box.
	SystemEnvDir = "/etc/fleet"
	// SystemEnvFile is the file deploy/fleet.service EnvironmentFile=s. The
	// unit deliberately UnsetEnvironment=FLEET_ENV_FILE, so on a provisioned
	// box the variable is legitimately absent from every shell and the path
	// has to be inferred from SystemEnvDir.
	SystemEnvFile = SystemEnvDir + "/fleet.env"
	// LocalEnvFile is the dev/source-checkout convention (bootstrap writes it
	// when there is no /etc/fleet).
	LocalEnvFile = ".env.local"
)

// ResolveEnvFile returns the env file the deployment reads: explicit (a
// --env-file flag) when non-empty, else $FLEET_ENV_FILE, else SystemEnvFile
// when SystemEnvDir exists, else LocalEnvFile.
//
// The /etc probe is on the DIRECTORY, not the file: on a provisioned box the
// canonical path is right even before the file has been created (the
// credential writers create it 0600), and falling back to a CWD-relative file
// there would scatter credentials outside the deployment.
//
// This is the single resolver behind the operator CLI's env-file reads and
// writes (`fleet config set-*`, `fleet mcp account`, `fleet env`, `fleet
// status`) AND the in-binary preflight verbs (`fleet validate-config`, `fleet
// eval`, `fleet mcp test`), which used to pass a bare $FLEET_ENV_FILE to Load —
// an empty path that loads nothing, so on a provisioned box they reported a
// healthy deployment's OPENROUTER_API_KEY as missing. `fleet serve` itself is
// NOT routed through it: the unit hands the daemon its environment via
// EnvironmentFile=, and a bare `fleet` in a checkout keeps its documented
// "process env, plus $FLEET_ENV_FILE when set" contract.
func ResolveEnvFile(explicit string) string {
	return resolveEnvFile(explicit, os.Getenv, isDir)
}

// resolveEnvFile is ResolveEnvFile with the environment and the directory
// probe injected, so the /etc/fleet branch is testable off a provisioned box.
func resolveEnvFile(explicit string, getenv func(string) string, isDir func(string) bool) string {
	if v := strings.TrimSpace(explicit); v != "" {
		return v
	}
	if v := strings.TrimSpace(getenv("FLEET_ENV_FILE")); v != "" {
		return v
	}
	if isDir(SystemEnvDir) {
		return SystemEnvFile
	}
	return LocalEnvFile
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
