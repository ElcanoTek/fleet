# ADR-0047: Self-serve team membership — create/leave is yours, joining is granted

- **Status:** Accepted
- **Date:** 2026-08-18
- **Deciders:** fleet maintainers

## Context

ADR-0013 introduced `users.team_id` as the trust-group label behind two shared
reads: `?scope=team` conversations (owner opt-in per conversation) and, from
ADR-0021, team-shared **projects** (visible and usable by every user whose
`team_id` matches). It deliberately left the *assignment* of `team_id` to admins:
the only write path was `PATCH /admin/users/{email}` (Settings → Admin → Users)
and the operator CLI.

On a real box that turned out to be a dead end (#1157):

1. **The admin path was itself broken for the person most likely to use it.**
   The Users tab PATCHes `role` and `team_id` together, and the handler refused
   any self-PATCH whose `role` was not `admin` as a lockout guard. An
   `ADMIN_EMAILS` bootstrap admin has the column-default `role = 'member'`, so
   *every* attempt that admin made to set their own team was rejected with
   "refusing to demote your own account". The first operator of a fresh box could
   not put themselves in a team at all.
2. **With no team, projects were unusable as advertised.** "Share with my team"
   400s for a teamless caller, and the message said "ask an admin to set one" —
   advice that pointed back at the broken path, and at a person who may not
   exist on a single-operator box.

So the shipped feature set (#509 projects, #237 team reads) was reachable only
through the CLI, and the UI told users to ask someone else. The question this ADR
settles is not *whether* users can set a team from the UI, but **which half of
team membership is self-serve** — because `team_id` is an authorization input,
not a display preference.

## Decision

Split team membership by what the write grants:

- **Creating a team and leaving one are self-serve.** `PUT /me/team`
  (`{"team_id": "platform"}`, `""` to leave), behind auth + membership +
  `rejectViewerWrites`. `GET /me` and `GET /me/team` report the caller's own
  `{email, role, team_id, admin}`.
- **Joining an existing team is granted, not claimed.** `store.SetOwnTeam`
  refuses a name that any *other* user — or any team-shared *project* — already
  carries, case-insensitively, with `ErrTeamExists` → HTTP 409. Admins bypass the
  gate (`allowExisting`), which is the same authority they already hold through
  the Users tab.

The gate lives in the store, in one function, alongside the write it guards. Two
details it must get right:

- **A `pg_advisory_xact_lock(hashtext(lower(team)))` around the check.** A plain
  `SELECT` cannot lock rows that do not exist yet, so two concurrent creates of
  the same name would each see an empty team and the second would land silently
  inside the first one's trust group.
- **Projects are checked alongside users.** A team-shared project outlives its
  team's last member (they left, or the account was deleted). Without the project
  check, its shared memory would become claimable by typing its team name.

Re-stating the team you are already in is an idempotent no-op (not a "join"), and
names are trimmed and capped at 64 characters — this is the one place a
non-admin writes that column.

The self-lockout guard on `PATCH /admin/users/{email}` is narrowed to fire on an
**actual** demotion: self, new role ≠ admin, current `users.role` = admin, and
the caller not in `ADMIN_EMAILS` (the env grant survives any column write). The
Users tab additionally now sends only the fields the admin actually changed, so
an unchanged role never reads as a demotion in the first place.

## Enforcement

- `internal/store/rbac_test.go:TestSetOwnTeam` — create, idempotent re-state,
  case-insensitive join refusal, admin bypass, the project-name reservation,
  leave, over-long name, unknown user.
- `internal/httpapi/teams_selfserve_test.go` — the #1157 regression (an env-admin
  PATCHing their own team succeeds), the narrowed guard still refusing a real
  self-demotion, the /me + /me/team lifecycle including the 409, viewers 403 on
  write but 200 on read, and the payoff: the same `team_shared` project write
  that 400s teamless succeeds after a self-serve create.
- `web/src/app/settings/team/team.test.tsx`,
  `web/src/app/chat/ui/ProjectsModal.test.tsx`, and the added
  `users.test.tsx` case cover the three UI surfaces.

## Consequences

- **A new self-serve write to an authorization column exists.** It is bounded to
  names nobody else holds, so it can only ever create a trust group of one that
  others must be *added* to. The exposure it can produce is a team the writer
  already controls.
- **Team names are effectively first-come.** A member can occupy a name an admin
  intended for a different group; the fix is the existing
  `POST /admin/teams/rename`, which relabels users and projects in one
  transaction. We accept this on a ~10–20 user box rather than add a teams table
  (ADR-0013's alternative, still deferred).
- **`ErrTeamExists` is an information disclosure of one bit:** that a team name
  is in use. That is inherent to any create-if-free rule, and the response never
  names who holds it.
- Nothing about the *read* gates changes: a shared `team_id` still exposes
  nothing on its own — a conversation needs its owner's `team_visible` opt-in,
  and a project needs its owner to mark it team-shared.

## Alternatives considered

- **Unrestricted self-assign (`PATCH /me {team_id}` with no check).** Rejected:
  anyone could type `leadership` and read every conversation that team has
  shared plus every project shared with it. That is exactly the silent privacy
  regression ADR-0013 rejected the naive `team_id` JOIN over.
- **A `FLEET_DEFAULT_TEAM` env var putting every new account in one team.**
  Rejected as the primary fix: it makes the whole box one trust group by
  configuration, which is a policy decision, not a bug fix — and it would not
  have helped the reporter, whose account already existed. The self-serve create
  gives a single-operator box a working team in one action without changing what
  a team means.
- **Leave the write admin-only and just fix the guard.** Rejected as
  insufficient: it fixes the reported 400 but keeps "ask an admin" as the only
  path for members, and on a single-admin box every team change stays a support
  request. Fixing the guard is necessary (we did it) but not the whole bug.
- **A teams table with invitations.** The right shape if teams grow roles or
  membership history; premature here, and the read-gate shape does not change if
  we promote the label later.
