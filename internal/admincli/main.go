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
//	fleet doctor    [--check] [--no-restart] [--node] [--dry-run]
//	fleet diagnose  [--output <file>] [--service <name>] [--no-sandbox]
//	fleet start|restart|stop [--service <name>]
//	fleet logs      [--service <name>] [-n 50] [-f]   (a.k.a. tail)
//	fleet timers install [--backup] [--maintenance] [--src <dir>] [--dry-run]
//	fleet chat                                        (interactive agent TUI, #457; --message for one-shot)
//	fleet admin add|list|rm                           (one-step full admin across both user planes)
//	fleet config set-openrouter-key|set-auth-pubkey|set-browserbase-key|set-env|unset-env   (guided credential/env-file writes)
//	fleet env [show|edit]                             (print the env files secrets-masked / open one in an editor)
//	fleet chat user add|update|role|del|list
//	fleet sched user add|update|set-role|rename|del|list
//	fleet sched apikey create|list|revoke|rotate|delete
//	fleet sched task list|export|import|set-model|set-credentials|set-description|set-limits|tag|estimate|batch-create
//	fleet sched trigger create|list|delete|rotate
//	fleet sched dlq list|replay
//	fleet sched budget list|create|delete
//	fleet task run <task.yaml>   (local one-shot through the governed runtime — dispatched by the fleet binary)
//	fleet task export|import    (definition-only #238: portable JSON/YAML, name-based conflict resolution)
//	fleet task memories list|clear|delete <task_id> [key]
//	fleet mcp account set|list|del
//	fleet notes set|get|list|rm
//	fleet notes proposal publish|reject
//	fleet worktree list|prune [--workspace DIR] [--older-than DUR]
//	fleet backup  [--db=chat|sched|all] [--out DIR]
//	fleet restore  --db=chat|sched <dump-file>
//	fleet import <bundle.json> [--dry-run] [--live-only]
//
// The operator lifecycle is bootstrap → update → status/doctor: bootstrap
// provisions a box, update rolls a new version in place, status reports health
// (quick, read-only, in-process), and doctor diagnoses AND repairs box-level
// drift (wraps scripts/doctor.sh; needs root except --check/--dry-run).
// bootstrap + update are thin wrappers over scripts/bootstrap.sh +
// scripts/update.sh. restart/stop/logs are day-2 conveniences over the host
// systemd unit (systemctl/journalctl).
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
	case "status":
		return cmdStatus(argv[1:])
	case "doctor":
		// Formerly an alias for status; now the box-level diagnose-and-REPAIR
		// pass (wraps scripts/doctor.sh — see cmdDoctor). `doctor --check`
		// keeps the old read-only contract, just deeper than status.
		return cmdDoctor(argv[1:])
	case "diagnose":
		return cmdDiagnose(argv[1:])
	case "start":
		// `fleet start` starts the systemd unit (#722) — the counterpart to
		// stop/restart. Distinct from `fleet serve`, which runs the daemon in
		// the foreground of THIS process.
		return cmdStart(argv[1:])
	case "restart":
		return cmdRestart(argv[1:])
	case "stop":
		return cmdStop(argv[1:])
	case "logs", "tail":
		return cmdLogs(argv[1:])
	case "timers":
		return cmdTimers(argv[1:])
	case "motd":
		return cmdMOTD(argv[1:])
	case "chat":
		return cmdChat(argv[1:])
	case "sched":
		return cmdSched(argv[1:])
	case "admin":
		return cmdAdmin(argv[1:])
	case "config":
		return cmdConfig(argv[1:])
	case "env":
		return cmdEnv(argv[1:])
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
	case "import":
		return cmdImport(argv[1:])
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

Operator lifecycle (bootstrap → update → status/doctor):
  fleet bootstrap [--postgres=local|external] [--client-config <url|path>] [--enable-service] [--dry-run]
  fleet update    [--check] [--no-pull] [--client-config <dir>] [--service <name>] [--branch <name>] [--adopt-units] [--no-node-repair] [--yes] [--dry-run]
                                                             (--check: read-only "N commits behind upstream" + can this box build the
                                                              web tier; mutates nothing, non-zero when the node floor is unmet;
                                                              --adopt-units: adopt shipped systemd units that drifted, without the prompt;
                                                              --no-node-repair: refuse instead of repairing a node shortfall in place)
  fleet cleanup   [--dry-run] [--deep]                 (reclaim build cruft: dangling podman layers + Go caches)
  fleet status    [--service <name>] [--no-sandbox]    (quick read-only health report; non-zero exit if unhealthy)
  fleet doctor    [--check] [--no-restart] [--node] [--dry-run]  (deep box pass: diagnose AND repair drift — packages, rootless
                                                             podman prereqs, unit drift, env files, services, sandbox smoke.
                                                             Needs root; --check only diagnoses; --dry-run prints the checklist;
                                                             --node repairs ONLY the node toolchain and exits, and
                                                             --node --check is a read-only probe that needs no root)
  fleet diagnose  [--output <file>] [--service <name>] [--no-sandbox]
                                                             (redacted support bundle: status + config names + DB versions + sandbox image → .tar.gz)
  fleet start     [--service <name>]                   (systemctl start; needs root/sudo — distinct from "fleet serve", which runs the daemon in the foreground)
  fleet restart   [--service <name>]                   (systemctl restart; needs root/sudo)
  fleet stop      [--service <name>]                   (systemctl stop; needs root/sudo)
  fleet logs      [--service <name>] [-n 50] [-f]      (journalctl tail; -f follows; a.k.a. tail)
  fleet timers install [--backup] [--maintenance] [--dry-run]
                                                        (install + enable the daily fleet-backup / fleet-maintenance
                                                         systemd timers from deploy/; idempotent, never overwrites an
                                                         installed unit. Needs root except --dry-run; on a box without
                                                         systemd it says what to schedule instead)
  fleet motd      [--service <name>] [--no-color]      (login banner: version + service state + commands; no secrets)

