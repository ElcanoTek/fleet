// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

// TestPodmanExecArgv pins the exact argv each execution shape execs, so a
// prefix-free context is guaranteed to be the historical plain-podman shape —
// the boot path (agent.buildSandboxPool) depends on that being byte-identical.
func TestPodmanExecArgv(t *testing.T) {
	cases := []struct {
		name string
		exec PodmanExec
		bin  string
		args []string
		want []string
	}{
		{
			name: "zero value is plain podman (the boot path)",
			exec: PodmanExec{},
			args: []string{"info"},
			want: []string{"podman", "info"},
		},
		{
			name: "configured binary is used verbatim",
			exec: PodmanExec{Binary: "/usr/bin/podman"},
			args: []string{"--runtime=krun", "info", "--format", "{{.Host.OCIRuntime.Path}}"},
			want: []string{"/usr/bin/podman", "--runtime=krun", "info", "--format", "{{.Host.OCIRuntime.Path}}"},
		},
		{
			name: "binary override wins over the configured one",
			exec: PodmanExec{Binary: "/usr/bin/podman"},
			bin:  "test",
			args: []string{"-rw", "/dev/kvm"},
			want: []string{"test", "-rw", "/dev/kvm"},
		},
		{
			name: "service-user prefix runs ahead of podman (validate-config as root)",
			exec: PodmanExec{
				Binary: "podman",
				Prefix: []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet"},
				Dir:    "/var/lib/fleet",
			},
			args: []string{"info"},
			want: []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet", "podman", "info"},
		},
		{
			name: "the KVM probe execs test, not podman, through the same prefix",
			exec: PodmanExec{
				Prefix: []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet"},
				Dir:    "/var/lib/fleet",
			},
			bin:  "test",
			args: []string{"-r", "/dev/kvm", "-a", "-w", "/dev/kvm"},
			want: []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet", "test", "-r", "/dev/kvm", "-a", "-w", "/dev/kvm"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.exec.argv(tc.bin, tc.args...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestServiceStorePodmanExec is the same contract the admincli status probes
// pin through ServiceStorePodmanArgv (which now delegates here): the runuser
// shape, the whose-store note, and the Dir rule — Dir follows the home ONLY
// when it is known, matching the status probe, which inherits otherwise.
func TestServiceStorePodmanExec(t *testing.T) {
	cases := []struct {
		name         string
		svcUser      string
		svcHome      string
		asRoot       bool
		wantPrefix   []string
		wantDir      string
		wantContains string
	}{
		{
			name:    "root with a non-root service user hops to that user",
			svcUser: "fleet", svcHome: "/var/lib/fleet", asRoot: true,
			wantPrefix:   []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet"},
			wantDir:      "/var/lib/fleet",
			wantContains: "as fleet — the service's image store",
		},
		{
			name:    "unknown home falls back for HOME but inherits the cwd for Dir",
			svcUser: "fleet", svcHome: "", asRoot: true,
			wantPrefix:   []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet"},
			wantDir:      "",
			wantContains: "as fleet — the service's image store",
		},
		{
			name:    "root service user probes root's store plainly",
			svcUser: "root", svcHome: "", asRoot: true,
			wantPrefix:   nil,
			wantDir:      "",
			wantContains: "root's store",
		},
		{
			name:    "non-root caller probes their own store with a warning note",
			svcUser: "fleet", svcHome: "/var/lib/fleet", asRoot: false,
			wantPrefix:   nil,
			wantDir:      "",
			wantContains: "YOUR store, not fleet's",
		},
		{
			name:    "unknown service user runs as the caller, no note",
			svcUser: "", svcHome: "", asRoot: false,
			wantPrefix:   nil,
			wantDir:      "",
			wantContains: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execCtx, note := ServiceStorePodmanExec(tc.svcUser, tc.svcHome, tc.asRoot)
			if !reflect.DeepEqual(execCtx.Prefix, tc.wantPrefix) {
				t.Errorf("Prefix = %q, want %q", execCtx.Prefix, tc.wantPrefix)
			}
			if execCtx.Dir != tc.wantDir {
				t.Errorf("Dir = %q, want %q", execCtx.Dir, tc.wantDir)
			}
			if tc.wantContains == "" {
				if note != "" {
					t.Errorf("note = %q, want empty", note)
				}
			} else if !strings.Contains(note, tc.wantContains) {
				t.Errorf("note %q should contain %q", note, tc.wantContains)
			}
		})
	}
}
