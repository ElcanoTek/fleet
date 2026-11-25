# ADR-0122: One `fleet serve` owns the database, enforced in Postgres

- **Status:** Accepted
- **Date:** 2026-11-25
- **Deciders:** fleet maintainers
- **Enforces:** the single-owner control plane of [ADR-0004](0004-single-box-vm-native-deployment.md)
  and [ADR-0049](0049-kubernetes-backend-split-control-plane.md), which until
  now rested on deployment shape alone (one systemd unit; Helm `replicas: 1`,
  `strategy: Recreate`).

## Context

`fleet serve`'s boot recovery treats whatever a previous process left
unfinished as crashed: every chat turn still `running` is projected into
history with a "server restarted mid-turn" marker and its
`history_committed_at` stamped (`store.RecoverStrandedTurns`), every executing
approval gets an outcome-unknown result, claimed inputs go back to the queue,
due approval resumes are dropped and show_ui countdowns paused. That is right
after a crash and wrong while the previous process still runs.

Nothing in the database said whether it did. A second `fleet serve` against
the same database — an operator running it by hand while the service runs, or
a cloned box pointed at the same `DATABASE_URL` — ran all of that before it
tried to bind its ports, then failed to bind and exited. The live server's
turns were already closed: when one finished, `CommitTurnHistory` met
`ErrTurnHistoryCommitted`, which it reads as "already written", and the real
answer was never stored. Its approvals in flight read outcome-unknown and its
claimed inputs ran again.

The sandbox layer already guarded against a sibling instance
(`internal/sandbox/prune.go` removes only containers whose labelled owner
process is dead). The server-presence lock of #2230
(`store.HoldServerPresence`) is held **shared**, so two servers hold it side
by side; it keeps CLI account deletion away from a running server and was
never meant to keep servers apart.

## Decision

**One `fleet serve` owns a database at a time, and it takes ownership before
it recovers or writes anything.**

- **The owner lock.** `store.ClaimServerOwnership` takes a session-level
  Postgres advisory lock (`serverOwnerLockKey`, "FLT_OWNR") **exclusive**, on
  a connection of its own, as soon as the configuration is read and the
  store opens: before the presence lock, the MCP broker, skills staging,
  boot recovery, the scheduler store and the listeners. Postgres releases it
  when that connection ends, so a crashed owner holds nothing.
- **Every lock connection is named and kept alive.** The owner connection
  carries `application_name` `fleet serve <instance> <host> pid <pid>`, the
  presence connection `fleet presence <instance> <host> pid <pid>`, the CLI
  account deletion's `fleet account deletion <instance> <host> pid <pid>`,
  where `<instance>` is the process's random 16-hex-digit id (the one its
  heartbeat row carries). The id comes first so Postgres' 63-byte limit
  never cuts it off: every container runs fleet as pid 1 and clones share
  host names, so host and pid alone do not tell two servers apart. Each sets
  `tcp_keepalives_idle=10`, `tcp_keepalives_interval=5`,
  `tcp_keepalives_count=3` and `tcp_user_timeout=25000` for its own session,
  so Postgres drops the session of a holder whose host vanished (a lost
  node) in about half a minute rather than after the operating system's
  defaults (keepalive after two hours, retransmissions for about fifteen
  minutes). That matters for all three: a lingering presence session would
  keep a replacement server out (below), and a lingering CLI deletion
  session would hold a starting server in its presence wait, which has no
  deadline. Postgres ignores these settings on a Unix-socket connection,
  where the peer is on the same host and its socket closes with it. The
  connections are discarded, never returned to the pool, when let go.
- **No other server without it.** Holding the lock, the starting server also
  refuses while (a) any other session holds the presence lock *shared* — a
  running server without the owner lock: an earlier release (so the first
  upgrade to this one is covered), or one whose owner connection dropped and
  is reconnecting — or (b) a heartbeat row in `core.server_presence` (any
  of the last hour, the age past which rows are deleted) is **still
  moving**: it looks again five seconds (`serverPresenceGrace`) later,
  holding the lock, and a row written meanwhile is a server reconnecting
  after a database outage, which writes its row on every try to take its
  lock back. The row's age says nothing — an outage longer than 45 seconds
  leaves a reconnecting server's row stale until its first try — so only
  movement counts. A row that does not move is a crashed server's and is
  passed over. A server that stopped cleanly removed its row, so a normal
  restart pays no delay; one within an hour of a crash pays the five
  seconds.
- **Wait, then refuse.** A starting server waits up to **60 seconds** for the
  owner to let go, logging at once and every 10 seconds whom it waits for
  (host, pid, address, connected since — read from `pg_locks` and
  `pg_stat_activity`), then exits non-zero with that holder named and nothing
  written; a stop signal while it waits exits cleanly. The wait covers a
  restart whose previous process is still letting go: a systemd restart, a
  Helm `Recreate` rollout, a node lost under the old pod (about half a
  minute, above). It is a fixed bound, not a setting: longer outages are the
  supervisor's job, and both supervisors retry anyway (systemd
  `Restart=always`, Kubernetes restarts the pod), so a knob would add nothing
  but another value to get wrong. 60 seconds stays well inside the unit's
  `TimeoutStartSec=120s`.
