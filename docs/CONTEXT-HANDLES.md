# Composer context handles (#517)

An OPT-IN way to hand the agent context inline from the chat composer, instead of
a separate upload/paste step. A message may contain typed handles that the server
expands into the turn context before the run:

| Handle | Expands to |
| --- | --- |
| `@url:https://example.com/page` | The fetched page, converted to markdown (HTML) or pretty-printed (JSON), injected into the turn. |
| `@file:"report.csv"` | The contents of that file **from this conversation's workspace** (e.g. a file the agent produced in an earlier turn), injected into the turn. |

Handles are expanded **concurrently**; each appears as its own block appended to
the user's message, in the order it was typed. Anything that fails to expand
(unreachable URL, missing file, disallowed path) degrades to a short **notice** —
the turn always proceeds.

## Safety

- **`@url` is SSRF-guarded.** The host-side fetch reuses the same guarded dialer
  as the `web_fetch` tool (`tools.FetchURLForContext`): private, loopback,
  link-local, and cloud-metadata addresses are refused on **every** dial
  (including redirect targets), the response is capped at 5 MiB, and non-UTF-8
  content is rejected.
- **`@file` is path-gated.** Paths resolve **only** against the conversation's
  workspace via `SafeWorkspaceJoin` — absolute paths, `..` components, and symlink
  escapes are rejected, so `@file` cannot read arbitrary host files. Each file is
  capped at 256 KiB (larger files are truncated with a marker).
- **Bounded.** At most 8 handles per message; extras are ignored with a notice.
- The expanded context enters the model prompt exactly like a manual attachment;
  the mandatory sandbox remains the execution boundary and credentials never enter
  the prompt.

## Configuration

Off by default — `@url` makes the server fetch a user-supplied URL, so it is
opt-in:

```
FLEET_CONTEXT_HANDLES_ENABLED=true
```

## Honest scope / deferred

- **v1 ships `@file` + `@url`.** `@diff` and `@folder` are deferred (per the
  issue).
- **Server-side expansion only.** The composer has no autocomplete/highlight
  affordance for handles yet — you type the handle as plain text and the server
  expands it. A composer UX affordance is a follow-on.
- `@file` resolves against the **conversation workspace** (agent-produced files),
  not arbitrary host paths — interactive chat has no general file root, and
  uploaded attachments already flow through the attachment path.
