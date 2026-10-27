# ADR-0093: A team share names its audience by team ID

- **Status:** Accepted
- **Date:** 2026-10-27
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0057](0057-team-shared-chats-live-in-team-shared-projects.md) (the
  stamped audience), [ADR-0013](0013-team-rbac.md)
- **Design note:** [`TEAM-SHARING.md`](../TEAM-SHARING.md), "Audience identity"

## Context

ADR-0057 stopped inferring a share's audience from the owner's current team and
stamped it on the row instead (`conversations.team_shared_with`). The stamp was
the team's NAME, and a team's name is not its identity: `RenameTeam` relabelled
`users.team_id` and `projects.team_id` but not the stamp. After a rename the
renamed team no longer matched its own shares, and once the old name was free,
anyone could form a new team under it (self-serve team creation only refuses a
name some user or project still holds) and match every chat — and its shared
outputs — the original team had been given.

## Decision

1. Every team that has been shared with gets a row in `teams` (surrogate `id`,
   unique exact `name`). Sharing stamps the id
   (`conversations.team_shared_with_id`) alongside the label, in one
   statement that creates the row on first share.
2. Every read gate (team view, team listings, the team-read check behind file
   downloads and branch, the project Team section, team links, the Sources
   pruning) requires the stamped id to equal the id registered under the
   caller's team name, in addition to the label match.
3. `RenameTeam` relabels the team's row in the same transaction, so the id —
   and every share — stays with the team; renaming onto an existing team (a
   documented merge) re-points the shares to the surviving team's id.
4. Before anyone joins a team name that no user and no project holds (a
   self-serve create, an admin move, an identity-provider sync, a new
   centrally provisioned account, or a rename onto that name), any `teams` row
   still registered under it belongs to a team that no longer exists: its
   remaining shares are unshared and the row is deleted, so the new team gets
   a new id and inherits nothing.
5. Writes that create, relabel or retire a `teams` row take a transaction
   advisory lock per team name (sorted, so overlapping sets cannot
   deadlock): the share stamp for its team, `RenameTeam` for both names, the
   retire path for the joined name, and every team change of a user for the
   user's current and new team — always BEFORE any users row is locked
   ("team names, then user rows"), the order `RenameTeam` uses. A row lock
   cannot order a first share against a rename, because the first share's
   `teams` row is an uncommitted insert nobody else can see.
6. Migration 084 backfills ids for every share whose label is still its
   owner's current team, and unshares the rest: those are exactly the stamps a
   rename left behind, and which team they meant cannot be told from the data.

## Consequences

- A rename can no longer orphan or expose a share. Shares orphaned by renames
  made before this change are unshared by the migration; owners share them
  again in one click.
- Every place that clears `team_shared_with` also clears the id; the owner-side
  bookkeeping that matches by label (unshare on leave, the leave-impact count)
  keeps working because the label always follows the id.
- `users.team_id` and `projects.team_id` remain names. Making membership itself
  id-based is a larger change this ADR does not make; the share is the record
  that outlives membership changes, so it is where identity was needed.