- **Losing it.** The owner checks its lock connection every second. One
  found dead is replaced and the lock taken again, every half second, writing
  the heartbeat row on every try. Each refused try it looks at who holds the
  lock: a holder it cannot see (the read failed, or the holder let go
  between the two reads) says nothing; a holder that is one of the sessions
  this process held the lock on — matched by backend pid **and** start time,
  never by name, which a clone can share — is its old session, discarded
  after a ping timed out in a network stall and not yet ended by Postgres,
  and is ended (`pg_terminate_backend`, which the same role may) before the
  next try. If
  any other session holds the lock through every try for 15 seconds (three
  graces — longer than a starting server holds it while it looks), another
  server owns the database: this one stops at once (as on a listener error)
  and exits non-zero. A server that is merely reconnecting is not stopped: a
  starting server sees its row move and lets go. The claim is checked again
  before boot recovery and after it, before the scheduler store opens. A
  heartbeat row written while taking the lock back is removed when the
  server stops, also when it stops before it held the presence lock.
- **Who takes it.** Only `fleet serve`. The CLIs that run alongside a server —
  `fleet migrate`, `fleet backup`, `fleet admin`, `fleet task run`, doctor,
  status — never take it, and still run while a server does. The CLI account
  deletion keeps to the presence lock (exclusive), a different key: a server
  takes the owner lock, then the presence lock; the CLI only the presence
  lock — so they never wait on each other in a cycle, a server starting
  during a CLI deletion holds the owner lock and waits for the deletion as
  before, and a CLI deletion is still refused while a server runs.
- **Before the lock.** The sandbox settings are resolved first (env and
  bundle only), so an invalid one fails without opening the database. In
  the database, only `store.Open`'s migrations run before the lock; every CLI's store open runs them too, behind the migration
  runner's own lock (ADR-0107). Outside it, see below.

## Consequences

- A second `fleet serve` on a database another one owns refuses with, for
  example, `take the database: another fleet server owns this database: fleet
  serve 3f9c0a1e5b7d2c48 box-a pid 4182 from 10.0.0.5, connected since
  2026-11-25T09:00:00Z (stop the other fleet serve first, or point this one at
  a database of its own)`, having changed nothing. Under systemd it is restarted and refuses
  again, harmlessly, until the other server stops.
- A restart within an hour of a crash waits five seconds before it goes
  on; a clean restart does not wait.
- The owner holds one more connection of its chat pool for its lifetime
  (the presence lock holds another).
- Both locks are per database, so the check depends on no unit name, port or
  host. A session pooler in transaction mode (PgBouncer) between fleet and
  Postgres breaks session advisory locks (and the keepalive settings would
  apply to the pooler's connection, not fleet's); the presence lock already
  required a direct or session-mode connection.

## What it does not do

- It does not keep a second process from opening the database: CLIs do that
  by design, and a `fleet serve` refused here has only run migrations.
- An owner cut off from the database while the database itself stays up
  for others (a network partition, so its row cannot move) may find, once
  it reconnects, that a new server took over and ran boot recovery
  meanwhile; it then stops within 15 seconds, but its turns were already
  recovered by the new owner. Fencing writes with a token on every
  statement would close that, at a cost not worth it for a single-box
  product.
- During a database restart, two refused clones retrying in turn can each
  hold the free lock for their five-second look often enough that the
  owner's tries keep failing for 15 seconds; it then stops (and its
  supervisor restarts it), although no other server took over.
- An earlier release started against a database a server of this release
  owns is not refused: it does not look for the owner lock, and runs its
  boot recovery. The first upgrade is covered the other way round only
  (a new server refuses while an earlier one runs).
- What runs before the database opens is not covered: reading the client
  bundle (which rebuilds the merged skills tree under `FLEET_DATA_DIR`),
  reading the configuration and opening (and possibly rotating)
  `FLEET_LOG_FILE`. Two boxes with the same `DATABASE_URL` have their own
  disks; on one box a refused second `fleet serve` has rewritten those
  files with what its own bundle says. The MCP broker, skills staging in a
  kubernetes workspace claim (ADR-0055), sandbox pruning and everything
  else come after the lock.

## Alternatives considered

- **A lease row with a heartbeat only.** A crashed holder blocks every start
  for the lease length, and a lease needs its own expiry logic; Postgres
  already ends a session lock with its connection. The heartbeat rows of
  #2230 are reused for the one case the lock cannot see (an owner between
  connections).
- **Taking the presence lock exclusive in `fleet serve`.** That key is shared
  on purpose; making it exclusive would change what the CLI deletion's
  refusal means and the reconnect logic it relies on.
- **Refusing at once with no wait.** A clean restart would not notice (the
  old process lets go as it exits), but a start racing a predecessor that is
  still letting go — a pod rescheduled after its node was lost, a process
  killed on a host whose network went first — would fail and lean on the
  supervisor's restart loop for something a short wait settles.
