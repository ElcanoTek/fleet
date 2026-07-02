// Package admincli is the operator/admin command dispatch for the unified
// `fleet` CLI (#461). It folds chat's chat-admin (chat users) and moc's
// -create-user/-set-role flags (sched users + API keys) into one tool, plus MCP
// credential-account management, the notes wiki admin verbs, and the
// bootstrap/update/status lifecycle. Both the unified `fleet` binary (as
// `fleet <verb>`) and the transitional `fleet-admin` deprecation shim call
// Run; nothing here boots the server (that is `fleet serve`).
//
// Subcommands (invoked as `fleet <verb>`; `fleet-admin <verb>` still works for
// one deprecation release):
//
//	fleet bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
//	fleet update    [--no-pull] [--client-config <dir>] [--service <name>] [--yes] [--dry-run]
//	fleet status    [--service <name>] [--no-sandbox]
//	fleet diagnose  [--output <file>] [--service <name>] [--no-sandbox]
//	fleet restart|stop [--service <name>]
//	fleet logs      [--service <name>] [-n 50] [-f]   (a.k.a. tail)
//	fleet chat                                        (interactive agent TUI, #457; --message for one-shot)
//	fleet chat user add|update|role|del|list
//	fleet sched user add|update|set-role|rename|del|list
//	fleet sched apikey create|list|revoke|delete
//	fleet sched task export|import|set-model|set-credentials|set-description|tag|estimate|batch-create
//	fleet task export|import    (definition-only #238: portable JSON/YAML, name-based conflict resolution)
//	fleet mcp account set|list|del
//	fleet notes set|get|list|rm
//	fleet notes proposal publish|reject
//	fleet worktree list|prune [--workspace DIR] [--older-than DUR]
//	fleet backup  [--db=chat|sched|all] [--out DIR]
//	fleet restore  --db=chat|sched <dump-file>
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
package admincli

import (
	"fmt"
	"os"

	"github.com/ElcanoTek/fleet/internal/version"
)

// Run dispatches one admin/operator subcommand (argv[0] is the verb) and returns
// the process exit code. It is the single entry point both the unified `fleet`
// binary and the `fleet-admin` shim call.
func Run(argv []string) int {
	if len(argv) == 0 {
		Usage()
		return 1
	}
	switch argv[0] {
	case "bootstrap":
		return cmdBootstrap(argv[1:])
	case "update":
		return cmdUpdate(argv[1:])
	case "cleanup":
		return cmdCleanup(argv[1:])
	case "status", "doctor":
		return cmdStatus(argv[1:])
	case "diagnose":
		return cmdDiagnose(argv[1:])
	case "restart":
		return cmdRestart(argv[1:])
	case "stop":
		return cmdStop(argv[1:])
	case "logs", "tail":
		return cmdLogs(argv[1:])
	case "motd":
		return cmdMOTD(argv[1:])
	case "chat":
		return cmdChat(argv[1:])
	case "sched":
		return cmdSched(argv[1:])
	case "task":
		return cmdTask(argv[1:])
	case "mcp":
		return cmdMCP(argv[1:])
	case "notes":
		return cmdNotes(argv[1:])
	case "worktree":
		return cmdWorktree(argv[1:])
	case "migrate":
		return cmdMigrate(argv[1:])
	case "backup":
		return cmdBackup(argv[1:])
	case "restore":
		return cmdRestore(argv[1:])
	case "version", "--version", "-v":
		// Build identity: the release version stamped from the top-level VERSION
		// file plus the VCS revision. Touches no DB/host, so it works anywhere.
		fmt.Println("fleet " + version.String())
		return 0
	case "-h", "--help", "help":
		Usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", argv[0])
		Usage()
		return 1
	}
}

