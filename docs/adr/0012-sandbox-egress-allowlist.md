# ADR-0012: Sandbox network egress allowlist is best-effort, not a hard boundary

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers
- **Relates to:** ADR-0002 (mandatory rootless-Podman sandbox), issue #211

## Context

Until now a sandbox's network posture was binary (ADR-0002): **lockdown**
(`--network=none` — an empty network namespace, the hard seal used where
exfiltration is in the threat model) or **open** (rootless slirp4netns, full
outbound egress, so `pip install` and `curl` work in routine data flows).

Real tasks often want the missing middle: "let this task reach PyPI and
GitHub, nothing else." Issue #211 asks for a per-domain egress allowlist.

The only mechanism that fits rootless Podman without privileged
network-namespace machinery is an **HTTP CONNECT proxy with `HTTPS_PROXY`**.
That mechanism has a hard limit worth stating plainly: `HTTPS_PROXY`/`HTTP_PROXY`
are **advisory** — they constrain only clients that choose to honor them (pip,
curl, most HTTP libraries). A process inside the sandbox that opens a raw socket,
or simply ignores the proxy environment, still has full slirp4netns egress.
Against an adversarial, prompt-injected agent trying to exfiltrate, a proxy-env
allowlist provides **no** containment.

A hard boundary (firewalling the container's network namespace so *all* egress
is forced through the proxy) was considered and rejected for this change — it
requires rootless-Podman-and-distro-specific nftables/custom-network-backend
machinery that is hard to test and high-risk to operate.

## Decision

We add a third network mode, **allowlisted**, implemented as a host-side CONNECT
proxy (`internal/sandbox/proxy.go`, `EgressProxy`): a sandbox in this mode runs
on slirp4netns with `allow_host_loopback` and has `HTTPS_PROXY`/`HTTP_PROXY`
pointed at the proxy, which permits a CONNECT tunnel only to hosts on the turn's
allowlist (exact domains or `*.`-wildcards). Each turn gets a fresh random token
(carried as the proxy URL's basic-auth userinfo) bound to its allowlist, so one
turn's grant cannot be reused by another.

This mode is **explicitly a best-effort control for proxy-honoring clients, NOT a
security boundary against a hostile process.** We state this in the code
(`EgressProxy` doc), in operator docs (`AGENTS.md`), and here.

- **Lockdown remains the hard seal.** It is unchanged (`--network=none`) and is
  the only posture valid when adversarial exfiltration is in the threat model.
  It stays the default for sealed scheduled runs and lockdown conversations.
- **Allowlisted is opt-in and fails closed.** It is selected only by
  `FLEET_DEFAULT_NETWORK_MODE=allowlisted`. If the proxy cannot be stood up, boot
  fails; a request for an allowlisted sandbox with no proxy configured returns an
  error rather than silently downgrading to open egress
  (`Pool.TakeContainerWithEgress`).
- **`lockdown` as the default-mode value** is an egress kill-switch for the
  paths this change wires: it seals every **scheduled-task** sandbox, overriding
  any per-task `AllowNetwork`. (Interactive chat turns are not yet wired — see
  the deferred-scope note below — so the value is scoped to scheduled tasks
  today, not literally "every sandbox fleet-wide".)

This does not weaken ADR-0002: allowlisted is strictly *more* restrictive than
the pre-existing **open** mode (which already grants unrestricted egress), and
**lockdown** is untouched. No posture that was sealed becomes unsealed.

## Enforcement

- `internal/sandbox/proxy_test.go` — the CONNECT proxy's allow/deny, per-turn
  token auth, and domain matcher (incl. label-boundary guards) are unit-tested.
- `internal/sandbox/network_args_test.go` — `networkArgs` proves lockdown wins
  over a proxy URL, open emits no network flag, and allowlisted emits
  `slirp4netns:allow_host_loopback` + the proxy env (never `--network=none`).
- `internal/sandbox/pool_egress_test.go` — `TakeContainerWithEgress` fails closed
  (a distinct error, not open egress) when no proxy is configured.
- `internal/config/config.go` rejects an unknown `FLEET_DEFAULT_NETWORK_MODE`.
- The host-loopback reachability and that the sandbox image's `curl` honors
  `HTTPS_PROXY` via CONNECT were verified manually against rootless Podman + the
  real sandbox image (see the #211 PR description); these depend on the runtime
  environment and are not unit-testable.

## Consequences

- Operators get a useful middle ground for *cooperating* workloads (CI/data
  tasks that need PyPI/GitHub and nothing else) without opening full egress.
- Allowlisted turns always cold-start (the per-turn proxy token must be fresh),
  so they do not benefit from the warm pool — a deliberate cost.
- Operators MUST NOT treat allowlisted as a containment boundary for untrusted
  input. The docs say so; mislabeling it would violate the honesty-in-docs
  invariant.
- Per-task / per-conversation allowlist overrides and a web UI are deferred
  follow-ups; this change ships the default-mode knob + the scheduled-task path
  ONLY. The interactive chat sandbox path (`takeTurnSandbox`) does NOT yet
  consult the mode — a non-lockdown chat turn still gets open egress regardless
  of `FLEET_DEFAULT_NETWORK_MODE`. Wiring chat is a follow-up.

## Alternatives considered

- **Hard netns firewall (all egress forced through the proxy).** A real boundary,
  but rootless-Podman/distro-specific, hard to test in CI, and high-risk to
  operate. Deferred; would warrant its own ADR superseding the "best-effort"
  framing here.
- **Keep the binary lockdown/open model.** Rejected: it forces operators to grant
  full egress for any task that needs one domain.
