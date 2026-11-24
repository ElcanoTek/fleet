# Account groups: one click for "this client on every connector"

A deployment often holds several accounts on the same connectors: one per
client, per brand, per region. Setting a chat to one client used to mean
opening each connector's account menu in turn. An **account group** names that
set once (which account of which connector belong together), and the chat
Tools picker shows a chip per group. One click sets every connector in the
group to the group's accounts.

It builds on [MULTI-ACCOUNT-CHATS.md](MULTI-ACCOUNT-CHATS.md): a second group
adds its accounts beside the first, so a chat can run a connector as two
clients at once.

fleet does not know what a group is. The bundle supplies the heading
("Clients", "Brands") and the groups; fleet checks their shape and, per
request, which members exist on this box for this user.

## The manifest block

```yaml
account_groups:
  label: Clients            # the heading over the chips; optional
  groups:
    - name: The Weather Company
      # id: the-weather-company   # optional; derived from the name
      accounts:
        - {server: ix_mcp, account: twc}
        - {server: openx_mcp, account: twc}
    - name: Outcomes
      accounts:              # two accounts of one connector are fine
        - {server: ix_mcp, account: outcomes_usd}
        - {server: ix_mcp, account: outcomes_gbp}
```

Checked at load (`internal/clientconfig/account_groups.go`); a bundle that
breaks a rule does not load:

- `name` is required, one line, at most 80 characters. `id` is optional and
  derived from the name (lowercase, hyphens); ids are unique.
- `accounts` is not empty. Each `server` is an entry in `mcp_servers` with
  `optional: true`: the picker can only switch a connector that has a switch.
- `account` is an account name as the credential catalog lists it (letters,
  digits, underscores; hyphens, spaces and case are folded), or empty for the
  connector's default account.
- At most 6 accounts of one server in a group (the per-chat cap), 200 members
  per group, 500 groups.

Whether an account is provisioned is a fact about a box, not the bundle, so it
is not checked at load. See "Members that are not set up" below.

## Groups from a live source

A deployment whose groups live in another system (one per client, changed by
the people who onboard clients) names a tool that lists them:

```yaml
agent_policy:
  parallel_safe_tools: [mcp_deals_list_account_groups]

account_groups:
  label: Clients
  source:
    server: deals                 # an mcp_servers entry; called on its default account
    tool: list_account_groups     # the tool's own name on that server
    refresh_seconds: 300          # optional; 60 to 86400, default 300
```

The tool takes no arguments and answers one JSON object:

```json
{"groups": [
  {"id": "the-weather-company", "name": "The Weather Company",
   "accounts": [{"server": "ix_mcp", "account": "twc"}]}
]}
```

- `id` is optional, as in the manifest. Other keys are ignored.
- Each group is held to the manifest's rules. One that breaks a rule is left
  out and the rest are kept: one bad record elsewhere must not take every
  chip away. An answer with groups of which none is valid is a failure.
- A member's `server` is not checked against the bundle. A connector this box
  does not offer shows on the chip as not set up.
- The picker lists the manifest's groups first, then the source's. A sourced
  group whose id a manifest group has is left out.

fleet calls the tool itself, with no chat turn behind the call, so it is held
to the host-read rules ([ADR-0119](adr/0119-account-groups-source-is-a-host-read.md)):

- The tool must be in `agent_policy.parallel_safe_tools` under its registered
  name (`mcp_<server>_<tool>`) and must not be critical, and no `pre_tool_use`
  hook may match it. Otherwise it is never called: `fleet validate-config`
  reports each of the three (the `agent_policy` check) and boot logs it.
- The server's own tool list is checked before each call: the tool must be
  exported, and must not be one the server marks critical.
- The call goes through the credential owner's scope gate, on the server's
  default account, with no user identity. The list is the deployment's.
- An answer is used for `refresh_seconds`. One read after boot waits for the
  source, at most two seconds; every other read, including one that arrives
  while that first call is still running, gets the last answer (none yet, at
  first) while a refresh runs. A failed refresh keeps the last good answer and
  retries after a minute. Each call has fifteen seconds.
