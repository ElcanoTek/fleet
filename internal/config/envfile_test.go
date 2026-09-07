package config

import "testing"

// TestResolveEnvFile pins the one resolution order every env-file reader and
// writer shares: explicit flag, then $FLEET_ENV_FILE, then the systemd
// deployment's /etc/fleet/fleet.env (keyed on the DIRECTORY existing, not the
// file), then the checkout-local .env.local.
func TestResolveEnvFile(t *testing.T) {
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	provisioned := func(p string) bool { return p == SystemEnvDir }
	bare := func(string) bool { return false }

	cases := []struct {
		name     string
		explicit string
		getenv   func(string) string
		isDir    func(string) bool
		want     string
	}{
		{"flag wins over everything", " /tmp/x.env ", env(map[string]string{"FLEET_ENV_FILE": "/env/y.env"}), provisioned, "/tmp/x.env"},
		{"FLEET_ENV_FILE wins over the /etc probe", "", env(map[string]string{"FLEET_ENV_FILE": "/env/y.env"}), provisioned, "/env/y.env"},
		{"blank FLEET_ENV_FILE is unset", "", env(map[string]string{"FLEET_ENV_FILE": "  "}), provisioned, SystemEnvFile},
		{"provisioned box → /etc/fleet/fleet.env even when the file is absent", "", env(nil), provisioned, SystemEnvFile},
		{"dev checkout → .env.local", "", env(nil), bare, LocalEnvFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveEnvFile(tc.explicit, tc.getenv, tc.isDir); got != tc.want {
				t.Fatalf("resolveEnvFile = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveEnvFileHonorsProcessEnv is the exported wrapper against the real
// process env: FLEET_ENV_FILE set means exactly that path, no probing.
func TestResolveEnvFileHonorsProcessEnv(t *testing.T) {
	t.Setenv("FLEET_ENV_FILE", "/some/where/fleet.env")
	if got := ResolveEnvFile(""); got != "/some/where/fleet.env" {
		t.Fatalf("ResolveEnvFile = %q", got)
	}
	if got := ResolveEnvFile("/flag.env"); got != "/flag.env" {
		t.Fatalf("ResolveEnvFile(flag) = %q", got)
	}
}
