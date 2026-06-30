package admincli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/creds"
)

// cmdMCP dispatches `fleet-admin mcp account set|list|del` — the MCP credential
// account store over the 0600 env file. Values are read from stdin (never argv);
// list never prints values.
//
// Account secrets are stored as suffixed env keys: <VAR>_<UPPER(account)>. The
// account name is canonicalized first (creds.CanonicalAccount: hyphen/space
// folded to underscore), so `client-a` and `client_a` write the SAME key and
// never fork two seats. This is the same convention creds.ApplyClientSuffix
// overlays at run time.
func cmdMCP(argv []string) int {
	if len(argv) < 1 || argv[0] != "account" {
		return errf(1, "usage: fleet-admin mcp account set|list|del")
	}
	sub := ""
	rest := argv[1:]
	if len(rest) > 0 {
		sub = rest[0]
		rest = rest[1:]
	}
	switch sub {
	case "set":
		return mcpAccountSet(rest)
	case "list", "ls":
		return mcpAccountList(rest)
	case "del", "delete", "rm":
		return mcpAccountDel(rest)
	default:
		return errf(1, "usage: fleet-admin mcp account set|list|del")
	}
}

// mcpAccountSet writes <VAR>_<UPPER(account)>=<stdin> into the env file, with
// the account name canonicalized (separators folded to underscore). The
// --secret flag carries KEY=- where "-" means "read the value from stdin".
func mcpAccountSet(argv []string) int {
	fs := flag.NewFlagSet("mcp account set", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "credential env file (default .env.local / FLEET_ENV_FILE)")
	secret := fs.String("secret", "", "KEY=- (value read from stdin) or KEY=value")
	pos, flagArgs := splitTwoPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if len(pos) < 2 {
		return errf(1, "usage: fleet-admin mcp account set <server> <account> --secret KEY=-")
	}
	server, account := pos[0], pos[1]
	if strings.TrimSpace(*secret) == "" {
		return errf(1, "--secret KEY=- is required")
	}
	eq := strings.IndexByte(*secret, '=')
	if eq <= 0 {
		return errf(1, "--secret must be KEY=- or KEY=value")
	}
	key := strings.TrimSpace((*secret)[:eq])
	val := (*secret)[eq+1:]
	if val == "-" {
		v, err := readStdinValue()
		if err != nil {
			return errf(5, "%v", err)
		}
		val = v
	}
	if val == "" {
		return errf(1, "empty secret value")
	}
	suffixed := key + "_" + strings.ToUpper(creds.CanonicalAccount(account))
	path := envFilePath(*envFile)
	if err := creds.SetEnvKey(path, suffixed, val); err != nil {
		return errf(5, "write %s: %v", path, err)
	}
	// server is recorded only in the message; the key naming carries the seat.
	fmt.Printf("set %s for server %q account %q in %s\n", suffixed, server, account, path)
	return 0
}

