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

const keyOnlyConf = `# Key-only SSH: no passwords, root by key only. Sorts before cloud-init's
# 50-cloud-init.conf so it wins (sshd keeps the first value it reads).
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin prohibit-password
`

// sshBox is a scratch root for scripts/lib/ssh.sh: FLEET_SSH_ROOT points at it,
// and stub sshd/systemctl binaries log their calls.
type sshBox struct {
	root, bin, log, conf string
}

func newSSHBox(t *testing.T, withSSHD bool) *sshBox {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	b := &sshBox{root: filepath.Join(dir, "root"), bin: filepath.Join(dir, "bin"), log: filepath.Join(dir, "calls.log")}
	b.conf = filepath.Join(b.root, "etc", "ssh", "sshd_config.d", "01-no-password-login.conf")
	for _, d := range []string{b.bin, filepath.Dir(b.conf), filepath.Join(b.root, "root", ".ssh"), filepath.Join(b.root, "home", "ops")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The real tools the lib needs, and nothing else from the host PATH (a CI
	// runner's own sshd must not leak in).
	for _, tool := range []string{"cat", "cmp", "cp", "grep", "install", "mktemp", "mv", "rm"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(p, filepath.Join(b.bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(b.bin, name), []byte("#!"+bash+"\nprintf '"+name+" %s\\n' \"$*\" >> '"+b.log+"'\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if withSSHD {
		stub("sshd", `case "$1" in
  -t) exit "${STUB_T_RC:-0}" ;;
  -T) printf 'pubkeyauthentication %s\nauthorizedkeysfile .ssh/authorized_keys /etc/ssh/keys/%%u\npasswordauthentication %s\npermitrootlogin prohibit-password\n' "${STUB_PK:-yes}" "${STUB_PA:-no}" ;;
esac
`)
	}
	stub("systemctl", `[[ "${STUB_RELOAD_FAIL:-}" != "$2" ]]`+"\n")
	return b
}

func (b *sshBox) addKey(t *testing.T, rel string) {
	t.Helper()
	p := filepath.Join(b.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("ssh-ed25519 AAAAC3Nza test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (b *sshBox) run(t *testing.T, env ...string) (out, calls string) {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "ssh.sh")
	_ = os.Remove(b.log)
	script := `set -euo pipefail
. "$1"
info() { printf 'INFO %s\n' "$*"; }
ok()   { printf 'OK %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*"; }
ssh_key_only
printf 'RC=%s\n' "$?"
`
	bash, _ := exec.LookPath("bash")
	cmd := exec.Command(bash, "-c", script, "ssh", lib)
	cmd.Env = append([]string{"PATH=" + b.bin, "FLEET_SSH_ROOT=" + b.root, "TMPDIR=" + t.TempDir()}, env...)
	o, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(o), "RC=0") {
		t.Fatalf("ssh_key_only failed: %v\n%s", err, o)
	}
	c, _ := os.ReadFile(b.log)
	return string(o), string(c)
}

// TestSSHKeyOnly pins scripts/lib/ssh.sh: with a key installed it writes the
// exact drop-in 0600, validates with sshd -t and reloads sshd; it is a no-op
// on a second run, honours the opt-out and DRY_RUN, refuses with 0 keys,
// reverts on a failed sshd -t, falls back to the ssh unit, and warns when an
// earlier directive still wins.
func TestSSHKeyOnly(t *testing.T) {
	t.Run("applies, then idempotent", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		out, calls := b.run(t)
		got, err := os.ReadFile(b.conf)
		if err != nil || string(got) != keyOnlyConf {
			t.Fatalf("drop-in: %v\n%q", err, got)
		}
		if fi, _ := os.Stat(b.conf); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", fi.Mode().Perm())
		}
		if !strings.Contains(calls, "sshd -t") || !strings.Contains(calls, "systemctl reload sshd") || !strings.Contains(out, "OK SSH is key-only") {
			t.Errorf("want sshd -t + reload + ok\ncalls:\n%s\n%s", calls, out)
		}
		out, calls = b.run(t)
		if strings.TrimSpace(out) != "RC=0" || strings.Contains(calls, "systemctl") || strings.Contains(calls, "sshd -t") {
			t.Errorf("second run should do nothing\ncalls:\n%s\n%s", calls, out)
		}
		// Already in place, but an earlier directive still wins: warn anyway.
		if out, calls := b.run(t, "STUB_PA=yes"); !strings.Contains(out, "WARN sshd still accepts passwords") || strings.Contains(calls, "systemctl") {
			t.Errorf("in place + earlier directive: want a warning, no reload\ncalls:\n%s\n%s", calls, out)
		}
	})

	t.Run("AuthorizedKeysFile from sshd -T with %u", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "etc/ssh/keys/ops")
		if out, _ := b.run(t); !strings.Contains(out, "OK SSH is key-only") {
			t.Errorf("a key under /etc/ssh/keys/%%u should count\n%s", out)
		}
	})

	t.Run("no keys: warn, write nothing", func(t *testing.T) {
		b := newSSHBox(t, true)
		out, calls := b.run(t)
		if !strings.Contains(out, "WARN no SSH authorized keys") || strings.Contains(calls, "systemctl") {
			t.Errorf("want lockout warning and no reload\ncalls:\n%s\n%s", calls, out)
		}
		if _, err := os.Stat(b.conf); err == nil {
			t.Error("drop-in written with no keys")
		}
	})

	t.Run("pubkey auth off counts as no keys", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		if out, _ := b.run(t, "STUB_PK=no"); !strings.Contains(out, "WARN no SSH authorized keys") {
			t.Errorf("pubkeyauthentication no: want the lockout warning\n%s", out)
		}
	})

	t.Run("silent skips", func(t *testing.T) {
		b := newSSHBox(t, false)
		b.addKey(t, "root/.ssh/authorized_keys")
		if out, _ := b.run(t); strings.TrimSpace(out) != "RC=0" {
			t.Errorf("no sshd: want silence\n%s", out)
		}
		b = newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		if err := os.RemoveAll(filepath.Dir(b.conf)); err != nil {
			t.Fatal(err)
		}
		if out, _ := b.run(t); strings.TrimSpace(out) != "RC=0" {
			t.Errorf("no sshd_config.d: want silence\n%s", out)
		}
	})

	t.Run("opt-out and dry run", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		if out, calls := b.run(t, "FLEET_KEEP_SSH_PASSWORDS=1"); !strings.Contains(out, "INFO FLEET_KEEP_SSH_PASSWORDS=1") || calls != "" {
			t.Errorf("opt-out\ncalls:\n%s\n%s", calls, out)
		}
		out, calls := b.run(t, "DRY_RUN=1")
		if !strings.Contains(out, "[dry-run] would write") || strings.Contains(calls, "systemctl") || strings.Contains(calls, "sshd -t") {
			t.Errorf("dry run\ncalls:\n%s\n%s", calls, out)
		}
		if _, err := os.Stat(b.conf); err == nil {
			t.Error("dry run wrote the drop-in")
		}
	})

	t.Run("sshd -t fails: revert", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		out, calls := b.run(t, "STUB_T_RC=1")
		if !strings.Contains(out, "WARN sshd -t rejected") || strings.Contains(calls, "systemctl") {
			t.Errorf("want revert warning, no reload\ncalls:\n%s\n%s", calls, out)
		}
		if _, err := os.Stat(b.conf); err == nil {
			t.Error("drop-in left behind after sshd -t failed")
		}
		// A differing hand-made file is put back, not deleted.
		if err := os.WriteFile(b.conf, []byte("PasswordAuthentication no\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		b.run(t, "STUB_T_RC=1")
		if got, _ := os.ReadFile(b.conf); string(got) != "PasswordAuthentication no\n" {
			t.Errorf("hand-made drop-in not restored: %q", got)
		}
	})

	t.Run("reload falls back to ssh", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		out, calls := b.run(t, "STUB_RELOAD_FAIL=sshd")
		if !strings.Contains(calls, "systemctl reload ssh\n") || !strings.Contains(out, "OK SSH is key-only") {
			t.Errorf("want fallback to ssh\ncalls:\n%s\n%s", calls, out)
		}
	})

	t.Run("earlier directive wins: warn", func(t *testing.T) {
		b := newSSHBox(t, true)
		b.addKey(t, "root/.ssh/authorized_keys")
		if out, _ := b.run(t, "STUB_PA=yes"); !strings.Contains(out, "WARN sshd still accepts passwords") {
			t.Errorf("want an earlier-directive warning\n%s", out)
		}
	})
}

// TestSSHKeyOnlyIsBootstrapOnly — update never touches sshd.
func TestSSHKeyOnlyIsBootstrapOnly(t *testing.T) {
	root := repoRootFromTest(t)
	boot, _ := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if !strings.Contains(string(boot), "\nssh_key_only\n") {
		t.Error("bootstrap.sh does not call ssh_key_only")
	}
	upd, _ := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if strings.Contains(string(upd), "ssh_key_only") || strings.Contains(string(upd), "lib/ssh.sh") {
		t.Error("update.sh must not touch sshd")
	}
}

func TestSSHDAcceptsPasswords(t *testing.T) {
	for in, want := range map[string]bool{
		"passwordauthentication no\npermitrootlogin prohibit-password\n":  false,
		"passwordauthentication yes\npermitrootlogin prohibit-password\n": true,
		"passwordauthentication no\npermitrootlogin yes\n":                true,
		"permitrootlogin without-password\n":                              false,
		"":                                                                false,
	} {
		if got := sshdAcceptsPasswords(in); got != want {
			t.Errorf("sshdAcceptsPasswords(%q) = %v, want %v", in, got, want)
		}
	}
}
