// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// keyStore is where `fleet sched apikey …` reads and writes api_keys.json.
//
// The service resolves its store as FLEET_DATA_DIR (else DATA_DIR, else
// ./data) RELATIVE TO ITS WORKING DIRECTORY — /var/lib/fleet under the shipped
// unit, so the live store is /var/lib/fleet/data/api_keys.json. The CLI used to
// resolve the same env vars against ITS cwd and fall back to ./data there, so a
// root shell in /root minted keys into /root/data/api_keys.json — a file the
// service never reads — and the operator saw a 401 indistinguishable from a
// typo'd key. Now the CLI derives the service's store the way the service does
// (the unit's WorkingDirectory + the env FILE's value), says which store it is
// using, and warns loudly when an explicit env override points elsewhere.
type keyStore struct {
	// Dir is the directory holding api_keys.json that the command will use.
	Dir string
	// ServiceDir is the store the systemd unit reads, or "" when it cannot be
	// derived (no unit installed, no systemctl — a dev box).
	ServiceDir string
	// Source names how Dir was chosen, for the operator-facing note.
	Source string
}

// Path is the api_keys.json the command operates on.
func (k keyStore) Path() string { return filepath.Join(k.Dir, "api_keys.json") }

// keyStoreInputs are the environment facts chooseKeyStore decides from; split
// out so the decision is unit-testable without systemd or a filesystem.
type keyStoreInputs struct {
	explicit    string // FLEET_DATA_DIR / DATA_DIR from the CLI's own environment
	envFileDir  string // FLEET_DATA_DIR / DATA_DIR read from the service's env FILE ("" = unset)
	unitWorkDir string // the unit's WorkingDirectory ("" = unit unknown)
	cwd         string
}

// chooseKeyStore applies the precedence: an explicit env override wins (that is
// how a dev box or a second deployment points the CLI somewhere on purpose);
// otherwise the service's own store when it can be derived; otherwise the
// historical ./data relative to cwd. ServiceDir is reported whenever the unit
// is known so the caller can warn about a mismatch.
func chooseKeyStore(in keyStoreInputs) keyStore {
	abs := func(base, p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Clean(filepath.Join(base, p))
	}
	var serviceDir string
	if in.unitWorkDir != "" {
		dd := strings.TrimSpace(in.envFileDir)
		if dd == "" {
			dd = "./data" // config.Load's default, relative to the unit's cwd
		}
		serviceDir = abs(in.unitWorkDir, dd)
	}
	if e := strings.TrimSpace(in.explicit); e != "" {
		return keyStore{Dir: abs(in.cwd, e), ServiceDir: serviceDir, Source: "FLEET_DATA_DIR/DATA_DIR in your environment"}
	}
	if serviceDir != "" {
		return keyStore{Dir: serviceDir, ServiceDir: serviceDir, Source: "the fleet.service store (unit WorkingDirectory + its env file)"}
	}
	return keyStore{Dir: abs(in.cwd, "./data"), Source: "./data relative to the current directory (no fleet.service found)"}
}

// resolveKeyStore gathers the inputs from the live box and decides.
func resolveKeyStore() keyStore {
	in := keyStoreInputs{}
	in.explicit = strings.TrimSpace(os.Getenv("FLEET_DATA_DIR"))
	if in.explicit == "" {
		in.explicit = strings.TrimSpace(os.Getenv("DATA_DIR"))
	}
	if cwd, err := os.Getwd(); err == nil {
		in.cwd = cwd
	} else {
		in.cwd = "."
	}
	in.unitWorkDir = unitProperty(serviceName(""), "WorkingDirectory")
	if in.unitWorkDir != "" {
		// The unit's EnvironmentFile is the same file serverEnvFile resolves
		// (FLEET_ENV_FILE / /etc/fleet/fleet.env). Read the two keys the store
		// hangs on without sourcing it; an unreadable file (not root) just means
		// the default applies, which is also what the unit gets when unset.
		if vals, err := creds.ReadEnvValues(serverEnvFile(""), "FLEET_DATA_DIR", "DATA_DIR"); err == nil {
			in.envFileDir = vals["FLEET_DATA_DIR"]
			if in.envFileDir == "" {
				in.envFileDir = vals["DATA_DIR"]
			}
		}
	}
	return chooseKeyStore(in)
}

// unitProperty returns one `systemctl show` property of <service>.service, or
// "" when systemd or the unit is absent. Bounded so a wedged systemd cannot
// hang a CLI verb.
func unitProperty(service, prop string) string {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unit := service + ".service"
	//nolint:gosec // G204: fixed "systemctl" binary; unit is the configured service name, prop a compile-time constant.
	if err := exec.CommandContext(ctx, "systemctl", "cat", unit).Run(); err != nil {
		return ""
	}
	//nolint:gosec // G204: same fixed binary and operator-configured unit name.
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", prop, "--value", unit).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// announce prints where the command is operating and warns when that is not
// the store the running service reads. Stderr, so `apikey list` piping stays
// clean.
func (k keyStore) announce() {
	fmt.Fprintf(os.Stderr, "key store: %s (%s)\n", k.Path(), k.Source)
	if k.ServiceDir != "" && k.ServiceDir != k.Dir {
		fmt.Fprintf(os.Stderr, "WARNING: this is NOT the fleet.service store (%s) — keys minted here are invisible to the running service (its callers get 401).\n", filepath.Join(k.ServiceDir, "api_keys.json"))
		fmt.Fprintf(os.Stderr, "         unset FLEET_DATA_DIR/DATA_DIR, or set FLEET_DATA_DIR=%s, to write the store the service reads.\n", k.ServiceDir)
	}
}

// fixOwnership hands the store's files back to the store's owner after a
// write. The manager persists via temp+rename, so a root-run mint left
// api_keys.json (and the audit log) owned by root inside a directory the
// service user owns — the unit (User=fleet) then could neither read the new
// key nor persist its own changes, which is the "the key I minted vanished /
// every call 401s" failure. Root-only; a non-root run can only have written
// files it already owns. Best-effort: a failure is reported, never fatal.
func (k keyStore) fixOwnership() {
	if os.Geteuid() != 0 {
		return
	}
	dirInfo, err := os.Stat(k.Dir)
	if err != nil {
		return
	}
	uid, _, ok := creds.FileOwner(dirInfo)
	if !ok || uid == 0 {
		return // root's own store (a dev box), nothing to hand back
	}
	for _, name := range []string{"api_keys.json", "audit_log.jsonl"} {
		p := filepath.Join(k.Dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if cur, _, ok := creds.FileOwner(fi); ok && cur == uid {
			continue
		}
		if err := creds.PreserveOwner(p, dirInfo); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not chown %s to the store owner (uid %d): %v — the service may not be able to read it\n", p, uid, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "chowned %s to uid %d (the store's owner, the service user)\n", p, uid)
	}
}
