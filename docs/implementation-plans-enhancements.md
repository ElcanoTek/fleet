# Implementation plan: #984 — Fleet ↔ Buzz bridge

**Status: NOT SHIPPED.** This is a forward-looking design note for one open
enhancement. Read it as a proposal, not as a description of fleet's behavior, and
in particular do not read the acceptance checklist below as a list of open
security findings — the unchecked boxes are criteria this feature must MEET
BEFORE it ships, on code that does not exist yet.

Everything else that used to live in this file has been removed because it had
shipped, and a shipped plan sitting next to the authoritative record is the worse
of the two copies:

| was | now recorded in |
| --- | --- |
| #987 Browserbase skill | `internal/clientconfig/builtin_skills/browserbase/`, `internal/tools/browserbase_live_view.go`, `docs/BROWSERBASE.md` |
| #985 Bento built-in skill | `internal/clientconfig/builtin_skills/bento-slides/`, `docs/SKILLS.md`, `docs/FEATURE-NOTES.md` |
| #167 three residual decisions | `docs/MCP-BROKER-SCOPES.md`, [ADR-0042](adr/0042-child-side-mcp-scope-authorization.md), [ADR-0040](adr/0040-child-owned-remote-mcp-runtime.md), `SECURITY.md` |

The #167 entry mattered most: it carried a section headed "OAuth control-plane
tokens parent-readable" whose resolution ("accepted for v1, threat model
documented — parent compromise implies stored remote-MCP tokens; agent runs stay
child-side") was already written down in `SECURITY.md` and
`docs/MCP-BROKER-SCOPES.md`. A second copy phrased as an open decision, in an
unlinked file, read like an unresolved gap. It is not one.

For the current plan-of-record on anything else, prefer the GitHub issue.

## #984 — Fleet ↔ Buzz bridge

Stand up a Buzz workspace where a human can **message a Fleet-backed agent** and get real governed fleet turns. Fleet stays execution/governance; Buzz is chat front door.

| System | Role |
| --- | --- |
| **Fleet** | Agent loop, sandbox, MCP broker, budgets, approvals |
| **Buzz** | Human/agent messaging (Nostr + ACP) |

Do **not** reimplement fleet inside Buzz. Thin bridge only.

```
Human in Buzz ──▶ buzz-acp agent ("Fleet") ──▶ fleet chat HTTP/SSE (bot user)
```

1. **Bridge process** — registers as Buzz agent; on inbound message POSTs fleet turn as service/bot user; streams SSE back to Buzz.
2. **Auth** — dedicated bot user + token (`fleet chat user add`). v1 single shared fleet user; multi-tenant mapping is v2.
3. **Capability** — text in/out; approval deep-link to Fleet UI; short tool summaries in Buzz, full detail in fleet run log.
4. **Runbook** — `docs/BUZZ.md`: create workspace → install fleet → bot user → run bridge → message agent.
5. **Tests** — contract test with mock SSE; manual integration checklist.

### Phases

| Phase | Deliverable |
| --- | --- |
| 0 | Spike ACP external command constraints |
| 1 | Bridge MVP + bot user auth |
| 2 | Streaming + error/approval messaging |
| 3 | `docs/BUZZ.md` + smoke checklist |

### Non-goals (v1)

Fleet hosting Buzz relay; full tool UI parity; every Buzz user → fleet user map.

### Acceptance

- [ ] Documented Buzz + fleet setup
- [ ] Message in Buzz → fleet turn → answer in Buzz
- [ ] Clear errors for fleet down / 401 / timeout
- [ ] Bot token not logged; secrets in env only

Size: **M** if ACP external command is clean; **L** if deep Buzz harness embed needed.
