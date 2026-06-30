package admincli

import (
	"os"
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
