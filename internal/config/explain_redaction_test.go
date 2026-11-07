package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// explainRun is Explain with positional arguments, returning the settings.
func explainRun(envFile string, keys []string, all bool, unit *UnitEnv) ([]Explanation, error) {
	rep, err := Explain(ExplainOptions{EnvFile: envFile, Keys: keys, All: all, Unit: unit})
	if err != nil {
		return nil, err
	}
	return rep.Settings, nil
}

// unitExporting is the standard unit's environment: EnvironmentFile=path
// (every line of it), FLEET_ENV_FILE unset.
func unitExporting(t *testing.T, path string) *UnitEnv {
	t.Helper()
	show := "LoadState=loaded\nEnvironment=\nEnvironmentFiles=" + path + " (ignore_errors=yes)\nUnsetEnvironment=FLEET_ENV_FILE FLEET_CLIENT_CONFIG_DIR\n"
	u := UnitEnvFromSystemctl("fleet.service", show, os.ReadFile)
	if u == nil {
		t.Fatal("unit not modelled")
	}
	return u
}

// Codex P2 (#2057): an alias the registry marks reader-specific (only one
// reader honours it; the server ignores it) must never be reported as the
// setting's configured value — only as a note. Every such alias, from the
// process env, the env file, and the systemd unit's environment.
func TestExplainNeverSelectsAReaderSpecificAlias(t *testing.T) {
	var checked int
	for _, e := range envreg.All() {
		for _, a := range e.Aliases {
			if a.ReadBy == "" {
				continue
			}
			checked++
			for _, where := range []string{"process", "file", "unit"} {
				t.Run(a.Name+"/"+where, func(t *testing.T) {
					unsetForTest(t, e.Spellings()...)
					path := filepath.Join(t.TempDir(), "fleet.env")
					writeEnv(t, path, "")
					var unit *UnitEnv
					switch where {
					case "process":
						t.Setenv(a.Name, "reader-only-value")
					case "file":
						writeEnv(t, path, a.Name+"=reader-only-value\n")
					case "unit":
						writeEnv(t, path, a.Name+"=reader-only-value\n")
						unit = unitExporting(t, path)
					}
					got, err := explainRun(path, []string{e.Name}, false, unit)
					if err != nil {
						t.Fatal(err)
					}
					x := got[0]
					if x.Set || x.Source != SourceDefault || x.Value == "reader-only-value" {
						t.Fatalf("%s = %+v; a reader-specific alias must not be the configured value", e.Name, x)
					}
					notes := strings.Join(x.Notes, "\n")
					if !strings.Contains(notes, "set as "+a.Name) || !strings.Contains(notes, "ignores it") {
						t.Errorf("notes %v do not report %s as read only by %s", x.Notes, a.Name, a.ReadBy)
					}
				})
			}
		}
	}
	if checked < 7 {
		t.Fatalf("checked only %d reader-specific aliases; the registry marks at least 7", checked)
	}
}

