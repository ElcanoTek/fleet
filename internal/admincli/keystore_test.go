// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"strings"
	"testing"
)

// TestChooseKeyStore pins the precedence that ends "minted a key into a file
// the service never reads": with a unit installed the service's store is the
// default; an explicit env override still wins but ServiceDir is reported so
// the caller can warn; without a unit the historical ./data stays.
func TestChooseKeyStore(t *testing.T) {
	cases := []struct {
		name          string
		in            keyStoreInputs
		wantDir       string
		wantService   string
		wantMismatch  bool
		wantSourceHas string
	}{
		{
			name:          "unit installed, env file silent → the unit's ./data",
			in:            keyStoreInputs{unitWorkDir: "/var/lib/fleet", cwd: "/root"},
			wantDir:       "/var/lib/fleet/data",
			wantService:   "/var/lib/fleet/data",
			wantSourceHas: "fleet.service",
		},
		{
			name:          "unit installed, env file relative override → relative to the unit, not cwd",
			in:            keyStoreInputs{unitWorkDir: "/var/lib/fleet", envFileDir: "state/keys", cwd: "/root"},
			wantDir:       "/var/lib/fleet/state/keys",
			wantService:   "/var/lib/fleet/state/keys",
			wantSourceHas: "fleet.service",
		},
		{
			name:          "unit installed, env file absolute override",
			in:            keyStoreInputs{unitWorkDir: "/var/lib/fleet", envFileDir: "/srv/fleet-data", cwd: "/root"},
			wantDir:       "/srv/fleet-data",
			wantService:   "/srv/fleet-data",
			wantSourceHas: "fleet.service",
		},
		{
			name:          "explicit env in the shell wins, mismatch reported",
			in:            keyStoreInputs{explicit: "./data", unitWorkDir: "/var/lib/fleet", cwd: "/root"},
			wantDir:       "/root/data",
			wantService:   "/var/lib/fleet/data",
			wantMismatch:  true,
			wantSourceHas: "your environment",
		},
		{
			name:          "explicit env pointing AT the service store is not a mismatch",
			in:            keyStoreInputs{explicit: "/var/lib/fleet/data/", unitWorkDir: "/var/lib/fleet", cwd: "/root"},
			wantDir:       "/var/lib/fleet/data",
			wantService:   "/var/lib/fleet/data",
			wantSourceHas: "your environment",
		},
		{
			name:          "no unit (dev box) → ./data relative to cwd, no service store",
			in:            keyStoreInputs{cwd: "/home/dev/fleet"},
			wantDir:       "/home/dev/fleet/data",
			wantSourceHas: "no fleet.service",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseKeyStore(tc.in)
			if got.Dir != tc.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tc.wantDir)
			}
			if got.ServiceDir != tc.wantService {
				t.Errorf("ServiceDir = %q, want %q", got.ServiceDir, tc.wantService)
			}
			if mismatch := got.ServiceDir != "" && got.ServiceDir != got.Dir; mismatch != tc.wantMismatch {
				t.Errorf("mismatch = %v, want %v", mismatch, tc.wantMismatch)
			}
			if !strings.Contains(got.Source, tc.wantSourceHas) {
				t.Errorf("Source = %q, want it to mention %q", got.Source, tc.wantSourceHas)
			}
			if !strings.HasSuffix(got.Path(), "/api_keys.json") {
				t.Errorf("Path() = %q", got.Path())
			}
		})
	}
}

// TestKeyFormatNoteNamesBothFamilies — the note printed by --help and every
// mint must name the typed prefix, the legacy prefix and the header, because
// "which string am I looking for" is exactly what an operator handing a key to
// a client gets wrong.
func TestKeyFormatNoteNamesBothFamilies(t *testing.T) {
	for _, want := range []string{"fleet_<type>_<base58>", "fleet_task_", "sk-", "X-API-Key"} {
		if !strings.Contains(keyFormatNote, want) {
			t.Errorf("keyFormatNote missing %q", want)
		}
	}
}
