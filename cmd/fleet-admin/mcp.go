package main

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
// Account secrets are stored as suffixed env keys: <VAR>_<UPPER(account)>. This
// is the same convention creds.ApplyClientSuffix overlays at run time.
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

// mcpAccountSet writes <VAR>_<UPPER(account)>=<stdin> into the env file. The
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
	suffixed := key + "_" + strings.ToUpper(account)
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
	prefix := strings.ToUpper(server)
	seen := map[string]struct{}{}
	var matched []string
	for _, k := range keys {
		if !strings.Contains(strings.ToUpper(k), prefix) {
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

// mcpAccountDel removes a server account's suffixed keys from the env file. It
// deletes every <VAR>_<UPPER(account)> key whose VAR contains the server token.
func mcpAccountDel(argv []string) int {
	fs := flag.NewFlagSet("mcp account del", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "credential env file (default .env.local / FLEET_ENV_FILE)")
	pos, flagArgs := splitTwoPositional(argv)
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	if len(pos) < 2 {
		return errf(1, "usage: fleet-admin mcp account del <server> <account>")
	}
	server, account := pos[0], pos[1]
	path := envFilePath(*envFile)
	keys, err := creds.ListEnvKeys(path)
	if err != nil {
		return errf(5, "read %s: %v", path, err)
	}
	wantSuffix := "_" + strings.ToUpper(account)
	serverTok := strings.ToUpper(server)
	removed := 0
	for _, k := range keys {
		up := strings.ToUpper(k)
		if strings.HasSuffix(up, wantSuffix) && strings.Contains(up, serverTok) {
			ok, derr := creds.DeleteEnvKey(path, k)
			if derr != nil {
				return errf(5, "delete %s: %v", k, derr)
			}
			if ok {
				removed++
			}
		}
	}
	if removed == 0 {
		fmt.Fprintf(os.Stderr, "(no keys matched server %q account %q in %s)\n", server, account, path)
		return 2
	}
	fmt.Printf("removed %d key(s) for server %q account %q from %s\n", removed, server, account, path)
	return 0
}
