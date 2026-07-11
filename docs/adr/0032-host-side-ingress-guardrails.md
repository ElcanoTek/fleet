# ADR-0032: Host-side untrusted-ingress guardrails

Status: accepted

## Context

Fleet's sandbox, broker, approvals, and ceilings contain prompt-injection
impact, but external instructions can still enter through chat, automation, and
tools. A model-visible instruction cannot reliably police another instruction.

## Decision

An optional workspace policy screens variable user/task text in `agentcore.Run`
and tool text in the shared tool wrappers before either reaches the provider.
The policy and detector client remain host-side. `observe` is fail-observable;
`block` is fail-closed. The system prefix is excluded.

The detector is an out-of-process HTTP interface. This avoids embedding a model
runtime and keeps policy independent of a particular classifier. The feature is
off by default and live-configurable through the workspace settings service.

## Consequences

Blocking adds detector latency and availability to each screened boundary and
can reject benign content. Observe mode lets operators calibrate first. The
detector sees raw input and must run in Fleet's trust domain. Existing execution
containment remains mandatory because detector verdicts are probabilistic.

