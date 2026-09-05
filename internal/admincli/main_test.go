package admincli

import (
	"regexp"
	"strings"
	"testing"
)

// usageLineFlags returns the --flag names named on the first usage line that
// starts with prefix (after leading whitespace), e.g. "fleet validate-config".
func usageLineFlags(text, prefix string) []string {
	re := regexp.MustCompile(`--([a-z][a-z0-9-]*)`)
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		var out []string
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
		return out
	}
	return nil
}

// TestUsageNamesEveryDispatchedVerb — every verb Run dispatches must appear in
// the top-level help as "fleet <verb>", so a shipped verb is never invisible
// (the #722 round found several). The list mirrors Run's switch.
func TestUsageNamesEveryDispatchedVerb(t *testing.T) {
	text := UsageText()
	for _, verb := range []string{
		"bootstrap", "update", "cleanup", "status", "doctor", "diagnose", "start", "restart", "stop",
		"logs", "timers", "motd", "chat", "sched", "admin", "config", "env", "task", "mcp", "notes",
		"worktree", "migrate", "backup", "restore", "import", "version",
	} {
		if !strings.Contains(text, "fleet "+verb) {
			t.Errorf("usage does not mention %q", "fleet "+verb)
		}
	}
	// The in-binary verbs cmd/fleet routes are documented here too.
	for _, verb := range []string{"fleet validate-config", "fleet mcp test", "fleet eval", "fleet generate-vapid-keys"} {
		if !strings.Contains(text, verb) {
			t.Errorf("usage does not mention %q", verb)
		}
	}
}

// TestUsageValidateConfigFlags — the validate-config usage line advertised
// --client-config and --check-model-api, two flags the verb never defined
// (the real ones are --bundle-path / --skip-network-checks / --json). This
// pins the names; cmd/fleet's TestUsageValidateConfigFlagsMatchFlagSet checks
// the same line against the FlagSet that actually parses them.
func TestUsageValidateConfigFlags(t *testing.T) {
	got := usageLineFlags(UsageText(), "fleet validate-config")
	want := map[string]bool{"bundle-path": true, "skip-network-checks": true, "json": true}
	if len(got) != len(want) {
		t.Fatalf("validate-config usage flags = %v, want exactly %v", got, want)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("usage names %q, which validate-config does not define", "--"+f)
		}
	}
}

// TestUsageExitCodeLegend — the legend must name the codes the verbs actually
// return (grep errf\( for the set): 2 is "not found" on the admin verbs, 3 is
// "already exists", 4 is "wrong state" — three facts the old legend omitted or
// got wrong.
func TestUsageExitCodeLegend(t *testing.T) {
	text := UsageText()
	idx := strings.Index(text, "Exit codes")
	if idx < 0 {
		t.Fatalf("usage has no exit-code legend:\n%s", text)
	}
	legend := text[idx:]
	for _, want := range []string{"2 not found", "3 already exists", "4 wrong state", "5 operational failure", "6 fleet status"} {
		if !strings.Contains(legend, want) {
			t.Errorf("exit-code legend missing %q:\n%s", want, legend)
		}
	}
}

// TestUsageEnvFileLineMatchesResolver — the Connection footer must describe the
// real env-file order (flag, FLEET_ENV_FILE, /etc/fleet/fleet.env, .env.local);
// it used to say "(default .env.local) for mcp account", which was exactly the
// bug: mcp account wrote .env.local on a provisioned box.
func TestUsageEnvFileLineMatchesResolver(t *testing.T) {
	text := UsageText()
	if !strings.Contains(text, "/etc/fleet/fleet.env when /etc/fleet exists, else .env.local") {
		t.Errorf("usage env-file line does not describe the resolver order")
	}
	if strings.Contains(text, "(default .env.local) for mcp account") {
		t.Errorf("usage still carries the stale mcp-account-only env-file note")
	}
}