// mcpAccountList prints the suffixed credential KEYS provisioned for a server
// (NEVER values), derived from the env-file keys. We print the full suffixed key
// names rather than guessing the account label — account names may themselves
// contain underscores (CLIENT_A), so a key name is the unambiguous, leak-free
// view of which seats are provisioned.
func mcpAccountList(argv []string) int {
	fs := flag.NewFlagSet("mcp account list", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "credential env file (default .env.local / FLEET_ENV_FILE)")
	server, flagArgs := splitPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if strings.TrimSpace(server) == "" {
		return errf(1, "usage: fleet-admin mcp account list <server>")
	}
	path := envFilePath(*envFile)
	keys, err := creds.ListEnvKeys(path)
	if err != nil {
		return errf(5, "read %s: %v", path, err)
	}
	seen := map[string]struct{}{}
	var matched []string
	for _, k := range keys {
		if !serverMatchesVar(k, server) {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		matched = append(matched, k)
	}
	sort.Strings(matched)
	if len(matched) == 0 {
		fmt.Fprintf(os.Stderr, "(no account seats provisioned for server %q in %s)\n", server, path)
		return 0
	}
	for _, k := range matched {
		fmt.Println(k)
	}
	return 0
}

// mcpAccountDel removes a server account's suffixed keys (<VAR>_<UPPER(account)>)
// from the env file. The key naming does NOT encode the server, so the server
// name is matched against the VAR's underscore-delimited segments (anchored, not
// an arbitrary substring). To avoid silently destroying an unrelated connector's
// credential, it REFUSES when the server matches more than one distinct VAR;
// pass --key <VAR> to target one exactly.
func mcpAccountDel(argv []string) int {
	fs := flag.NewFlagSet("mcp account del", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "credential env file (default .env.local / FLEET_ENV_FILE)")
	keyVar := fs.String("key", "", "exact base VAR to target (skips server-name matching; use when del reports ambiguity)")
	pos, flagArgs := splitTwoPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if len(pos) < 2 {
		return errf(1, "usage: fleet-admin mcp account del [--key VAR] <server> <account>")
	}
	server, account := pos[0], pos[1]
	path := envFilePath(*envFile)
	keys, err := creds.ListEnvKeys(path)
	if err != nil {
		return errf(5, "read %s: %v", path, err)
	}
	wantSuffix := "_" + strings.ToUpper(creds.CanonicalAccount(account))

	// Collect candidate keys ending in _<ACCOUNT>, keyed by their base VAR.
	type cand struct{ key, baseVar string }
	var cands []cand
	for _, k := range keys {
		if !strings.HasSuffix(strings.ToUpper(k), wantSuffix) {
			continue
		}
		baseVar := k[:len(k)-len(wantSuffix)]
		if *keyVar != "" {
			if strings.EqualFold(baseVar, *keyVar) {
				cands = append(cands, cand{k, baseVar})
			}
			continue
		}
		if serverMatchesVar(baseVar, server) {
			cands = append(cands, cand{k, baseVar})
		}
	}
	if len(cands) == 0 {
		fmt.Fprintf(os.Stderr, "(no keys matched server %q account %q in %s)\n", server, account, path)
		return 2
	}

	// Refuse ambiguous deletes: matches spanning >1 distinct VAR could destroy an
	// unrelated connector's credential. Operator must disambiguate with --key.
	distinct := map[string]struct{}{}
	for _, c := range cands {
		distinct[strings.ToUpper(c.baseVar)] = struct{}{}
	}
	if len(distinct) > 1 {
		vars := make([]string, 0, len(distinct))
		for v := range distinct {
			vars = append(vars, v)
		}
		sort.Strings(vars)
		return errf(1, "ambiguous: server %q matches %d credential vars (%s) — re-run with --key <VAR> to target one (the key naming does not encode the server)",
			server, len(vars), strings.Join(vars, ", "))
	}

	removed := 0
	for _, c := range cands {
		ok, derr := creds.DeleteEnvKey(path, c.key)
		if derr != nil {
			return errf(5, "delete %s: %v", c.key, derr)
		}
		if ok {
			removed++
		}
	}
	fmt.Printf("removed %d key(s) for server %q account %q from %s\n", removed, server, account, path)
	return 0
}

// serverMatchesVar reports whether a credential VAR (or a full suffixed key)
// belongs to server, by matching the server name as a CONTIGUOUS run of the
// VAR's underscore-delimited segments rather than an arbitrary substring. This
// stops a short server token from over-matching unrelated connectors (server
// "io" no longer matches RATIO_TOKEN, only e.g. FAST_IO_API_KEY).
func serverMatchesVar(varOrKey, server string) bool {
	s := strings.ToUpper(strings.NewReplacer("-", "_", " ", "_").Replace(strings.TrimSpace(server)))
	if s == "" {
		return false
	}
	want := strings.Split(s, "_")
	have := strings.Split(strings.ToUpper(varOrKey), "_")
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j := range want {
			if have[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
