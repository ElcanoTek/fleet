# `fleet env` — inspect + edit the deployment env files

`fleet env` is the operator convenience for the two 0600 env files a deployed
box runs on, patterned after gig's `gig env`: **show** prints them with every
secret value masked, **edit** opens one in an editor without ever echoing a
secret to the terminal. It complements — never replaces — the guided writers
(`fleet config set-openrouter-key|set-auth-pubkey|set-browserbase-key`,
`fleet mcp account set`), which remain the right tool for single credential
writes because they take the value on stdin and quote it correctly.

## The two files

| File | Resolution | Read by |
|---|---|---|
| server env | `--env-file`, else `FLEET_ENV_FILE`, else `/etc/fleet/fleet.env` when `/etc/fleet` exists, else `.env.local` | `deploy/fleet.service` (`EnvironmentFile=`) and `config.Load` — LLM keys, DSNs, MCP connector creds, tuning knobs ([OPERATORS.md](OPERATORS.md)) |
| web env | `/etc/fleet/fleet-web.env` when `/etc/fleet` exists, else `web/.env.local` | `deploy/fleet-web.service` — the `AUTH_*` SSO keys, the shared token |

The resolution is the same `serverEnvFile`/`webEnvFile` logic the
`fleet config set-*` writers use, so `fleet env` always targets the file those
commands write and the unit actually reads.

## `fleet env [show]`

Prints both files, one `# path` header each, keys sorted. A missing file is a
note, not an error. Values are masked twice over:

- a key whose NAME matches the credential heuristic
  (`KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL`) prints as `[REDACTED]` —
  the SAME exported heuristic (`redact.IsSecretEnvName`) the diagnose
  bundle's scrubber seeds from, so the two can never drift;
- any surviving value with URL userinfo (`scheme://user:pass@host`) has the
  userinfo stripped to `***@`, so a Postgres DSN shows host + database but
  never the password.

Parsing reuses `creds.ReadEnvValues`, which mirrors the server's own env-file
parser — what show renders (post-masking) is byte-for-byte what the server
would load. Reading a root-owned 0600 file without root is an error naming the
`sudo` fix.

## `fleet env edit`

```
fleet env edit [--web] [--env-file <path>] [--editor <cmd>]
```

1. Resolves the server file (or the web file with `--web`), creating it 0600 —
   parent dir 0700 — when missing, and fails up front with a
   `sudo fleet env edit` hint when the file isn't writable (better than an
   editor session whose save fails).
2. Resolves the editor: `--editor`, else `$VISUAL`, else `$EDITOR` (values are
   whitespace-split, so `EDITOR="code -w"` works), else — on a TTY — an
   interactive pick of nano / vim / helix. A picked-but-missing editor is
   offered via `dnf install -y` (fleet targets Fedora boxes; helix's binary is
   `hx`). Non-TTY with nothing configured is a hard error, so scripts fail
   loudly instead of hanging.
3. Runs the editor on the file, then **restores 0600** — vim's default
   write-via-rename would otherwise leave the secrets file at the umask mode.
4. If the content didn't change, says so and stops. Otherwise it warns about
   lines the server's parser would silently skip (non-`KEY=VALUE` lines) and
   duplicate keys (the later line wins), then prints the apply hints:
   `fleet validate-config` to preflight and `fleet restart` to apply for the
   server file (reloadable ceilings also apply live via `SIGUSR2` —
   [CONFIG-RELOAD.md](CONFIG-RELOAD.md)); `systemctl restart fleet-web` for
   the web file.

## `fleet config set-env` / `unset-env` — one key, no editor

For a single variable — a callback token, a connector credential the bundle
names, a knob — there is no need to open the file:

```sh
printf '%s' "$TOKEN" | sudo fleet config set-env MANIFEST_CALLBACK_TOKEN
sudo fleet config set-env FLEET_TRUSTED_PROXIES --value 127.0.0.1,::1
sudo fleet config set-env NEXT_PUBLIC_APP_NAME --web       # the web-tier file; hidden prompt on a TTY
sudo fleet config unset-env OLD_KEY
```

The value is read from stdin (`--value -`, or a pipe) or a hidden prompt —
never from argv, so it stays out of shell history. The write goes through the
same helper the credential verbs use (`creds.SetEnvKey`): the result has
**exactly one** line for the key (every duplicate is removed — the server
loads last-assignment-wins, so a first-only replacement used to leave a stale
later line in force and the "set" silently did nothing), the file is written
atomically and kept `0600`, and **its owner is unchanged**. The key must match
`[A-Za-z_][A-Za-z0-9_]*`, the value may not contain a line break.

## Ownership

Both editors leave the file owned by whoever owned it before. `fleet env edit`
records the owner before launching the editor and restores it afterwards
(along with `0600`), because an editor that saves via rename — vim's default —
otherwise hands the new inode to the editing user, root under `sudo`. The
`set-*`/`set-env` writers do the same on their temp+rename. Ownership is only
ever touched when running as root and it actually changed; a non-root run can
only have written a file it already owned. On the shipped units the server
file is `root:root 0600` (systemd reads it as root) and stays that way.

## Honest scope

- **No value validation beyond the line lint.** `fleet validate-config` is the
  real preflight (it runs the same loaders the server boots through); edit
  only catches the two mistakes the parser would otherwise eat silently.
- **The lint is warn-only.** An operator mid-refactor should not be blocked;
  nothing is written back or rewritten by fleet — the editor's bytes are the
  operator's bytes.
- **show masks by key name.** A secret stored under a name the heuristic does
  not match (e.g. `MY_OPAQUE_VALUE`) prints in the clear — the same honest
  limitation the diagnose scrubber has, which is why credential seats should
  keep their conventional `_KEY`/`_TOKEN` suffixes.
- **No systemd integration.** edit never restarts anything; it prints the
  apply hints and leaves the restart decision to the operator.
