# ADR-0087: An interactive MCP scope carries the signed-in user's email to its bundle servers

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** fleet maintainers (owner-approved 2026-10-20; scope cut by the owner 2026-10-21 to the email only — no name token)
- **Relates to:** [ADR-0042](0042-child-side-mcp-scope-authorization.md)
  (child-side MCP scope authorization),
  [ADR-0040](0040-child-owned-remote-mcp-runtime.md) (the remote
  overlay, which already sends the user's email to the child), the
  "Identity/credentials are harness-side" coupling rule in `AGENTS.md`.
- **Design note:** [`MCP-BUNDLE-ENV.md`](../MCP-BUNDLE-ENV.md) §1b

## Context

A bundle connector that writes something addressed to a person needs to know
who that person is. The concrete case: the deal-sheet email a bundle's
`finish_plan_booking` sends at the end of a chat booking should default its
recipient to the trader who asked for it. Fleet gave bundle tools no user
identity at all, so the model had to be told the recipient.

There were three ways to get the identity to the server:

1. **Let the model pass it.** The model can be told the user's email and put
   it in an argument. Rejected: the value would be model-authored, so a
   prompt-injected document could change who receives the email, and the
   connector could not tell an honest argument from a forged one.
2. **Read it from the operator's env.** A deployment-wide `${VAR}` is one
   value for every user. Rejected: it is wrong for everyone but one person.
3. **Substitute it into the server's env when the turn's scope is opened.**
   Production `fleet serve` already spawns fresh MCP subprocesses per
   interactive turn, inside the credential-owning `fleet mcp-broker` child
   (ADR-0042), and already substitutes per-scope values there
   (`${FLEET_WORKSPACE}`, `${FLEET_TASK_ID}`). The signed-in user is one more
   per-scope value.

Option 3 means a person's identity crosses the broker boundary for bundle
scopes — today only the remote overlay sends one (its `userEmail`, ADR-0040) —
and reaches a host-side MCP subprocess. That is a new kind of data on a path
ADR-0042 governs, so it is recorded here.

## Decision

1. **One reserved token.** `${FLEET_USER_EMAIL}` (the normalized, lowercased
   email) may appear in an `mcp_servers[].env` value of a stdio server, like
   the workspace tokens: exact bare spelling only, never read from the process
   env (an operator export would give every user one person's identity),
   refused anywhere else in the manifest or an Agent Plugin's `mcp.json`.
   It is honoured only where the bundle writes it: a process value reached
   through an ordinary `${VAR}` that names the token counts as unset, and a
   `<VAR>_<ACCOUNT>` overlay that names it refuses the seat, so an exported
   value can never smuggle the user into a key the load checks did not see
   (an `identity_env` key above all).
   `fleet validate-config` and bundle preflight therefore accept it where it
   works and fail it where it cannot.
2. **No name token; `FLEET_USER_NAME` is reserved and refused.** No sign-in
   path carries a name. Fleet signs people in three ways — its own password
   form, OIDC against the Elcano Auth service, and the Auth service's
   `elcano_auth` magic-link cookie — and all three hand fleet an email and
   nothing else (the Auth service holds no name, advertises only the `openid`
   and `email` scopes, and issues no `name` or `given_name` claim). A name
   guessed from the email was rejected by the owner: `jsmith@…` is not
   "Jsmith", and a greeting that gets someone's name wrong is worse than
   "Hi team,". A user-saved name (a Settings field and a `users` column) was
   built and then cut by the owner as not worth the surface. So any spelling
   of `${FLEET_USER_NAME}` fails the load with an error saying no name token
   exists, and the spawn-time binder drops any key that still carries it. It
   stays reserved so an operator-exported `FLEET_USER_NAME` can never be read
   into every user's turn, and so a name token can be added later, on a real
   name source, without changing this contract.
3. **One source: the authenticated sender of an interactive turn.** The HTTP
   layer fills `TurnInput.MCPUser` from the request's authenticated identity
   (`X-User-Email` over the shared-secret channel) for a turn a person sent —
   directly or from their own input queue — and for the turn fleet resumes in
   their conversation after one of their approval cards settles (ADR-0083),
   which continues that person's own turn (a card that settles by timing out
   included). Conversations have exactly one owner and every turn path
   loads the conversation owner-scoped, so the sender is also the owner; a
   teammate reading a team-shared chat cannot send a turn in it (they branch).
   An approved card's call carries the approving user, who is the only user
   who can claim that card. Nothing reads the identity from the conversation,
   the transcript, the model, or the bundle.
4. **Everything else gets no user, and the keys are dropped.** A
   webhook-started turn (it runs as the trigger owner, but nobody signed in
   sent it), evals, dataset runs, every scheduled run, the boot catalog, hot
   reload, load-on-demand onto a shared client, and `fleet mcp test` drop each
   token-bearing key, so the server sees the variable as **unset** — never a
   literal token, an empty string, or another user's identity. A
   process-lifetime subprocess serves every user, so it can never carry one.
5. **The identity crosses as data, and only on bundle interactive scopes.**
   `ScopeSpec.User` carries `{email}`. The child refuses a spec that pairs it
   with a `TaskID` (a scheduled scope) or a remote overlay before the backend
   sees it, and the parent never sends one on a task scope. The child
   re-validates the email against a conservative character set
   (`agentcore.MCPUserIdentity.Sanitized`) and treats a value that fails as
   absent. The set admits no `$`, `{` or `}`, so a substituted value can never
   spell another reserved token for a later pass to expand. The child writes
   the user into each server's env last, at spawn, after the
   `<VAR>_<ACCOUNT>` overlay and its inherited-identity guard, so a named seat
   gets the user exactly like the default seat.
6. **ADR-0042 is unchanged: the user authorizes nothing.** The value selects
   no server, seat or tool and is not part of `ScopePolicy`. The child's
   Gate-2 floor, enabled-server set and Gate-3 relay apply exactly as before,
   and a scope with a user reaches precisely what it would have reached
   without one. A bundle server must not treat the variable as a credential
   or a seat selector; the docs say so, and the load refuses a key listed in
   `identity_env` (seat/money routing) whose value references the token.
7. **PII handling follows existing norms.** Fleet adds no log line, metric
   label or audit field that prints the value, and the env map is not
   model-visible and never enters the sandbox. Operational logs that already
   name the authenticated user (turn errors, audit rows) are unchanged. What a
   bundle server does with its own env is the bundle's responsibility.
8. **One spelling matcher for both sides.** What "names a user token" means —
   the exact spelling, an escape, padding of any kind (Unicode whitespace and
   invisible characters included), a colon form, or `FLEET_USER_NAME` — lives
   in `internal/usertoken`, used by the loader to refuse and by the binder to
   drop, so the two can never disagree.

## Consequences

- A bundle can address the person at the keyboard without the model being in
  the loop, and a scheduled run of the same connector degrades to the
  connector's own "no user" behaviour instead of borrowing anyone's identity.
- A connector that greets someone has no name to use and keeps its own
  no-name wording ("Hi team,").
- The broker protocol gains an additive field. The parent and the child are
  the same binary, so there is no skew between them. A bundle must still
  adopt the token only after the fleet release carrying this ADR (the
  additive-first rule): an older fleet treats `${FLEET_USER_EMAIL}` as an
  ordinary unset variable and hands the server an **empty** value, which a
  connector must already tolerate as "no user".
- The transitional local-client mode (no broker) has no per-turn spawn, so it
  drops the keys like every shared spawn. Production `fleet serve` always runs
  the broker.
- A person's email now reaches host-side bundle subprocesses for the duration
  of their own turn. The subprocess already handles that user's data (the
  turn's arguments and results); its env is not model-visible and never enters
  the sandbox.

## Alternatives considered

- **Model-supplied recipient.** See Context; injectable and unverifiable.
- **A per-request header to the MCP server.** stdio servers have no request
  headers, and the bundle's servers are stdio.
- **Put the user in `ScopePolicy`.** Rejected: the policy is authorization
  input that can only narrow, and identity is not authorization. Keeping it a
  separate field keeps ADR-0042's reasoning intact.
- **Derive a `${FLEET_USER_NAME}` from the email's local part**
  (`elyse.messer@…` → `Elyse`). Rejected by the owner: it greets `jsmith@…` as
  "Jsmith".
- **A first name each user saves in Settings** (`users.given_name`, a
  `PUT /me/name` endpoint and a Settings row). Built, then cut by the owner
  (2026-10-21): the greeting is not worth a new account field, endpoint and
  migration. The reservation leaves room to add it back.
- **Read a name claim from the identity provider.** None exists on any
  sign-in path today, and an Auth-side name would cover OIDC sessions only.
