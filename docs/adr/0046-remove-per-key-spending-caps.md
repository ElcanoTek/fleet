# ADR-0046: Remove per-API-key spending caps; rolling budgets are the one spend gate

- **Status:** Accepted
- **Date:** 2026-08-15
- **Deciders:** fleet maintainers
- **Related:** [ADR-0045](0045-remove-node-name-scopes.md) (the same "an
  authority that enforces nothing is worse than no authority" argument, applied
  to node-name scopes)

## Context

A scoped API key carried its own spend gate, inherited from moc: four fields on
the key's JSON row (`max_cost_per_day_usd`, `max_cost_per_month_usd`,
`cost_today_usd`, `cost_this_month_usd`, plus two reset watermarks), a
`CheckBudget` pre-flight on `POST /tasks` and `POST /tasks/batch`, an
`AccumulateCost` mutator, lazy UTC day/month rollover, and two endpoints —
`GET /keys/{id}/spending` and `POST /keys/{id}/reset-spending`.

Every part of it worked except the one that mattered: **nothing ever called
`AccumulateCost`.** The counter it incremented had no producer anywhere in the
unified runtime. In moc, task cost reached it through the HTTP
task-completion callback; folding gig's remote lease/report protocol into the
in-process `internal/runner` (status and logs became direct storage writes, no
HTTP hop) dropped that call site, and nothing replaced it.

The observable consequences:

- An operator who set `max_cost_per_day_usd: 50` on a key got a `200 OK`, saw
  the cap echoed back on the key, and was **not** capped. `CheckBudget`
  compared a live cap against a counter frozen at `0.0`, so it returned nil
  forever.
- `GET /keys/{id}/spending` reported `$0.00` spent for every key, for all time.
- `POST /keys/{id}/reset-spending` zeroed numbers that were already zero.
- The package's own tests passed, because they called `AccumulateCost`
  themselves. Unit coverage proved the machinery; nothing proved it was wired.

This is the failure mode the repo's honesty invariant exists to prevent, in its
most expensive form: a **security/cost control that reports success and does
not hold**. An operator is worse off than with no cap at all, because they
stopped watching.

The capability itself is not in question — it already shipped, correctly, as
part of #601 part 2. `internal/sched/budget` enforces per-principal rolling
budgets with `scope: key`, keyed by `tasks.created_by_key_id`, at the same
task-create gate. Its founding constraint was explicitly *"no second accounting
path"*: a budget never accumulates on its own row, it recomputes the window's
spend from the metering the governed core already persists (`task_iterations` ⋈
`tasks`, plus chat `turn_metrics`). The per-key caps were exactly the second
accounting path #601 refused to build — and the reason they drifted into
silence unnoticed is that they had no shared producer to keep them honest.

## Decision

**Delete the per-key spending caps. `POST /admin/budgets` with `scope: key` is
the one per-key spend gate.** Concretely:

1. The six spend fields leave `apikeys.APIKey`, and `SetBudgets`,
   `AccumulateCost`, `CheckBudget`, `ResetSpending`, `SpendingSnapshot`, and
   `maybeResetSpendingLocked` are removed with them. `models.APIKeySpending`
   and the four spend fields on `APIKeyResponse` are removed.
2. `GET /keys/{key_id}/spending` and `POST /keys/{key_id}/reset-spending` are
   unregistered (404) and removed from `docs/openapi.yaml`.
3. The `CheckBudget` pre-flights in `CreateTask` and `CreateTaskBatch` are
   removed. `budgetCapError` — already running immediately after them on both
   paths — is the sole spend gate, which restores the "one shared helper, so
   the create paths cannot drift" discipline both gates were meant to follow.
4. `POST /keys` **rejects** `max_cost_per_day_usd` / `max_cost_per_month_usd`
   with `400` naming the replacement, before minting the key. The fields stay
   on `APIKeyCreate` for exactly that purpose. Silently ignoring them would
   reproduce the original bug — an operator believing a key is capped when it
   is not — which is the one outcome this ADR exists to end.
5. `tasks.created_by_key_id` stays. It is the `key` bucket of the usage read
   model (and therefore the principal a `scope=key` budget is evaluated
   against) and the ownership check behind the transcript gate; only the stale
   comments that justified it by the removed caps change.

Old key files load unchanged — `encoding/json` ignores the retired fields, and
the next save drops them. No migration runs.

**No invariant is weakened.** A spend gate that never fired is removed; the
gate that does fire is untouched, and it is strictly the stronger of the two:
real metering instead of a dead counter, day/**week**/month windows instead of
day/month, token bounds as well as dollars, one soft alert per window
crossing, and chat turns counted alongside tasks.

## Consequences

- A deployment that set per-key caps loses a setting that was never enforcing
  anything, and now gets a `400` telling it where to put the cap instead. This
  is **breaking at the API surface** and is called out as such in the
  CHANGELOG; it is not a behavior regression, because there was no behavior.
- Any client polling `GET /keys/{id}/spending` gets a 404. The replacement read
  is `GET /admin/usage?group_by=key` (real spend) or `GET /admin/budgets` (live
  spend vs. bound).
- [ADR-0045](0045-remove-node-name-scopes.md) lists "budgets" among the
  typed-key gates that "do enforce something real". For per-key budgets that
  was not accurate; this ADR corrects it. The rate limit, trigger slugs, and
  the priority ceiling — the other gates it names — are unaffected and do
  enforce.

## Alternatives rejected

- **Finish it: call `AccumulateCost` from the runner's terminal path.** This
  builds the second accounting path #601 deliberately refused. It would drift
  from the usage read model (different rounding, different retry/dead-letter
  handling, nothing at all for chat turns), and a deployment could then be told
  two different numbers for the same key. Two spend gates that disagree is a
  worse outcome than the one that already works.
- **Keep the fields, drop only the enforcement.** A cap that is stored,
  displayed, and documented but explicitly not enforced is the dishonest
  surface with extra steps.
- **Accept the field on `POST /keys` and ignore it.** Ignoring a spend cap
  silently is precisely how this went unnoticed for as long as it did.
