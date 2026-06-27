// Command fleet-admin is the unified admin CLI for a fleet deployment. It folds chat's
// chat-admin (chat users) and moc's -create-user/-set-role flags (sched users +
// API keys) into one tool, plus MCP credential-account management, the notes
// wiki admin verbs, and a thin bootstrap wrapper.
//
// Subcommands:
//
//	fleet-admin bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
//	fleet-admin update    [--no-pull] [--client-config <dir>] [--service <name>] [--yes] [--dry-run]
//	fleet-admin status    [--service <name>] [--no-sandbox]
//	fleet-admin restart|stop [--service <name>]
//	fleet-admin logs      [--service <name>] [-n 50] [-f]   (a.k.a. tail)
//	fleet-admin chat user add|update|del|list
//	fleet-admin sched user add|update|set-role|rename|del|list
//	fleet-admin sched apikey create|list|revoke|delete
//	fleet-admin sched task export|import|set-model
//	fleet-admin mcp account set|list|del
//	fleet-admin notes set|get|list|rm
//	fleet-admin notes proposal publish|reject
//	fleet-admin backup  [--db=chat|sched|all] [--out DIR]
//	fleet-admin restore  --db=chat|sched <dump-file>
//
// The operator lifecycle is bootstrap → update → status: bootstrap provisions a
// box, update rolls a new version in place, status (a.k.a. doctor) reports
// health. bootstrap + update are thin wrappers over scripts/bootstrap.sh +
// scripts/update.sh; status runs in-process read-only checks. restart/stop/logs
// are day-2 conveniences over the host systemd unit (systemctl/journalctl).
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
	case "restart":
		return cmdRestart(argv[1:])
	case "stop":
		return cmdStop(argv[1:])
	case "logs", "tail":
		return cmdLogs(argv[1:])
	case "chat":
		return cmdChat(argv[1:])
	case "sched":
		return cmdSched(argv[1:])
	case "mcp":
		return cmdMCP(argv[1:])
	case "notes":
		return cmdNotes(argv[1:])
	case "backup":
		return cmdBackup(argv[1:])
	case "restore":
		return cmdRestore(argv[1:])
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
	fmt.Fprint(os.Stderr, `fleet-admin — unified fleet admin CLI

Operator lifecycle (bootstrap → update → status):
  fleet-admin bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
  fleet-admin update    [--no-pull] [--client-config <dir>] [--service <name>] [--branch <name>] [--yes] [--dry-run]
  fleet-admin status    [--service <name>] [--no-sandbox]    (a.k.a. doctor; non-zero exit if unhealthy)
  fleet-admin restart   [--service <name>]                   (systemctl restart; needs root/sudo)
  fleet-admin stop      [--service <name>]                   (systemctl stop; needs root/sudo)
  fleet-admin logs      [--service <name>] [-n 50] [-f]      (journalctl tail; -f follows; a.k.a. tail)

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
  fleet-admin sched task export > tasks.json    (versioned JSON of scheduled tasks → stdout)
  fleet-admin sched task import < tasks.json     (recreate tasks from stdin; upsert on id)
  fleet-admin sched task set-model --model <slug> [--fallback-model <slug>] [--from-model <slug>] [--dry-run]
  fleet-admin sched task set-credentials <task_id> --allow server[:account] ... | --clear   (per-task MCP credential allowlist)
  fleet-admin mcp account set <server> <account> --secret KEY=-   (value via stdin)
  fleet-admin mcp account list <server>
  fleet-admin mcp account del <server> <account>
    (account names are canonicalized: hyphen/space fold to underscore and case
     is ignored, so client-a, client_a, and Client_A name ONE seat — use
     distinct base words, not separators, to keep seats apart)
  fleet-admin notes set <slug> --title "..."  (body via stdin)
  fleet-admin notes get <slug>
  fleet-admin notes list [--all]
  fleet-admin notes rm <slug>
  fleet-admin notes proposal publish <id> [--note "..."]
  fleet-admin notes proposal reject  <id> --reason "..."

Backup / restore (pg_dump -Fc / pg_restore; one dump file per DB):
  fleet-admin backup  [--db=chat|sched|all] [--out DIR]   (writes fleet-<db>-<stamp>.dump; prints each path)
  fleet-admin restore  --db=chat|sched <dump-file>         (--clean --if-exists; overwrites the live DB)

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
