// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package config

import (
	"reflect"
	"testing"
)

// TestEnvironmentFilesMembers — every EnvironmentFiles member is kept, whether
// systemctl prints one line per file or several members on one line.
func TestEnvironmentFilesMembers(t *testing.T) {
	cases := map[string][]string{
		"/etc/fleet/fleet.env (ignore_errors=no)":                                          {"/etc/fleet/fleet.env"},
		"/etc/fleet/fleet.env (ignore_errors=no) /etc/fleet/extra.env (ignore_errors=yes)": {"/etc/fleet/fleet.env", "/etc/fleet/extra.env"},
		"-/etc/fleet/opt.env": {"/etc/fleet/opt.env"},
		"":                    nil,
	}
	for in, want := range cases {
		if got := environmentFilesMembers(in); !reflect.DeepEqual(got, want) {
			t.Errorf("environmentFilesMembers(%q) = %q, want %q", in, got, want)
		}
	}

	show := "LoadState=loaded\nEnvironmentFiles=/a.env (ignore_errors=no) /b.env (ignore_errors=yes)\n"
	files := map[string]string{"/a.env": "FLEET_A=1\n", "/b.env": "FLEET_B=2\n"}
	u := UnitEnvFromSystemctl("fleet.service", show, func(p string) ([]byte, error) { return []byte(files[p]), nil })
	if u == nil || u.Env["FLEET_A"] != "1" || u.Env["FLEET_B"] != "2" {
		t.Fatalf("both env files must be read, got %+v", u)
	}
}
