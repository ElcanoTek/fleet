package admincli

import (
	"os"
	"testing"
)

func TestRedactDSN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"postgres://chat:secret@127.0.0.1:5432/chat?sslmode=disable", "postgres://***@127.0.0.1:5432/chat?sslmode=disable"},
		{"postgres://user@host:5432/db", "postgres://***@host:5432/db"},
		{"postgres://host:5432/db", "postgres://host:5432/db"}, // no userinfo, unchanged
		// #2055 review: every credential shape, not only userinfo.
		{"host=127.0.0.1 user=chat password=secret dbname=chat", "host=127.0.0.1 user=chat password=*** dbname=chat"},
		{"host=h password='a b' sslpassword=k passfile=/root/.pgpass dbname=x", "host=h password=*** sslpassword=*** passfile=*** dbname=x"},
		{"postgres://h/db?password=secret&sslmode=disable", "postgres://h/db?password=***&sslmode=disable"},
		{"postgres://u:pw@h/db?sslpassword=k&passfile=%2Froot%2F.pgpass", "postgres://***@h/db?sslpassword=***&passfile=***"},
		{"postgres://h/db?note=a@b", "postgres://h/db?note=a@b"}, // an @ in the query is not userinfo
		{"not a dsn with password inside", "[REDACTED]"},
		{"", ""},
	}
	for _, c := range cases {
		if got := redactDSN(c.in); got != c.want {
			t.Errorf("redactDSN(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
