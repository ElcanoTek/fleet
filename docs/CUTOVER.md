# v1 → fleet cutover runbook (a box already running the legacy chat + moc stack)

[`docs/LEGACY-IMPORT.md`](LEGACY-IMPORT.md) specifies the data migration
(export bundles → `fleet import`). This page is the **operational** runbook
around it: how to take a box (or boxes) that is *currently serving* the legacy
**chat** and **moc** apps and cut it over to one fleet, without locking
yourself out of the legacy databases, resurrecting the old stack on reboot, or
truncating the box's Caddy config. Follow the steps **in order** — the
ordering is load-bearing.

On a *fresh* box with no legacy install, ignore this page and use
[`docs/DEPLOYMENT.md`](DEPLOYMENT.md).

## 0. Know what collides

fleet's defaults were inherited from the legacy apps, so on a legacy box
**everything** collides:

| Resource | legacy chat | legacy moc | fleet default | fleet override |
| --- | --- | --- | --- | --- |
| Chat backend port | chat-server `127.0.0.1:8080` | — | `127.0.0.1:8080` | `FLEET_SERVER_ADDR` |
| Orchestrator port | — | moc `127.0.0.1:8000` | `127.0.0.1:8000` | `FLEET_ORCHESTRATOR_ADDR` |
| Web tier port | chat-web `:3000` | — | fleet-web `:3000` | `PORT` in `/etc/fleet/fleet-web.env` (+ matching `CHAT_SERVER_URL`/`ORCHESTRATOR_SERVER_URL`) |
| Postgres role + DB | role `chat`, db `chat` | role `moc`, db `moc` | role `chat`, db `chat` (+ role `sched`, db `sched`) | `--chat-db-name/--chat-db-user` etc. |
| `/etc/caddy/Caddyfile` | written by the legacy bootstraps (when Caddy TLS was chosen) | same | written by `--enable-web --domain` | `--force-caddy` (with a timestamped backup) |

Two protections in `scripts/bootstrap.sh` exist specifically for this
scenario — do not work around them:

