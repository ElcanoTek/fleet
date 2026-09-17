# ADR-0068: A task scheduled from chat inherits the conversation's connectors

Status: accepted; amends ADR-0052 (chat-created tasks) and the #239 / #455
approval-card contract.

## Context

ADR-0052 defines a scheduled task's `mcp_selection` as its optional connector
additions; a run binds those plus the bundle's always-on servers. The chat-side
scheduling contract (`tools.ScheduleTaskParams`) carried no connectors, and
neither entry point — the agent's `schedule_task` call or promote-to-task —
supplied any. Every task scheduled from chat was therefore saved with an empty
selection. In a bundle whose connectors are all optional that task ran with no
email, no mailbox and no data feeds: the agent did the whole report, its
self-audit correctly found it had no way to send, and the run aborted. Nothing
warned at creation time, and the only repair was knowing to edit the connector
list in the Operations Center.

## Decision

When a `schedule_task` card is staged — by the agent or by promote-to-task —
the stager resolves the conversation's live connector selection: the optional
bundle servers the conversation opted in (with their seats) and the user's
hosted connections, filtered by the same connections-page preferences a chat
turn applies. That snapshot is written into the staged args alongside the
agent's parameters, shown on the approval card, and written to the task's
`mcp_selection` on approval. The snapshot keys are not part of the tool schema
the model sees; anything the model puts there is overwritten. The
conversation's toggles are the authority, so a chat-created task can hold
exactly the connectors its author had already enabled and nothing more.

The card names the inherited connectors. When the snapshot is empty and the
bundle has no available always-on server, the card warns that approving as-is
schedules a task with no connectors and says where to fix it. The chat
confirmation after approval repeats the connector list. A stager that cannot
resolve the conversation fails the staging call rather than staging a silent,
connector-less card.

## Consequences

Tasks scheduled from chat run with the tools the chat had. Always-on semantics
(ADR-0052), `credential_allowlist`, persona policy and tool allowlists are
unchanged and still narrow the run. Tasks staged before this change carry no
snapshot; their cards render the no-connector warning, which is the truth about
them. Operators editing a task's connectors in the Operations Center keep the
final say.
