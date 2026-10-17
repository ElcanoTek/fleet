// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hostnameBox is a scratch "box" for scripts/lib/hostname.sh: a stub
// hostnamectl backed by a file, an /etc/hosts and an /etc/cloud.
type hostnameBox struct {
	dir, hostsFile, cloudDir, nameFile, path string
}

const vultrHosts = "127.0.0.1   localhost localhost.localdomain\n::1         localhost localhost.localdomain\n127.0.0.1 vultr.guest vultr\n"

func newHostnameBox(t *testing.T, current, hosts string, withHostnamectl bool) *hostnameBox {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	b := &hostnameBox{dir: t.TempDir()}
	b.hostsFile = filepath.Join(b.dir, "hosts")
	b.cloudDir = filepath.Join(b.dir, "cloud")
	b.nameFile = filepath.Join(b.dir, "hostname")
	bin := toolsOnlyPath(t, "awk", "cat", "mkdir")
	if withHostnamectl {
		stub := "#!" + bash + "\ncase \"$1\" in\n  --static) cat '" + b.nameFile + "' ;;\n  set-hostname) printf '%s\\n' \"$2\" > '" + b.nameFile + "' ;;\nesac\n"
		if err := os.WriteFile(filepath.Join(bin, "hostnamectl"), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.path = bin
	for path, body := range map[string]string{b.nameFile: current + "\n", b.hostsFile: hosts} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(b.cloudDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *hostnameBox) run(t *testing.T, fqdn string, env ...string) string {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "hostname.sh")
	script := `set -euo pipefail
. "$1"
info() { printf 'INFO %s\n' "$*"; }
ok()   { printf 'OK %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*"; }
fleet_set_hostname "$2"
printf 'RC=%s\n' "$?"
`
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, "-c", script, "hostname", lib, fqdn)
	cmd.Env = append([]string{"PATH=" + b.path, "FLEET_HOSTS_FILE=" + b.hostsFile, "FLEET_CLOUD_DIR=" + b.cloudDir}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "RC=0") {
		t.Fatalf("fleet_set_hostname failed: %v\n%s", err, out)
	}
	return string(out)
}

func (b *hostnameBox) read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func (b *hostnameBox) cloudCfg() string {
	return filepath.Join(b.cloudDir, "cloud.cfg.d", "99-preserve-hostname.cfg")
}

// TestSetHostname pins scripts/lib/hostname.sh: bootstrap renames a box to
// its domain only from an unset cloud-image default (or when forced), pins
// the name against cloud-init, fixes the loopback lines in /etc/hosts, and
// does it once.
func TestSetHostname(t *testing.T) {
	const wantCfg = "# The hostname is set by hand; do not let cloud-init reset it from Vultr metadata.\npreserve_hostname: true\nmanage_etc_hosts: false\n"

	t.Run("vultr default is renamed, pinned and written to hosts, once", func(t *testing.T) {
		b := newHostnameBox(t, "vultr.guest", vultrHosts, true)
		out := b.run(t, "Fleet.Example.com")
		if got := strings.TrimSpace(b.read(t, b.nameFile)); got != "fleet.example.com" {
			t.Errorf("hostname = %q, want fleet.example.com\n%s", got, out)
		}
		if got := b.read(t, b.cloudCfg()); got != wantCfg {
			t.Errorf("cloud cfg = %q", got)
		}
		// The loopback line naming the old name is rewritten in place; nothing
		// is appended because a line now names the FQDN.
		wantHosts := "127.0.0.1   localhost localhost.localdomain\n::1         localhost localhost.localdomain\n127.0.0.1 fleet.example.com fleet\n"
		if got := b.read(t, b.hostsFile); got != wantHosts {
			t.Errorf("hosts:\n got %q\nwant %q", got, wantHosts)
		}
		out = b.run(t, "fleet.example.com")
		if !strings.Contains(out, "hostname already fleet.example.com") || strings.Contains(out, "OK cloud-init") || strings.Contains(out, "names fleet.example.com") {
			t.Errorf("second run should change nothing\n%s", out)
		}
		if got := b.read(t, b.hostsFile); got != wantHosts {
			t.Errorf("second run changed hosts: %q", got)
		}
	})

	t.Run("a localhost line is never rewritten", func(t *testing.T) {
		b := newHostnameBox(t, "localhost", "127.0.0.1 localhost\n::1 localhost\n", true)
		b.run(t, "fleet.example.com")
		want := "127.0.0.1 localhost\n::1 localhost\n127.0.0.1 fleet.example.com fleet\n::1 fleet.example.com fleet\n"
		if got := b.read(t, b.hostsFile); got != want {
			t.Errorf("hosts:\n got %q\nwant %q", got, want)
		}
	})

	for _, name := range []string{"yoga", "fedora", "fleet.client.example"} {
		t.Run("a deliberate name is left alone: "+name, func(t *testing.T) {
			b := newHostnameBox(t, name, vultrHosts, true)
			out := b.run(t, "fleet.example.com")
			if got := strings.TrimSpace(b.read(t, b.nameFile)); got != name {
				t.Errorf("hostname changed to %q", got)
			}
			if b.read(t, b.hostsFile) != vultrHosts || b.read(t, b.cloudCfg()) != "" {
				t.Errorf("hosts or cloud cfg touched\n%s", out)
			}
			if !strings.Contains(out, "hostnamectl set-hostname fleet.example.com") {
				t.Errorf("want a one-line hint\n%s", out)
			}
		})
	}

	t.Run("FLEET_SET_HOSTNAME=1 forces, =0 skips", func(t *testing.T) {
		b := newHostnameBox(t, "yoga", vultrHosts, true)
		b.run(t, "fleet.example.com", "FLEET_SET_HOSTNAME=0")
		if got := strings.TrimSpace(b.read(t, b.nameFile)); got != "yoga" {
			t.Errorf("=0 renamed to %q", got)
		}
		b.run(t, "fleet.example.com", "FLEET_SET_HOSTNAME=1")
		if got := strings.TrimSpace(b.read(t, b.nameFile)); got != "fleet.example.com" {
			t.Errorf("=1 did not force the rename: %q", got)
		}
	})

	t.Run("skips a non-FQDN target, a missing hostnamectl, and a dry run", func(t *testing.T) {
		for _, tc := range []struct {
			fqdn string
			ctl  bool
			env  []string
		}{
			{"localhost", true, nil},
			{"10.0.0.5", true, nil},
			{"fleet.example.com", false, nil},
			{"fleet.example.com", true, []string{"DRY_RUN=1"}},
		} {
			b := newHostnameBox(t, "vultr.guest", vultrHosts, tc.ctl)
			out := b.run(t, tc.fqdn, tc.env...)
			if strings.TrimSpace(b.read(t, b.nameFile)) != "vultr.guest" || b.read(t, b.hostsFile) != vultrHosts || b.read(t, b.cloudCfg()) != "" {
				t.Errorf("%+v changed the box\n%s", tc, out)
			}
		}
	})
}

// TestHostnameStepIsBootstrapOnly — bootstrap names the box; update never does.
func TestHostnameStepIsBootstrapOnly(t *testing.T) {
	root := repoRootFromTest(t)
	boot, err := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(boot), `fleet_set_hostname "$WEB_DOMAIN"`) {
		t.Error("bootstrap.sh does not call fleet_set_hostname with the domain")
	}
	upd, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(upd), "hostname.sh") || strings.Contains(string(upd), "set_hostname") {
		t.Error("update.sh must never rename a box")
	}
}
