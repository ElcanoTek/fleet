// LEGACY-DB-LAYOUT: remove after consolidation — this whole file.

package admincli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsolidateChecksTheClusterIdentity (#2055 review): the superuser
// tools reach a socket and the DSNs may name loopback TCP — two clusters can
// answer on one port. The cluster each source DSN reaches (as written) must
// be the superuser's: by system identifier when the role can read it, else by
// postmaster start time, port and cluster name.
func TestConsolidateChecksTheClusterIdentity(t *testing.T) {
	clearDSNEnv(t)
	const sysid = "pg_control_system"
	cases := []struct {
		name    string
		dsnSide func(q string) ([][]string, error)
		ok      bool
		want    string
	}{
		{"same system identifier", nil, true, "system identifier 7701435355509418483"},
		{"another cluster", func(q string) ([][]string, error) {
			if strings.Contains(q, sysid) {
				return [][]string{{"1111"}}, nil
			}
			return [][]string{{"1793000000.5 port 5432 cluster 16/main"}}, nil
		}, false, "two clusters answer here"},
		{"identifier unreadable, fallback equal", func(q string) ([][]string, error) {
			if strings.Contains(q, sysid) {
				return nil, errors.New("permission denied for function pg_control_system")
			}
			return [][]string{{"1793000000.5 port 5432 cluster 16/main"}}, nil
		}, true, "postmaster started 1793000000.5 port 5432 cluster 16/main"},
		{"identifier unreadable, fallback differs", func(q string) ([][]string, error) {
			if strings.Contains(q, sysid) {
				return nil, errors.New("permission denied")
			}
			return [][]string{{"1790000000.1 port 5432 cluster 17/other"}}, nil
		}, false, "two clusters answer here"},
		{"DSN does not connect", func(string) ([][]string, error) {
			return nil, errors.New("connection refused")
		}, false, "connect with the chat DSN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFake()
			var out bytes.Buffer
			d := testDeps(f, &out)
			if tc.dsnSide != nil {
				d.dsnQuery = func(_ context.Context, _ string, q string) ([][]string, error) { return tc.dsnSide(q) }
			}
			code := runConsolidate(t.Context(), consolidateOptions{envFile: writeLegacyEnv(t, ""), targetDB: "fleet"}, d)
			if (code == 0) != tc.ok || !strings.Contains(out.String(), tc.want) {
				t.Errorf("exit %d (want ok=%v), output lacks %q:\n%s", code, tc.ok, tc.want, out.String())
			}
		})
	}
}

// TestConsolidateRedactsQueryPasswords (#2055 review): a password carried as
// a DSN query parameter is a password too — collected into plan.passwords,
// scrubbed from failure output, and hidden in the printed DSNs.
func TestConsolidateRedactsQueryPasswords(t *testing.T) {
	clearDSNEnv(t)
	env := filepath.Join(t.TempDir(), "fleet.env")
	body := "FLEET_CHAT_DATABASE_URL=postgres://chat@127.0.0.1:5432/chat?password=placeholder-chat-qpw\n" +
		"FLEET_SCHED_DATABASE_URL=postgres://sched@127.0.0.1:5432/sched?sslmode=disable&password=placeholder-sched-qpw\n"
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := healthyFake()
	var out bytes.Buffer
	d := testDeps(f, &out)
	d.dsnQuery = func(context.Context, string, string) ([][]string, error) {
		return nil, errors.New(`failed to connect to "password=placeholder-chat-qpw"`)
	}
	_ = runConsolidate(t.Context(), consolidateOptions{envFile: env, targetDB: "fleet"}, d)
	got := out.String()
	for _, secret := range []string{"placeholder-chat-qpw", "placeholder-sched-qpw"} {
		if strings.Contains(got, secret) {
			t.Errorf("output leaks %s:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "password=***") {
		t.Errorf("the printed DSNs do not show the redacted password parameter:\n%s", got)
	}
	p, _, err := buildPlan(consolidateOptions{envFile: env, targetDB: "fleet"}, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.passwords) != 2 {
		t.Errorf("plan.passwords = %d entries, want both query passwords", len(p.passwords))
	}
}
