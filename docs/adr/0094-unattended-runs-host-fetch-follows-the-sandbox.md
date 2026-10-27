# ADR-0094: In unattended runs, host-side fetch follows the sandbox's egress, and a bundle may remove built-ins

- **Status:** Accepted
- **Date:** 2026-10-27
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0036](0036-sandboxed-file-tools-and-host-io-exceptions.md)
  (host network / brokered fetch exception class), for scheduled runs only
- **Relates to:** [ADR-0012](0012-sandbox-egress-allowlist.md),
  [ADR-0085](0085-interactive-hidden-tools.md)
- **Design note:** [`UNATTENDED-BUILTINS.md`](../UNATTENDED-BUILTINS.md)

## Context

A scheduled run is sealed by default: its sandbox has no network unless the
task sets `allow_network`. But `web_fetch` is a host-side tool (ADR-0036), so a
sealed run still had it — an open channel to any public URL, with nobody at the
keyboard. Runs that read outside input (an inbox, vendor reports) are the ones a
crafted message can steer, and through `web_fetch` it could both send data out
in a URL and pull new instructions in. In allowlisted mode the host-side fetch
also ignored the allowlist the sandbox's proxy enforces.

`create_task` is already opt-in per task (`allow_task_creation`), capped per
run and budget-limited, but a deployment had no way to switch it off for all
unattended runs, and a spawned task's prompt is what its run treats as its
instruction.

There is no person to read an approval card in a scheduled run, so "require
approval" is not available there; the safe options are deny and narrow.

## Decision

1. In a scheduled run, `web_fetch` is registered only when the run's sandbox
   has egress (`allow_network` set and fleet-wide mode not `lockdown`). In
   `allowlisted` mode it is held to the same allowlist as the egress proxy
   (`sandbox.DomainAllowed`), on the first request and on every redirect.
   `allow_network` is therefore the explicit, per-task opt-in.
2. `agent_policy.unattended_disabled_tools` lets a bundle remove
   `create_task`, `web_fetch` and/or `download_url` from every scheduled run,
   whatever the task's own grants say. Other names are refused (boot log line,
   `validate-config` failure); governance tools cannot be named.
3. Interactive chat is unchanged.

## Consequences

- A scheduled task that used `web_fetch` without `allow_network` loses it.
  Operators set **Allow network egress** on the tasks that need it. This is a
  behavior change, stated in the release notes.
- `download_url` keeps its current behavior by default: it is how sealed runs
  pull Fast.io `fleet-download://` handles. A bundle that does not need that
  can remove it with the list above.
- The rule lives in the scheduled driver as data applied to the native tool
  list (one function, `applyUnattendedBuiltinPolicy`), not as a branch in the
  governed loop.
