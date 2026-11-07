package envreg

import (
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// The DefaultDerive functions of the table: each mirrors, line for line, how
// the setting's reader turns the inherited value into its default (the reader
// is named on each). TestInheritedDefaultsMatchTheReaders (internal/config)
// compares every one against the reader itself.

// drainFileFromEnvFile mirrors httpapi.UpdateDrainFile: <FLEET_ENV_FILE>.update.drain
// when that path is absolute, else the provisioned box's path.
func drainFileFromEnvFile(envFile string) (string, bool) {
	envFile = strings.TrimSpace(envFile)
	if envFile != "" && filepath.IsAbs(envFile) {
		return filepath.Clean(envFile) + ".update.drain", true
	}
	return "", false
}

// auditDirFromDataDir mirrors internal/tools AuditDir: <data dir>/audit.
func auditDirFromDataDir(dataDir string) (string, bool) {
	if dataDir == "" {
		return "", false
	}
	return filepath.Join(dataDir, "audit"), true
}

// oidcCallbackFromOrigin mirrors internal/webui's redirectURI: the public
// origin's /api/auth/oidc/callback. Without a parseable origin the gateway
// derives it from each request instead (not a configured value).
func oidcCallbackFromOrigin(origin string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return u.ResolveReference(&url.URL{Path: "/api/auth/oidc/callback"}).String(), true
}

// warmSizeFromMaxConcurrent mirrors internal/agent warmPoolSize: the agent
// concurrency clamped to 2..8.
func warmSizeFromMaxConcurrent(maxConcurrent string) (string, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(maxConcurrent))
	if err != nil {
		return "", false
	}
	n = min(max(n, 2), 8)
	return strconv.Itoa(n), true
}
