# ADR-0027: Event-driven triggers 2.0 — email ingress

- **Status:** Accepted
- **Date:** 2026-07-02
- **Deciders:** fleet maintainers

## Context

fleet already fires tasks on a cron cadence and on a generic signed webhook
(#177, `TriggerType` cron/webhook, migration 021). Issue #511 asks to let agents
wake on real-world events — inbound email to a per-agent address and
connected-source changes — turning fleet from a batch worker into a real-time
operator. The maintainer scoped the delivery explicitly: **email ingress is v1**
(the Manus "Mail Manus" pattern: a unique address, approved senders, a run that
replies with the result), then a trigger-management UI, then source-change
ingress. A hard rule accompanies it: an event trigger must be **opt-in per task**
and must never inherit write-capable connectors unless the task explicitly opted
in, so an untrusted inbound event can't auto-escalate.

Two design pressures shape the decision:

1. **No second ingress path.** The webhook trigger already proves the
   authenticate → clone template → spawn one governed run path. Email must reuse
   it, not fork a parallel one.
2. **fleet hosts no SMTP server.** Running inbound SMTP (MX records, TLS, spam
   handling) is a large operational surface and out of scope. The realistic v1 is
   to accept a *normalized* inbound-email JSON from an email provider's
   inbound-parse webhook (Postmark/Mailgun/SendGrid) — vendor-neutral, and the
   provider already does DKIM/SPF verification.

## Decision

Add an **email trigger kind** layered on the existing `task_triggers` machinery:

- `task_triggers.kind` (`webhook` | `email`, default `webhook`) discriminates a
  trigger row; `email_policy` (JSONB, nullable) holds the email-kind controls.
  The template task stays `trigger_type='webhook'` (inert — the cron engine never
  runs it); the email-ness lives on the trigger row, so there is **no** new task
  `TriggerType` and **no** new trigger table.
- `POST /triggers/email/{slug}` authenticates with the **same** per-trigger
  HMAC-SHA256 secret and timing-equalized-401 logic as the webhook endpoint
  (unknown slug, wrong-kind slug, and bad signature are indistinguishable — no
  enumeration), then enforces the email policy: approved senders, provider-reported
  DKIM/SPF requirements, attachment count/size caps, and `Message-ID` dedup via a
  `trigger_events` idempotency ledger. It spawns the run through the same
  `storage` → `models.NewTask` → `AddTask` constructor + `agentcore.Run` loop the
  webhook path uses.
- **Connector opt-in is the security boundary.** A per-task
  `allow_event_triggers` boolean (threaded like `allow_network`) gates whether an
  event-spawned run inherits the template's write-capable connectors. Off (the
  default) ⇒ the spawned run gets native tools only (no MCP selection, no
  credential allowlist). A connector needs both its selection and its credential
  allowlist to write, so the opt-in gates both together. The #177 webhook path is
  unchanged (it always inherits its historical MCP-selection subset).
- **The run is tied back to the event, and can reply.** The `trigger_events` row
  records sender + subject + `Message-ID` and links the spawned `run_id`. On a
  successful run, when SMTP is configured for sending, an off-thread reply-back
  seam (`EmailReplier`, implemented by the existing `notify` package) emails the
  result to the original sender, threaded via `In-Reply-To`/`References`. Untrusted
  header values are CR/LF-sanitized to foreclose header injection.

## Consequences

- One ingress path, one governed core: email is an inbound I/O adapter over the
  same spawn + run machinery, not a second governance path.
- Safe by default: without `allow_event_triggers`, an email-triggered run cannot
  touch any connector; without a configured policy, an email trigger rejects
  everything. Both fail closed.
- Vendor-neutral: any provider (or a small forwarder) that can POST the normalized
  JSON works; fleet is not coupled to one email vendor's payload.
- **Deferred (honest scope):** source-change ingress (the issue's v2, reusing this
  same table + opt-in), a trigger-management web UI (management is CLI + API for
  now), "needs-input" replies (ties to the ask/notify pause #510), per-connector
  granularity of the opt-in, and any ingestion of attachment *content* (metadata
  only). Reply-back's SMTP send reuses the existing tested `notify` sender but is
  not verified against a live provider in CI.

## Alternatives considered

- **Run an inbound SMTP/IMAP server in fleet.** Rejected for v1: large
  operational surface (MX/TLS/spam), and it does not remove the need to normalize
  and DKIM/SPF-verify — which providers already do well.
- **A separate email-trigger table + endpoint.** Rejected: it would duplicate the
  security-critical auth/spawn path and risk drift from the webhook trigger.
- **Inherit connectors by default and let operators restrict.** Rejected: it
  violates the issue's hard security default; untrusted inbound events must not
  auto-escalate.
