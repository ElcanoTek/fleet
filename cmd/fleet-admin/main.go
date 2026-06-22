// Command fleet-admin is the unified admin CLI for the Mega Box. It folds chat's
// chat-admin (chat users) and moc's -create-user/-set-role flags (sched users +
// API keys) into one tool, plus MCP credential-account management, the notes
// wiki admin verbs, and a thin bootstrap wrapper.
//
// Subcommands:
//
//	fleet-admin bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
//	fleet-admin update    [--no-pull] [--client-config <dir>] [--service <name>] [--yes] [--dry-run]
//	fleet-admin status    [--service <name>] [--no-sandbox]
//	fleet-admin chat user add|update|del|list
//	fleet-admin sched user add|update|set-role|rename|del|list
//	fleet-admin sched apikey create|list|revoke|delete
//	fleet-admin mcp account set|list|del
//	fleet-admin notes set|get|list|rm
//	fleet-admin notes proposal publish|reject
//
// The operator lifecycle is bootstrap → update → status: bootstrap provisions a
// box, update rolls a new version in place, status (a.k.a. doctor) reports
// health. bootstrap + update are thin wrappers over scripts/bootstrap.sh +
// scripts/update.sh; status runs in-process read-only checks.
//
// Passwords are NEVER taken on argv — pass `--password -` to read from stdin.
// Email/username normalization, bcrypt.DefaultCost, and the 0-users
// unprovisioned guard are preserved from the source tools.
package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(argv []string) int {
	if len(argv) == 0 {
		usage()
		return 1
	}
	switch argv[0] {
	case "bootstrap":
		return cmdBootstrap(argv[1:])
	case "update":
		return cmdUpdate(argv[1:])
	case "status", "doctor":
		return cmdStatus(argv[1:])
	case "chat":
		return cmdChat(argv[1:])
	case "sched":
		return cmdSched(argv[1:])
	case "mcp":
		return cmdMCP(argv[1:])
	case "notes":
		return cmdNotes(argv[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", argv[0])
		usage()
		return 1
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `fleet-admin — unified Mega Box admin CLI

Operator lifecycle (bootstrap → update → status):
  fleet-admin bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
  fleet-admin update    [--no-pull] [--client-config <dir>] [--service <name>] [--branch <name>] [--yes] [--dry-run]
  fleet-admin status    [--service <name>] [--no-sandbox]    (a.k.a. doctor; non-zero exit if unhealthy)

Users, credentials, notes:
  fleet-admin chat user add <email>    --password -
  fleet-admin chat user update <email> --password -
  fleet-admin chat user del <email>
  fleet-admin chat user list
  fleet-admin sched user add <username> --role admin|client|readonly --password -
  fleet-admin sched user update <username> --password -
  fleet-admin sched user set-role <username> --role admin|client|readonly
  fleet-admin sched user rename <username> <new-username>
  fleet-admin sched user del <username>
  fleet-admin sched user list
  fleet-admin sched apikey create <name> [--role admin]
  fleet-admin sched apikey list
  fleet-admin sched apikey revoke <key-id>
  fleet-admin sched apikey delete <key-id>
  fleet-admin mcp account set <server> <account> --secret KEY=-   (value via stdin)
  fleet-admin mcp account list <server>
  fleet-admin mcp account del <server> <account>
  fleet-admin notes set <slug> --title "..."  (body via stdin)
  fleet-admin notes get <slug>
  fleet-admin notes list [--all]
  fleet-admin notes rm <slug>
  fleet-admin notes proposal publish <id> [--note "..."]
  fleet-admin notes proposal reject  <id> --reason "..."

Connection:
  Chat DB:  --database-url or FLEET_CHAT_DATABASE_URL / DATABASE_URL
  Sched DB: --database-url or FLEET_SCHED_DATABASE_URL / DATABASE_URL
  Env file: --env-file or FLEET_ENV_FILE (default .env.local) for mcp account

bootstrap + update wrap scripts/bootstrap.sh + scripts/update.sh (found via
FLEET_ROOT, ./scripts, or the binary's dir). status runs read-only checks
in-process: both DBs reachable, the sandbox image present + runnable, required
env vars set, the client bundle loads, and the systemd unit state.

Passwords are read from stdin with --password - (never on argv).
`)
}
