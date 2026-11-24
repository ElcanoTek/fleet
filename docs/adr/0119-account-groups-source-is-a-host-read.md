# ADR-0119: The account-groups source is a bundle-declared read fleet runs with no turn

- **Status:** Accepted
- **Date:** 2026-11-23
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0082](0082-approval-card-describers-run-a-declared-read-at-staging.md)
  (approval-card describers run a declared read at staging),
  [ADR-0042](0042-child-side-mcp-scope-authorization.md) (child-side MCP scope
  authorization), the "Governance is one core" and "Client content is
  external" invariants in `AGENTS.md`.
- **Design note:** [`ACCOUNT-GROUPS.md`](../ACCOUNT-GROUPS.md)

## Context

A bundle can list account groups in its manifest: named sets of connector
accounts the chat Tools picker offers as chips. Some deployments keep that
list in another system (one group per client, added and changed by the people
who onboard clients), and a bundle release per client is the wrong cadence.
The only code that can read that system is the bundle's own MCP server, with
credentials fleet's main process does not hold.

So fleet has to call a bundle tool to draw a picker. That call has no model
behind it, no user message, no approval and no chat turn at all. ADR-0082
allowed one host-side call per staged approval, inside a turn's scope. This
one is further from the loop: it is made when a person opens the picker.

## Decision

A bundle may name one tool as the source of account groups
(`account_groups.source: {server, tool}`). fleet calls it under these
constraints:

1. **Only a declared read.** The tool must be listed in
   `agent_policy.parallel_safe_tools` under its registered name and must not
   be critical. `fleet validate-config` reports a source that is not, boot
   logs it, and the check runs again before every call under the installed
   policy (`agentcore.HostReadRefusal`). fleet never calls a critical tool
   outside the approval path.
2. **No hook is bypassed.** A `pre_tool_use` hook that matches the tool
   cannot run outside the loop, so the call is skipped rather than made
   without it.
3. **The credential owner's own gate.** The call opens a scope holding only
   the source server, on its default account, through the same scope opener
   approvals use. The credential-owning child authorizes it against its own
   bundle (ADR-0042): a tool missing from the server's allowlist is refused
   there. No new broker path exists.
4. **No arguments, no identity.** The tool is called with an empty argument
   object and no user identity (`${FLEET_USER_EMAIL}` keys are dropped). The
   answer is the deployment's list, the same for every user. What differs per
   user is computed by fleet afterwards, from that user's own picker rows.
5. **Bounded, cached, never in the request's way.** Fifteen seconds per call,
   as a deadline and as the child's per-call budget; no retries inside a
   call. An answer is served for `refresh_seconds` (default 300, at least
   60). Only the first read after boot waits, for at most two seconds. Every
   other read is served the last answer while one refresh runs behind it. A
   failed refresh keeps the last good answer and is tried again after a
   minute.
6. **Display data only.** The answer never reaches a model, a transcript or a
   tool result. It is parsed into the fixed group schema the manifest's own
   groups use (`clientconfig.NormalizeAccountGroups`), a group that breaks a
   rule is left out, an answer the secret redaction would alter is refused
   whole, and what is kept is sent to the picker as names. It decides nothing
   on its own: a group only pre-fills connector switches and account choices
   the person could set by hand, each still checked by
   `cleanMCPAccounts` against the live catalog when it is saved.

## Consequences

- A deployment's groups can follow another system with no bundle release and
  no fleet code that knows the system.
- fleet now makes a tool call nobody asked for in that moment. It is limited
  to a tool the bundle itself declared read-only, at most once per refresh
  interval per process, and it can be turned off by removing the manifest
  key.
- A group names accounts, and a wrong answer could offer a chip that sets a
  connector to an account the person did not mean. The chip shows which
  connectors it sets, the account menus show the result, every turn's prompt
  names the accounts that are on, and every write is approved per account.
  The source cannot make an account usable that the box does not have: a
  member that is not provisioned is reported on the chip and never replaced.
- The answer is trusted as much as the bundle is. A bundle that can name a
  source can already list the same groups in its manifest.
- `HostReadRefusal` / `HostReadProblems` are the one statement of the
  host-read rule for future callers. A new host read still needs its own ADR
  for what it does with the answer.

## Alternatives considered

- **Fetch over HTTP from fleet's main process.** It would need the other
  system's credential in the process that deliberately holds no connector
  credentials after boot.
- **A file the bundle refreshes.** A scheduled task writing a file fleet
  reads adds a writer, a schedule and a staleness window nobody watches, to
  avoid one read.
- **Ask the model to set the accounts.** A model choosing which client's
  account a chat runs as is the failure the picker exists to prevent.
