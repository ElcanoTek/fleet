# Built-in tools in unattended runs

What a scheduled (unattended) run gets of fleet's own host-side built-ins, and
how a deployment narrows it. Decision record:
[ADR-0094](adr/0094-unattended-runs-host-fetch-follows-the-sandbox.md).

## What shipped

- **`web_fetch` follows the run's network posture.** A scheduled run's sandbox
  is sealed unless the task sets `allow_network` (**Allow network egress** in
  Advanced). `web_fetch` runs on the host, outside that seal, so it is now
  registered only when the sandbox has egress:

  | Task `allow_network` | Fleet-wide mode | `web_fetch` |
  |---|---|---|
  | off (default) | any | absent |
  | on | `lockdown` | absent |
  | on | `allowlisted` | present, limited like the egress proxy: HTTPS on port 443 to an allowlisted host (first request and every redirect) |
  | on | open / unset | present, unchanged |

- **A bundle can remove built-ins from every scheduled run:**

  ```yaml
  agent_policy:
    unattended_disabled_tools: [create_task, web_fetch]
  ```

  Allowed members are `create_task`, `web_fetch` and `download_url`. A member
  outside that set is ignored with a boot log line and fails
  `fleet validate-config`. The list wins over the task's own grants
  (`allow_task_creation`, `allow_network`).

- **`fleet task run` follows the same rules.** Its task YAML takes
  `allow_network` (default false), and it installs the bundle's
  `unattended_disabled_tools` like the server does
  (`docs/examples/local-task.yaml`).

- **Interactive chat is unchanged**: a person sees every turn, and chat's
  approval cards are the gate there.

## Operator action

Tasks that relied on `web_fetch` without network egress need **Allow network
egress** turned on (and, in allowlisted mode, the sites they read on the
allowlist). Deployments whose scheduled runs read outside email and never need
to spawn follow-ups or read arbitrary pages should set
`unattended_disabled_tools: [create_task, web_fetch]`.

## Deliberately not done

- **No approval card for unattended runs.** Nobody is there to answer one; an
  ask-style pause is possible but would stall every run that reaches it.
- **`download_url` stays available by default.** It is the documented way a
  sealed run downloads Fast.io `fleet-download://` handles; removing it by
  default would break those runs. It has the same SSRF guard as `web_fetch`.
- **`web_search` / `smart_search`** are untouched: they query fixed vendor
  hosts, not model-chosen URLs.
- **Webhook-triggered chat turns and A2A** are not scheduled runs and are not
  covered here.