Users, credentials, notes:
  fleet admin add <email>                             (ONE step: web login + chat-admin + Operations Center admin; prompts for password)
  fleet admin list                                    (every chat login + whether it's an Operations Center admin)
  fleet admin rm <email>                              (remove from both planes)
  fleet config set-openrouter-key                     (hidden prompt; or --key -; upserts OPENROUTER_API_KEY into the server env file)
  fleet config set-browserbase-key                    (hidden prompt; or --key -; upserts BROWSERBASE_API_KEY into the server env file —
                                                       mints hosted-browser live views; see docs/BROWSERBASE.md)
  fleet config set-auth-pubkey [<key>|--from <file>]  (enable Elcano SSO: validates + writes AUTH_SIGNING_PUBKEY into the web env file;
                                                       accepts the "auth pubkey" output line verbatim; --login-url/--cookie-domain optional)
  fleet config set-env <KEY> [--value -] [--web]      (upsert ANY key into the server — or web — env file as exactly one line:
                                                       duplicates removed, 0600 + owner kept; value from stdin/hidden prompt, never argv)
  fleet config unset-env <KEY> [--web]                (remove every line for KEY)
  fleet env [show]                                    (print the server + web env files with secret values masked)
  fleet env edit [--web] [--editor CMD]               (open the server env file — or the web one — in $EDITOR, or pick
                                                       nano/vim/helix interactively; offered via dnf install if missing.
                                                       Restores 0600 and prints the validate/restart apply hints)
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
  fleet sched apikey create <name> [--type admin|task|webhook|readonly] [--rate-limit-per-minute N] [--trigger-slugs a,b] [--role admin]
                                                      (typed keys are fleet_<type>_<base58>, legacy --role keys sk-…; send as X-API-Key.
                                                       Writes the store fleet.service reads — announced as "key store:"; FLEET_DATA_DIR overrides)
  fleet sched apikey list
  fleet sched apikey revoke <key-id>
  fleet sched apikey rotate <key-id> [--grace-hours 24]   (fresh secret, same type/scope; old key valid through the grace window)
  fleet sched apikey delete <key-id>
  fleet sched task list [--status scheduled] [--limit 50] [--json]   (most recent first; the daily-driver read)
  fleet sched task export > tasks.json    (versioned JSON of scheduled tasks → stdout)
  fleet sched task import [--replace-status] < tasks.json
                                                 (recreate tasks from stdin; upsert on id. An id that already
                                                  exists here is a write over live state: a status collision is
                                                  refused unless --replace-status is passed, a running/leased
                                                  task is never written over, and lease columns are never
                                                  imported onto an existing row — docs/OPERATORS.md)
  fleet sched task batch-create --from-file <file> [--atomic]
                                                 (submit multiple tasks atomically or best-effort from a JSON file)
  fleet sched task set-model --model <slug> [--fallback-model <slug>] [--from-model <slug>] [--dry-run] [--no-confirm]
  fleet sched task set-credentials <task_id> --allow server[:account] ... | --clear   (per-task MCP credential allowlist)
  fleet sched task set-description <task_id> <text>|-    (operator docs; - reads stdin, e.g. < TASK_README.md)
  fleet sched task set-limits <task_id> --memory-mb N --cpus N --pids N | --clear
                                                 (per-task sandbox cgroup override; --clear reverts to global defaults)
  fleet sched task tag <task_id> --add <tag> ... --remove <tag> ...   (organize tasks by label)
  fleet sched task estimate --model <slug> --prompt <text> [--max-iter N] [--mcp-tools N] [--max-cost USD] [--system-prompt <text>] [--json]   (pre-submission cost forecast; no DB, no model call)
  fleet sched trigger create --task <task_id> --slug <slug> [--kind webhook|email] | list | rotate <trigger_id> | delete <trigger_id>   (event triggers; #177/#511)
  fleet sched dlq list [--tag <tag>] [--limit N] [--json] | replay <task_id>   (dead-letter queue review/replay; #253)
  fleet sched budget list | create --scope user|key --principal <id> --window day|week|month [--soft-usd N] [--hard-usd N] [--soft-tokens N] [--hard-tokens N] | delete <budget_id>
  fleet task run <task.yaml> [--log FILE] [--workspace DIR]   (local one-shot through the governed runtime)
  fleet task memories list|clear|delete <task_id> [key]   (inspect/reset a task's Captain's Log memory; #198)
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

Legacy migration (one-time; docs/LEGACY-IMPORT.md):
  fleet import <bundle.json> [--dry-run] [--live-only]   (ingest a chat/moc migration bundle; idempotent re-runs)

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