- A refresh is tracked server background work: shutdown waits for one that is
  in flight (at most its fifteen seconds) before the MCP runtime closes, and
  none is started once shutdown has begun.
- The answer is display data. It never reaches a model.

Load-time checks: `server` is an `mcp_servers` entry, `tool` is a tool name
(not the `mcp_<server>_` form) and, when the server lists its `tools`, is one
of them, and `refresh_seconds` is in range.

A bundle with no block is unchanged: no chips, and the catalog responses carry
no new key. Per the additive-schema rule, a bundle adds the block only after
every box it deploys to runs a fleet that knows it; an older fleet refuses the
unknown key.

## What the picker gets

`GET /mcp-servers` and `GET /conversations/{id}/mcp-servers` add
`account_groups` beside `servers` when the bundle has groups:

```json
"account_groups": {
  "label": "Clients",
  "groups": [
    {"id": "the-weather-company", "name": "The Weather Company", "accounts": [
      {"server": "ix_mcp", "account": "twc", "status": "ok"},
      {"server": "openx_mcp", "account": "twc", "status": "account_missing"}
    ]}
  ]
}
```

Each member is checked against the rows the same response lists
(`internal/httpapi/account_groups.go`):

| `status` | Meaning |
| --- | --- |
| `ok` | the connector is in this user's picker with a switch and has the account |
| `account_missing` | the connector is there, but this account is not provisioned on the box |
| `connector_missing` | the connector is not usable by this user: not set up on the box, unavailable, or turned off under Settings → Connections (including one a conversation still lists only so it can be switched off) |

## What a click does

The rules are pure functions over the picker's rows
(`web/src/app/chat/ui/accountGroups.ts`); a click is one optimistic update and
one save, like **All on**. The chip row works out which groups are on once per
change of the rows (`accountGroupState`) and answers each chip from that, so a
bundle with hundreds of groups costs one pass, not one pass per chip.

- **A group is on** when every member that can be used here is: its connector
  is on in this chat and runs as that account. Accounts are compared by name:
  for a user who set a named default under Settings → Connections, a row on
  "my default" runs as that account, so a group that names it is on.
- **Turning a group on** switches its connectors on with its accounts. On a
  connector no other group that is on names, or one that was off, the group's
  accounts replace whatever it had, so the first client replaces the
  deployment's own account. On a connector another group that is on already
  set, they are added, and the chat runs it as both.
- **Turning a group off** removes its accounts, except one another group that
  is on also names. A connector left with no account is switched off. It is
  not put back on the default account by a click that only asked to remove a
  client.
- **Connectors the group does not name are not touched.**
- A click that would put a connector past six accounts does nothing; the chip
  is disabled and its tooltip names the connector.

Nothing about a group is stored. "On" is read from the chat's connector and
account state, so a chat set up by hand to match a group shows the chip
pressed, and the account menus stay the full control.

## Members that are not set up

A group with a member that is not `ok` is flagged with a warning icon, and its
tooltip and accessible name say which connectors or accounts are missing. A
click still applies the members that can be used. A group with none is
disabled.

A member on the deployment's own account (`account: ""`) cannot be used by a
user whose Settings default for that connector is a named account: "my default"
would run as that account under this group's name. The chip says so.

A missing member is never replaced by the default account. That would run a
client's request on the deployment's own account under the client's name.

## Deliberately out of scope

- The source is one tool with no arguments. There is no per-user source, no
  paging and no push: a change in the other system shows within
  `refresh_seconds`, or after a restart.
- The source's cache is per fleet process and is not shared across replicas.
- Groups do not change Settings → Connections, scheduled tasks, or what the
  assistant is told beyond what [MULTI-ACCOUNT-CHATS.md](MULTI-ACCOUNT-CHATS.md)
  already does for a connector on several accounts.
- Hosted (third-party) connections cannot be group members.
- The chip row shows every group and filters with the picker's search. It
  scrolls past two rows of chips; there is no separate group search.