func Usage() {
	fmt.Fprint(os.Stderr, `fleet — unified operator CLI  (run "fleet serve" to start the server)

Chat with the agent (TUI, #457):
  fleet chat                                          (interactive Bubble Tea chat with the fleet agent)
  fleet chat --message "<text>" [--no-tui]            (one-shot: stream the reply to stdout; scriptable)
  fleet chat [--conversation <id>] [--model <slug>] [--email …] [--server …] [--token-file <path>]

Operator lifecycle (bootstrap → update → status):
  fleet bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
  fleet update    [--check] [--no-pull] [--client-config <dir>] [--service <name>] [--branch <name>] [--yes] [--dry-run]
                                                             (--check: read-only "N commits behind upstream", mutates nothing)
  fleet cleanup   [--dry-run] [--deep]                 (reclaim build cruft: dangling podman layers + Go caches)
  fleet status    [--service <name>] [--no-sandbox]    (a.k.a. doctor; non-zero exit if unhealthy)
  fleet diagnose  [--output <file>] [--service <name>] [--no-sandbox]
                                                             (redacted support bundle: status + config names + DB versions + sandbox image → .tar.gz)
  fleet restart   [--service <name>]                   (systemctl restart; needs root/sudo)
  fleet stop      [--service <name>]                   (systemctl stop; needs root/sudo)
  fleet logs      [--service <name>] [-n 50] [-f]      (journalctl tail; -f follows; a.k.a. tail)
  fleet motd      [--service <name>] [--no-color]      (login banner: version + service state + commands; no secrets)

Users, credentials, notes:
  fleet chat user add <email>    --password -
  fleet chat user update <email> --password -
  fleet chat user role <email>   --role member|viewer|admin [--team <id>]
  fleet chat user del <email>
  fleet chat user list
  fleet sched user add <username> --role admin|client|readonly --password -
  fleet sched user update <username> --password -
  fleet sched user set-role <username> --role admin|client|readonly
  fleet sched user rename <username> <new-username>
  fleet sched user del <username>
  fleet sched user list
  fleet sched apikey create <name> [--role admin]
  fleet sched apikey list
  fleet sched apikey revoke <key-id>
  fleet sched apikey delete <key-id>
  fleet sched task export > tasks.json    (versioned JSON of scheduled tasks → stdout)
  fleet sched task import < tasks.json     (recreate tasks from stdin; upsert on id)
  fleet sched task batch-create --from-file <file> [--atomic]
                                                 (submit multiple tasks atomically or best-effort from a JSON file)
  fleet sched task set-model --model <slug> [--fallback-model <slug>] [--from-model <slug>] [--dry-run]
  fleet sched task set-credentials <task_id> --allow server[:account] ... | --clear   (per-task MCP credential allowlist)
  fleet sched task set-description <task_id> <text>|-    (operator docs; - reads stdin, e.g. < TASK_README.md)
  fleet sched task tag <task_id> --add <tag> ... --remove <tag> ...   (organize tasks by label)
  fleet sched task estimate --model <slug> --prompt <text> [--max-iter N] [--mcp-tools N] [--max-cost USD] [--system-prompt <text>] [--json]   (pre-submission cost forecast; no DB, no model call)
  fleet task export [--ids uuid1,uuid2] [--format json|yaml] [--recurrence-only]   (definition-only export → stdout; #238)
  fleet task import [--from tasks.yaml] [--format json|yaml] [--dry-run] [--conflict error|skip|replace]   (definition-only import; #238)
  fleet mcp account set <server> <account> --secret KEY=-   (value via stdin)
  fleet mcp account list <server>
  fleet mcp account del <server> <account>
  fleet mcp reload [--server <addr>] [--admin-key <key>] [--json]
    (hot-reload the MCP catalog without a restart (#218); re-reads the bundle
     and applies server add/remove/restart to the live agent. Equivalent to
     kill -HUP. Uses ADMIN_API_KEY / FLEET_ORCHESTRATOR_ADDR by default.)
    (account names are canonicalized: hyphen/space fold to underscore and case
     is ignored, so client-a, client_a, and Client_A name ONE seat — use
     distinct base words, not separators, to keep seats apart)
  fleet notes set <slug> --title "..."  (body via stdin)
  fleet notes get <slug>
  fleet notes list [--all]
  fleet notes rm <slug>
  fleet notes proposal publish <id> [--note "..."]
  fleet notes proposal reject  <id> --reason "..."

Git worktree isolation hygiene (#180; tasks with worktree_config enabled):
  fleet worktree list  [--workspace DIR]                       (git worktree list --porcelain)
  fleet worktree prune [--workspace DIR] [--older-than 24h] [--dry-run]
    (git worktree prune + remove stale <workspace>/.fleet-worktrees/* dirs)

Database migrations (#256):
  fleet migrate status [--database-url <dsn>] [--json]   (read-only: applied vs pending for the chat + sched DBs)

Backup / restore (pg_dump -Fc / pg_restore; one dump file per DB):
  fleet backup  [--db=chat|sched|all] [--out DIR]   (writes fleet-<db>-<stamp>.dump; prints each path)
  fleet restore  --db=chat|sched <dump-file>         (--clean --if-exists; overwrites the live DB)

Connection:
  Chat DB:  --database-url or FLEET_CHAT_DATABASE_URL / DATABASE_URL
  Sched DB: --database-url or FLEET_SCHED_DATABASE_URL / DATABASE_URL
  Env file: --env-file or FLEET_ENV_FILE (default .env.local) for mcp account

bootstrap + update wrap scripts/bootstrap.sh + scripts/update.sh (found via
FLEET_ROOT, ./scripts, or the binary's dir). status runs read-only checks
in-process: both DBs reachable, the sandbox image present + runnable, required
env vars set, the client bundle loads, and the systemd unit state.

Passwords are read from stdin with --password - (never on argv).

  fleet version                                       (print build version + VCS revision; a.k.a. --version)
`)
}
