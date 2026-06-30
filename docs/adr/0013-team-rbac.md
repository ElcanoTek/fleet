# ADR-0013: Team RBAC — roles + opt-in, team-scoped conversation reads

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

fleet is a small-team box (ADR-0004: single box, ~10–20 users). Until now every
account was functionally equal: any provisioned user could send messages and saw
only its own conversations, scoped at every read by `user_email`. Two operator
needs went unmet (issue #237):

1. **A read-only role.** Auditors, contractors, or stakeholders who should *see*
   activity but never *send* a message or mutate state. The only prior gate was
   the binary `ADMIN_EMAILS` env allowlist (`/admin/*` observability) — it has no
   middle tier between "full user" and "admin".
2. **A team supervisor view.** A manager wants to read the threads their team is
   working on without each report manually forwarding a public share link (#226)
   and without the manager gaining a blanket superuser read over the whole box.

The hard constraint is the package invariant in `internal/store/doc.go`: *every
read path is scoped by `user_email` — there is no superuser query*. A naive
"manager sees everyone on their team" JOIN would silently expose every private
conversation the moment two users share a `team_id`, retroactively breaking the
per-user privacy default. The #237 ticket text itself was internally
inconsistent on this point (a `team_id` JOIN in one place, a per-conversation
visibility flag in another); we resolved it toward the privacy-preserving model.

## Decision

We add a lightweight RBAC layer with **two** new `users` columns and **one**
`conversations` column (migration `024_rbac.sql`):

- `users.role` ∈ {`member` (default), `viewer`, `admin`}, with a `CHECK`
  constraint mirrored by `store.ValidRole`.
- `users.team_id` (nullable free-text label). A shared `team_id` is a *necessary
  but not sufficient* condition for cross-user reads.
- `conversations.team_visible` (default `FALSE`). The owner opts a single
  conversation in via `POST /conversations/{id}/share-with-team`.

**Roles.** `viewer` is enforced by `rejectViewerWrites`, a method-aware
middleware that 403s (`{"error":"read_only"}`) any state-changing method
(POST/PATCH/PUT/DELETE) on the data routes while leaving all reads (GET/HEAD)
intact. `admin` is granted by **either** the `ADMIN_EMAILS` env allowlist (the
out-of-band bootstrap gate, unchanged) **OR** `users.role = 'admin'` — an OR,
never a downgrade of the env path, so the first operator can always mint more
admins. The role is read from the request context, where `membershipMiddleware`
enriches it from a single `GetUser` lookup that already had to run to admit the
request.

**Team-scoped reads.** `Store.ListTeamConversations(callerEmail)` is the **only**
cross-user conversation read path. It is doubly gated:

```
WHERE team_visible = TRUE
  AND user_email IN (SELECT email FROM users WHERE team_id = <caller's team>)
```

so a conversation is exposed to a teammate only when (a) the reader shares the
owner's non-empty `team_id` AND (b) the owner explicitly flipped
`team_visible`. Team membership alone never auto-exposes anything; a caller with
no team gets `ErrNoTeam` (→ HTTP 400). The flag is owner-only to set
(`SetConversationTeamVisible` gates on `user_email`), so one teammate can never
expose another's thread. The result is exposed read-only at
`GET /conversations?scope=team` and is deliberately a *separate* list endpoint
that never mixes the caller's private conversations in.

This **relaxes** the absolute "no superuser query" wording of
`internal/store/doc.go` to "no *unconditional* superuser query": the one
cross-user query that now exists is bounded by a shared team AND a per-row owner
opt-in. We update that package doc in the same change rather than leave it
claiming a guarantee the code no longer makes (honesty-in-docs invariant).

## Enforcement

- `internal/store/rbac_test.go` proves the gates against real Postgres: default
  role is `member`; `SetUserRoleTeam` partial-PATCH semantics (nil = untouched,
  `""` = clear team); a teammate sees only the shared conversation and never the
  private one; a different team sees nothing; a no-team caller gets `ErrNoTeam`;
  a non-owner cannot flip `team_visible`.
- `internal/httpapi/rbac_http_test.go` proves the HTTP surface: a `viewer` is
  403'd on write but 200 on read; a DB-role `admin` reaches `/admin/users` with
  an empty `ADMIN_EMAILS`; a member does not; `PATCH /admin/users/{email}`
  assigns role+team and rejects an invalid role (400) / unknown user (404);
  `?scope=team` returns only shared threads and 400s a teamless caller.
- `internal/store/doc.go` carries the updated invariant text so the relaxation
  stays visible at the package boundary, not buried in a method.

## Consequences

- A new, bounded cross-user read path exists. It is the single place a future
  reviewer must scrutinize for the privacy property; concentrating it in one
  doubly-gated query (rather than scattering team logic across handlers) is the
  point.
- `viewer` is enforced at the HTTP middleware boundary, not in the store. A new
  *write* endpoint that forgets to sit behind `rejectViewerWrites` would not be
  viewer-gated. The mitigation is that `mutate` wraps the data-route fan-out in
  one place in `Routes()`; new mutating routes join that list.
- The columns are additive with safe defaults (`member`, `team_id NULL`,
  `team_visible FALSE`), so existing rows and the byte-for-byte default behavior
  are unchanged until an admin assigns a role/team.

## Alternatives considered

- **`team_id` JOIN with no per-conversation opt-in.** Rejected: adding a
  `team_id` would retroactively expose every existing private conversation to
  the whole team — a silent privacy regression and a direct violation of the
  per-user scoping default.
- **A separate `teams` table + membership rows.** Rejected as premature for a
  ~10–20 user box: a free-text `team_id` label is enough to express the trust
  group, mirrors the existing scoped-API-key glob convention, and avoids a join
  table with its own lifecycle. It can be promoted later without changing the
  read-gate shape.
- **Reuse public share tokens (#226) for the manager view.** Rejected: share
  tokens mint an unauthenticated public URL (entropy is the only guard). A
  supervisor view must stay identity-gated and must not create a link that
  works outside the box.
