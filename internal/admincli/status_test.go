package admincli

import (
	"os"
	"strings"
	"testing"
)

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"postgres://chat:secret@127.0.0.1:5432/chat?sslmode=disable", "postgres://***@127.0.0.1:5432/chat?sslmode=disable"},
		{"postgres://user@host:5432/db", "postgres://***@host:5432/db"},
		{"postgres://host:5432/db", "postgres://host:5432/db"},                                                           // no userinfo, unchanged
		{"host=127.0.0.1 user=chat password=secret dbname=chat", "host=127.0.0.1 user=chat password=secret dbname=chat"}, // keyword DSN: no scheme, left as-is
		{"", ""},
	}
	for _, c := range cases {
		if got := redactDSN(c.in); got != c.want {
			t.Errorf("redactDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo\nthree"); got != "one" {
		t.Errorf("firstLine multi = %q, want %q", got, "one")
	}
	if got := firstLine("only"); got != "only" {
		t.Errorf("firstLine single = %q, want %q", got, "only")
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine empty = %q, want empty", got)
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
}

func TestServiceName(t *testing.T) {
	t.Setenv("FLEET_SERVICE_NAME", "")
	if got := serviceName("explicit"); got != "explicit" {
		t.Errorf("serviceName(flag) = %q, want explicit", got)
	}
	t.Setenv("FLEET_SERVICE_NAME", "envunit")
	if got := serviceName(""); got != "envunit" {
		t.Errorf("serviceName(env) = %q, want envunit", got)
	}
	_ = os.Unsetenv("FLEET_SERVICE_NAME")
	if got := serviceName(""); got != "fleet" {
		t.Errorf("serviceName(default) = %q, want fleet", got)
	}
}

func TestFindScript(t *testing.T) {
	// The repo ships scripts/bootstrap.sh + scripts/update.sh; from the package
	// dir, FLEET_ROOT points findScript at the repo root.
	t.Setenv("FLEET_ROOT", "../..")
	for _, name := range []string{"bootstrap.sh", "update.sh"} {
		if got := findScript(name); got == "" {
			t.Errorf("findScript(%q) = empty, want a path", name)
		}
	}
	if got := findScript("does-not-exist.sh"); got != "" {
		t.Errorf("findScript(missing) = %q, want empty", got)
	}
}

// TestSandboxProbeArgv — `fleet status` as root must probe the SERVICE user's
// rootless image store (the one the unit actually runs from), not root's; as
// anyone else it runs in the caller's store and must say so, because that
// verdict is about a different store than the one that matters.
func TestSandboxProbeArgv(t *testing.T) {
	ref := "localhost/fleet-sandbox:latest"
	argv, note := sandboxProbeArgv(ref, "fleet", "/var/lib/fleet", true)
	want := []string{"runuser", "-u", "fleet", "--", "env", "HOME=/var/lib/fleet", "XDG_RUNTIME_DIR=/run/fleet", "podman", "run", "--rm", ref, "true"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("root+service user argv = %q, want %q", argv, want)
	}
	if !strings.Contains(note, "as fleet") {
		t.Errorf("note = %q, want it to name the service user", note)
	}

	// Unknown home falls back to the StateDirectory convention.
	argv, _ = sandboxProbeArgv(ref, "fleet", "", true)
	if !strings.Contains(strings.Join(argv, " "), "HOME=/var/lib/fleet") {
		t.Errorf("no-home argv = %q, want the /var/lib/<user> fallback", argv)
	}

	// Root with a root-run service: plain podman, root's store is the right one.
	argv, note = sandboxProbeArgv(ref, "root", "/root", true)
	if argv[0] != "podman" || !strings.Contains(note, "root's store") {
		t.Errorf("root service argv=%q note=%q", argv, note)
	}

	// Non-root caller: plain podman, and the note points at the authoritative check.
	argv, note = sandboxProbeArgv(ref, "fleet", "/var/lib/fleet", false)
	if argv[0] != "podman" || !strings.Contains(note, "YOUR store") || !strings.Contains(note, "sudo fleet doctor") {
		t.Errorf("non-root argv=%q note=%q", argv, note)
	}

	// No unit at all: plain podman, nothing to caveat.
	argv, note = sandboxProbeArgv(ref, "", "", false)
	if argv[0] != "podman" || note != "" {
		t.Errorf("no-unit argv=%q note=%q", argv, note)
	}
}