// Codex P2 (#2057): on the standard install systemd's EnvironmentFile=
// exports every line of the env file, so a spelling fleet's own loader does
// not admit is configured there — explain reports the unit as the source.
func TestExplainCountsLinesSystemdExports(t *testing.T) {
	unsetForTest(t, "FLEET_MAX_ITERATIONS", "CHAT_MAX_ITERATIONS", "CUTLASS_MAX_ITERATIONS")
	path := filepath.Join(t.TempDir(), "fleet.env")
	writeEnv(t, path, "CUTLASS_MAX_ITERATIONS=70\n")

	off, err := explainRun(path, []string{"FLEET_MAX_ITERATIONS"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if off[0].Set {
		t.Errorf("no unit: %+v, want the default (fleet's loader ignores the spelling)", off[0])
	}
	on, err := explainRun(path, []string{"FLEET_MAX_ITERATIONS"}, false, unitExporting(t, path))
	if err != nil {
		t.Fatal(err)
	}
	x := on[0]
	if !x.Set || x.Value != "70" || x.Spelling != "CUTLASS_MAX_ITERATIONS" || !x.Deprecated ||
		x.Source != SourceUnit || x.SourceDetail != "EnvironmentFile="+path {
		t.Fatalf("with the unit: %+v, want 70 from the unit's EnvironmentFile= via the deprecated alias", x)
	}
}

// Credential sentinels: whatever value an operator puts in ANY setting,
// explain must not print a password, token or key embedded in it, and must
// not print a secret at all.
const (
	sentinelPassword = "pwSENTINEL7"
	sentinelToken    = "tokSENTINEL7"
	sentinelHost     = "hostsentinel7.example"
	sentinelValue    = "https://user:" + sentinelPassword + "@" + sentinelHost + "/db?sslmode=disable&access_token=" + sentinelToken + "&api_key=" + sentinelToken
)

// SetCredentialSentinels sets every registered canonical name to a
// credential-shaped URL in the process env (restored after the test).
func setCredentialSentinels(t *testing.T) {
	t.Helper()
	for _, e := range envreg.All() {
		t.Setenv(e.Name, sentinelValue)
	}
}

func TestExplainNeverPrintsACredential(t *testing.T) {
	setCredentialSentinels(t)
	all, err := explainRun("", nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		e, _ := envreg.Find(x.Key)
		if strings.Contains(x.Value, sentinelPassword) || strings.Contains(x.Value, sentinelToken) {
			t.Errorf("%s printed a credential: %q", x.Key, x.Value)
		}
		if e.Secret && e.Type != envreg.TypeDSN && x.Value != Redacted {
			t.Errorf("%s is a secret but explain printed %q", x.Key, x.Value)
		}
		if e.Type == envreg.TypeDSN && !strings.Contains(x.Value, sentinelHost) {
			t.Errorf("%s: a DSN should keep its host readable, got %q", x.Key, x.Value)
		}
	}
}

func TestRedactDSN(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://chat:s3cret@db.internal:5432/chat?sslmode=disable": "postgres://***@db.internal:5432/chat?sslmode=disable",
		"postgres://chat@db/chat":                                      "postgres://***@db/chat",
		"https://hooks.example.com/x?token=abc&channel=ops#frag":       "https://hooks.example.com/x?token=REDACTED&channel=ops#frag",
		"https://key@o1.ingest.sentry.io/42":                           "https://***@o1.ingest.sentry.io/42",
		"https://u:p@h/p?sig=1&X-Amz-Signature=2&Api_Key=3&plain=4":    "https://***@h/p?sig=REDACTED&X-Amz-Signature=REDACTED&Api_Key=REDACTED&plain=4",
		"host=db port=5432 user=chat password=s3cret dbname=chat":      "host=db port=5432 user=chat password=*** dbname=chat",
		"host=db sslpassword='a b' password = x":                       "host=db sslpassword=*** password=***",
		"https://example.com/path@not-userinfo":                        "https://example.com/path@not-userinfo",
		"no credentials here":                                          "no credentials here",
		// Codex round 3 (#2057): percent-encoded parameter names are matched
		// decoded, and keep their spelling in the output.
		"https://h/p?%74oken=abc&api%5Fkey=def&%zz=1&ok=2":                "https://h/p?%74oken=REDACTED&api%5Fkey=REDACTED&%zz=1&ok=2",
		"https://h/p?Pass%77ord=x":                                        "https://h/p?Pass%77ord=REDACTED",
		"postgres://u:p%40ss@[2001:db8::1]:5432/chat?password=x&user=bob": "postgres://***@[2001:db8::1]:5432/chat?password=REDACTED&user=bob",
	} {
		if got := RedactDSN(in); got != want {
			t.Errorf("RedactDSN(%q) = %q, want %q", in, got, want)
		}
	}
}
