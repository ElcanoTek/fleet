# Implementation plans for open enhancements

Working notes for implementers. Source issues remain the work items.

## #989 Kubernetes / dual deployment mode

See comment on the issue: https://github.com/ElcanoTek/fleet/issues/989#issuecomment-5198861451

## #988 Remote MCP multi-login

See updated issue body: https://github.com/ElcanoTek/fleet/issues/988

## #987 Browserbase skill

Treat as a **skill + connector**, not a second first-class native tool in v1.

Goals: enable with API key; agent returns live view URL; user logs in / solves captcha; agent continues; explicit/TTL teardown.

Non-goals: replace in-sandbox Playwright; unattended captcha; secrets in sandbox env.

Design: BROWSERBASE_API_KEY via broker/catalog; skill pack; clickable live URL; fail closed under network=none; CI skips live calls without secret.

Phases: (1) skill + wiring + docs (2) e2e checklist (3) optional Connections entry.

## #986 Test official MCPs / improve catalog

Phases: inventory matrix → automated smoke → manual OAuth pack (GitHub, Google, Slack|Notion, Stripe) → hide dead / fill hints → UX polish for found bugs only.

## #985 Bento built-in skill (size S)

Built-in skill + template under builtin_skills/bento-slides/. Agent edits single-file HTML deck. No external APIs. Confirm license.

## #984 Fleet ↔ Buzz bridge

Thin bridge: Human in Buzz → buzz-acp agent → fleet chat HTTP/SSE as bot user. v1 text in/out + approval deep-link. docs/BUZZ.md runbook.

## #167 Three residual decisions

1. Child-side authorization → Implement (policy snapshot on OpenScope).
2. Approval seat → Persist staged scope (unblocks #988).
3. OAuth parent-readable → Accept v1 + document; optional v2 separate issue.

Sequence: 2 → 1 → 3 docs. Verify on dev code.