- It **refuses** to provision when a Postgres role/database matching its
  configured names already exists and was not created by a previous fleet
  bootstrap (i.e. is not recorded in fleet's env file). Without the guard, the
  legacy `chat` role's password would be rotated — locking out the
  still-installed legacy server *and* the `chat-admin export` you have not run
  yet — and fleet's migrations would run on the legacy database.
- It **refuses** to overwrite an `/etc/caddy/Caddyfile` it did not write
  (`--force-caddy` overrides, keeping a timestamped backup and printing a
  merge warning).

## 1. Back up the legacy state first

Before touching anything, snapshot what exists — both databases *and* the
on-disk state that is not in any database:

```sh
# Legacy DB dumps (each app's own tooling, while it still works):
sudo chat backup                 # pg_dump of the legacy chat DB
sudo moc  backup                 # pg_dump of the legacy moc DB

# On-disk state the dumps do NOT contain:
sudo tar -C / -czf /root/legacy-state-$(date +%F).tar.gz \
    opt/chat/data opt/moc/data   # chat: email attachments + uploads; moc: task files
```

Keep these until well after the cutover has soaked.

## 2. Stop AND disable the legacy stack

`systemctl stop` is not enough: every legacy unit is
`WantedBy=multi-user.target`, so a reboot resurrects the whole stack — port
fights on 8080/8000/3000 with fleet, and the legacy scheduler double-firing
tasks you have already migrated into fleet. **Disable** them:

```sh
# Orchestrator + chat box (the target itself always disables the units, not
# just the target — stopping/disabling chat.target alone leaves the two
# services enabled in their own right):
sudo systemctl disable --now chat.target chat-server.service chat-web.service
sudo systemctl disable --now moc.service

# EVERY worker box: the legacy runner daemon (gig) restarts forever, polling
# an orchestrator that no longer exists (fleet has no runner nodes). Disable
# it however it is supervised there, e.g.:
sudo systemctl disable --now gig.service
```

Verify nothing legacy is listening before proceeding:

```sh
sudo ss -tlnp | grep -E ':8080|:8000|:3000' || echo "ports clear"
```

Users are now dark until step 6 — schedule the window accordingly. (If you
need staged coexistence instead of a hard cutover, see step 7 first.)

## 3. Export the migration bundles

Export **after** stopping the legacy services (no writes racing the export)
and **before** bootstrapping fleet. Each exporter reads its own app's env file
for its DB DSN — these are the *legacy* env files, not fleet's:

```sh
sudo -u chat sh -c 'cd /opt/chat && set -a && . ./.env.local && set +a && \
    ./bin/chat-admin export --out /opt/chat/data/chat-bundle.json'
sudo -u moc  sh -c 'cd /opt/moc  && set -a && . ./.env      && set +a && \
    ./moc -export-fleet /opt/moc/data/moc-bundle.json'
```

## 4. Bootstrap fleet with an explicit database decision

Run bootstrap as documented in [`DEPLOYMENT.md`](DEPLOYMENT.md), plus the
cutover-specific choices. **Recommended: fresh DB names + import** (the
supported migration path — the legacy databases stay untouched as a fallback):

```sh
sudo bash /opt/fleet/src/scripts/bootstrap.sh \
  --postgres=local --enable-web --domain fleet.example.com \
  --client-config <your-bundle-url> \
  --chat-db-name fleet_chat --chat-db-user fleet_chat
```

(`sched`/`sched` does not collide with moc's `moc`/`moc`, so only the chat
names need overriding; add `--sched-db-name/--sched-db-user` if your legacy
install used non-default names that collide.)

The alternative is **in-place adoption** — `--adopt-existing-chat-db` with
`FLEET_CHAT_DATABASE_URL=<the legacy DSN, current password>`. Bootstrap then
never creates or alters that role (no password rotation) and fleet runs its
own migrations on that database at first start. Only adopt a database that is
meant to *become* fleet's: you skip the chat export/import, but you also give
up the untouched-legacy-DB fallback, and a later `fleet import` of a chat
bundle exported from that same database is a no-op (every row already
exists). When in doubt, use fresh names.

**Caddy:** the legacy bootstrap wrote `/etc/caddy/Caddyfile`, so
`--enable-web --domain` will refuse. Re-run with `--force-caddy` once you are
sure nothing else on the box needs the old config — the previous file is
saved as `/etc/caddy/Caddyfile.fleet-backup.<timestamp>`. To keep the old
hostname working, add a redirect vhost to `/etc/caddy/Caddyfile` afterwards
and `systemctl reload caddy`:

```
chat.example.com {
	redir https://fleet.example.com{uri} permanent
}
```

Note: the fleet-managed Caddyfile is rewritten by every
`--enable-web --domain` re-run (its first line says so) — re-apply hand-added
blocks like this redirect after a re-run.

## 5. Import the bundles (dry-run first)

On a systemd deploy the DSNs live in **`/etc/fleet/fleet.env`** (0600,
root-only) — *not* a `.env.local` — so source it explicitly (or pass
`--chat-database-url`/`--sched-database-url`):

```sh
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/chat/data/chat-bundle.json --dry-run'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/chat/data/chat-bundle.json'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/moc/data/moc-bundle.json --dry-run'
sudo sh -c 'set -a && . /etc/fleet/fleet.env && set +a && \
    fleet import /opt/moc/data/moc-bundle.json'
```

Read the dry-run plans before the real runs. Import semantics (idempotent
re-runs, what migrates, what deliberately does not) are specified in
[`LEGACY-IMPORT.md`](LEGACY-IMPORT.md).

## 6. Copy the non-DB state

The bundles carry database rows only. Two kinds of on-disk state must move by
hand:

**moc task files** — tasks whose `files` reference uploads under moc's data
dir (`fleet import` prints the count of tasks with file references). fleet
serves task files from `<data dir>/temp_uploads` (on a systemd deploy:
`/var/lib/fleet/data/temp_uploads`):

```sh
sudo install -d -o fleet -g fleet /var/lib/fleet/data/temp_uploads
sudo cp -a /opt/moc/data/temp_uploads/. /var/lib/fleet/data/temp_uploads/
sudo chown -R fleet:fleet /var/lib/fleet/data/temp_uploads
```

**The legacy chat data dir (email attachments + uploaded images)** — migrated
message history references these files by their **absolute paths on the
legacy box** (e.g. under `/opt/chat/data/attachments`). fleet re-reads those
exact paths when replaying a conversation's images; a missing or unreadable
file is silently dropped from replay (the conversation text is unaffected).
So do not relocate them — leave them at their original path and make them
readable by the `fleet` service user:

```sh
sudo chown -R fleet:fleet /opt/chat/data
# The fleet user must also be able to TRAVERSE the parent directory:
ls -ld /opt/chat            # needs o+x (or group access) for the fleet user
```

(fleet's systemd hardening mounts the OS read-only outside its state dir, but
read access to `/opt/chat/data` is enough — history replay only reads. If you
instead delete the legacy data dir, migrated conversations keep their text
and lose their inline images.) New uploads land under fleet's own data dir,
`/var/lib/fleet/data/attachments` — which is also why fleet backups need more
than the DB dumps; see [`BACKUP_RESTORE.md`](BACKUP_RESTORE.md).

Restart and verify:

```sh
sudo fleet restart && fleet status
```

## 7. Ports and Caddy if you must coexist (staged cutover)

The recommended path is the hard cutover above — legacy stopped before fleet
starts, no overlap. If you must run both stacks side by side (e.g. a pilot
group on fleet while the org stays on legacy chat), move **fleet's**
listeners, since the legacy apps predate their own override knobs being
documented:

- `/etc/fleet/fleet.env`: `FLEET_SERVER_ADDR=127.0.0.1:18080` and
  `FLEET_ORCHESTRATOR_ADDR=127.0.0.1:18000`.
- `/etc/fleet/fleet-web.env`: `PORT=13000`, and point `CHAT_SERVER_URL` /
  `ORCHESTRATOR_SERVER_URL` at the moved backends. Note that every
  `bootstrap.sh --enable-web` re-run writes the default
  `CHAT_SERVER_URL`/`ORCHESTRATOR_SERVER_URL`/`PORT` values back into this
  file — re-apply these edits after one, or finish the cutover promptly.
- Give fleet its own hostname in Caddy rather than fighting over the legacy
  one; **merge** the fleet vhost into the existing Caddyfile by hand instead
  of using `--force-caddy` (which replaces the file).
- Restart: `sudo systemctl restart fleet fleet-web && sudo systemctl reload caddy`.

Never let both schedulers own the same recurring tasks: import into fleet
only after the legacy scheduler is disabled, or keep the pilot to interactive
chat.

## 8. Mint fleet API keys and re-point external intake callers

moc's file-based API keys are **not** migrated ([`LEGACY-IMPORT.md`](LEGACY-IMPORT.md)),
so every external system that created tasks against the legacy orchestrator
needs a fresh fleet key and the new endpoint:

```sh
fleet sched apikey create ci-bot --type task     # prints the key exactly once
```

- Callers on the box hit the orchestrator directly at `127.0.0.1:8000`.
- Remote callers go through the web tier, which forwards a caller-supplied
  `Authorization: Bearer <key>` verbatim:
  `https://fleet.example.com/api/orchestrator/tasks`. See
  [`BUILDING-ON-FLEET.md`](BUILDING-ON-FLEET.md) for the API tour.
- Event-driven intake (inbound webhooks → tasks/conversations) is configured
  fresh in fleet — [`EVENT-TRIGGERS.md`](EVENT-TRIGGERS.md) /
  [`WEBHOOKS.md`](WEBHOOKS.md); update the calling systems' URLs and secrets.
- Users log in again (sessions don't migrate; passwords do). Promote admins
  with `fleet admin add <email>` if you didn't during bootstrap.

## 9. DNS

Point the public hostname(s) at the box/fleet vhost: either move the old
name(s) to the fleet Caddyfile as redirects (step 4) or update DNS to the new
`fleet.example.com` and communicate the new URL. Lower the records' TTL ahead
of the window if you are also changing boxes.

## 10. Post-cutover cleanup (after the soak)

Once fleet has run clean for your comfort window (and you have a **verified**
fleet backup — [`BACKUP_RESTORE.md`](BACKUP_RESTORE.md)):

```sh
# Stale legacy backup jobs — otherwise cron keeps dumping the legacy (or, if
# you adopted it, now-fleet-owned) chat DB with the old tooling:
sudo rm -f /etc/cron.daily/chat-backup      # or wherever you installed it
# ...and any moc backup cron/timer you added.

# Orphan legacy databases + roles (NOT if you adopted that database!):
sudo -u postgres dropdb chat  && sudo -u postgres dropuser chat
sudo -u postgres dropdb moc   && sudo -u postgres dropuser moc

# Legacy units, CLIs, and system users:
sudo rm -f /etc/systemd/system/{chat.target,chat-server.service,chat-web.service,moc.service}
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/chat /usr/local/bin/moc
sudo userdel moc                             # keep the `chat` user until you
                                             # retire /opt/chat/data (step 6 —
                                             # or chown it to fleet first)

# Source trees + data dirs — ONLY once nothing references them; remember step
# 6: /opt/chat/data holds the attachment files migrated history replays.
# sudo rm -rf /opt/chat-src /opt/moc /opt/chat

# Worker boxes: gig + its container images/workspaces, then retire the box.
```

Finally, install fleet's own daily backup timer
([`BACKUP_RESTORE.md`](BACKUP_RESTORE.md)) and take one full backup —
including the on-disk state under `/var/lib/fleet` — before you delete
anything legacy.
