# Several accounts of one connector in one chat

A conversation can run one bundled connector as several credential accounts at
once: Index Exchange as the house account and as two client accounts, say.
Before this, a conversation had exactly one account per connector, so working
across accounts meant one chat per account.

It builds on [CONNECTOR-PREFS.md](CONNECTOR-PREFS.md) (availability, selection,
binding) and the per-conversation account pick (#988).

## What was already there

- **Accounts are separate processes.** `agentcore.MCPSelection` is a list of
  `{server, account}`. Each entry is its own credentialed subprocess, registered
  as `server` (default account) or `server_account`, so its tools are
  `mcp_<server>_<tool>` or `mcp_<server>_<account>_<tool>`. The binder, the
  broker's scope authorization and the credential allowlist are all keyed by
  that registered name. Scheduled tasks could already pin several accounts of
  one server.
- **Approvals record their account.** A staged card stores the `{server,
  account}` it was staged under and execution reopens exactly that
  (`approvals.mcp_server`, `mcp_account`). One call, one account.
- **Only chat was limited.** The Tools picker and `conversations.mcp_accounts`
  held one account per connector.

So this change adds no new execution path. It lets a conversation put more than
one entry for a server into the selection it already builds.

## What shipped

### Storage: the same column, one value per connector

`conversations.mcp_accounts` keeps its shape (`{server: string}`). A connector
on several accounts stores them joined by a comma, the empty element standing
for the default account (`internal/store/mcp_account_list.go`):

| Value | Meaning |
| --- | --- |
| absent / `""` | the default account |
| `"twc"` | one named account (unchanged) |
| `",twc"` | the default account and `twc` |
| `"twc,arena"` | `twc` and `arena` |

No migration. A conversation on one account per connector reads and writes
byte-identical rows, and the paths that only move the map (multiplayer
participant keys, transfer, a user rename) are untouched. An account name
cannot contain a comma: bundled accounts are env-var suffixes folded to
`[a-z0-9_]`.

A binary from before this change reads a list as one unknown account name,
which it already treats as stale: the turn runs on the default account. It
never runs on a different client's account.

### API

- `POST /conversations/{id}/mcp-servers`, and `mcp_accounts` on `POST /chat`
  and `POST /conversations`: an account value may be a string (as before) or a
  list of strings, `""` for the default account. `cleanMCPAccounts` refuses an
  account the connector does not have and more than
  `store.MaxMCPAccountsPerConnector` (6) accounts, with a 400. It stores the
  list in one order (default first, then the catalog's), whatever order it was
  sent in.
- The POST answers `accounts` in the same two shapes: a string for one account,
  a list for several.
- `GET /conversations/{id}/mcp-servers`: a row's `account` is the first
  account, as before, and a row on several accounts also carries
  `selected_accounts`. A row on one account has no such key. A stored list is
  read against the live catalog the way the turn reads it
  (`bundledChosenAccounts`): an account since removed from the box is not
  listed, so the picker shows what the next turn will run as.
- `""` beside the account that is the user's own Settings default names one
  account twice. It is stored, answered and shown as that one account, by
  name.

### The turn

`applyConnectorPrefs` resolves each bundled connector's accounts
(`bundledTurnAccounts`, the picker's `bundledChosenAccounts` with `""` filled
in): `""` in the choice is the user's Settings → Connections default, a named
account that is no longer provisioned is left out, and a choice with nothing
left falls back to the default. The first
account goes where a single account always went (`TurnInput.MCPAccountDefaults`)
and the rest in `TurnInput.MCPExtraAccounts`. The scope selection and the run
selection add one `{server, account}` entry per extra account. Everything after
that (binding, identity guards, the credential allowlist, process isolation,
the approval seat) is the existing per-entry path.

### What the assistant is told

When a chat turn's selection names a connector under more than one account,
the system prompt gains a section, `## Connectors on for several accounts in
this chat`, listing each such connector, its accounts and their tool-name
prefixes, with four rules: a call acts on the one account in its tool name and
ids do not cross accounts; name the account in every answer and before every
action; ask when a request does not say which account; a change for several
accounts is one call and one approval per account.

`agentcore.Run` writes it, from the roster it actually registered
(`internal/agentcore/multi_account.go`), just before the live registry and for
the same reason: only the roster knows what survived. An account is a
best-effort server and can fail to connect while the connector's other
accounts come up, and a connected one can lose every tool to the allowlist,
the persona policy, the schema check or `interactive_hidden_tools`. An account
with no tool in the roster is listed as "NOT available this turn" with no tool
prefix, and a fifth rule tells the assistant to say so and never do its work on
another account. The section is rebuilt with the registry when the roster
changes mid-run.

It is a pure function of the selection and the roster (connectors sorted, no
counters), so it is byte-stable for the same choice and the same tools
([PROMPT-CACHE-CONTRACT.md](PROMPT-CACHE-CONTRACT.md)). The interactive driver
asks for it (`RunConfig.MultiAccountNotice`); a scheduled run does not, because
"ask the user which account" has no one to ask, so a task that pins several
accounts reads the prompt it always did. A turn with one account per connector
gets no section and an unchanged prompt.

### The picker

The account pill on a popover row and the account field in the library are the
same `SeatMenu`. For a bundled connector with named accounts it is now a
checklist (`aria-multiselectable`): each row ticks or unticks one account and
the menu stays open. The last ticked account cannot be unticked (the
connector's switch turns it off), and at the cap the unticked rows are
disabled with "Up to 6 accounts at once." The pill reads the first account and
how many more: `Elcano +2`.

### Approval cards

No card writes to two accounts: each staged call is one tool of one account,
and already showed "Runs as `<account>` on `<connector>`" for a named account.
A card on the default account showed nothing. In a chat that runs the
connector as several accounts, that card now reads "Runs as Elcano (default
account) on Index Exchange" (`DefaultAccountLabelsContext`). Grouped approvals
([GROUPED-APPROVALS.md](GROUPED-APPROVALS.md)) show the same line per row.

### Scheduled tasks pinned from chat

`schedule_task` snapshots the conversation's connectors
(`chatTaskConnectorsFor`, ADR-0068). A connector on several accounts
contributes one `{server, account}` pin per account, so the task keeps all of
them and its card lists each.

## Deliberately unchanged

- **Settings → Connections stays single-account.** A user's default account
  reaches their existing chats on the next turn. A multi-account default would
  silently put every one of those chats on several accounts. Several accounts
  are a per-conversation choice only.
- **Hosted (third-party) connections stay one account per chat.** The per-user
  overlay mounts one seat per connection name. A list for a hosted connection
  is a 400.
- **`fleet chat` and ACP** send no accounts and are unchanged.
- **The Operations Center task form** is unchanged; it already pins explicit
  `{server, account}` pairs.

## Limits and honest scope

- Six accounts per connector per conversation. Every account is a process and a
  full copy of the connector's tools in the roster; a large roster is deferred
  behind `tool_search` / `tool_call` as usual.
- The default-account label on an approval card is derived in the browser from
  the conversation's current picker state, not stored on the approval. A card
  staged while the connector had several accounts stops showing the default
  label if the conversation later goes back to one account. Named accounts are
  stored on the approval and always shown.
- The rules for the assistant are instructions, not enforcement. What is
  enforced is that a call can only reach the account whose process exports the
  tool, and that an approval runs on the account it was staged under.
- A scheduled task that pins several accounts of one server gets no
  multi-account section; its prompt is unchanged by this work.
