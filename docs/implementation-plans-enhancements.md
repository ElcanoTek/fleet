# Implementation plans: enhancements (all shipped)

**No plan is open here.** This file held forward-looking design notes for open
enhancements. Each was removed once it shipped, because a shipped plan sitting
next to the authoritative record is the worse of the two copies:

| was | now recorded in |
| --- | --- |
| #984 Fleet ↔ Buzz bridge | `fleet acp` (`internal/acp/`, `docs/ACP.md`); for Buzz, the separate [`fleet-buzz-bridge`](https://github.com/ElcanoTek/fleet-buzz-bridge) ("Buzz" in `docs/ACP.md`) |
| #987 Browserbase skill | `internal/clientconfig/builtin_skills/browserbase/`, `internal/tools/browserbase_live_view.go`, `docs/BROWSERBASE.md` |
| #985 Bento built-in skill | `internal/clientconfig/builtin_skills/bento-slides/`, `docs/SKILLS.md`, `docs/FEATURE-NOTES.md` |
| #167 three residual decisions | `docs/MCP-BROKER-SCOPES.md`, [ADR-0042](adr/0042-child-side-mcp-scope-authorization.md), [ADR-0040](adr/0040-child-owned-remote-mcp-runtime.md), `SECURITY.md` |

The #167 entry mattered most: it carried a section headed "OAuth control-plane
tokens parent-readable" whose resolution ("accepted for v1, threat model
documented — parent compromise implies stored remote-MCP tokens; agent runs stay
child-side") was already written down in `SECURITY.md` and
`docs/MCP-BROKER-SCOPES.md`. A second copy phrased as an open decision, in an
unlinked file, read like an unresolved gap. It is not one.

#984 shipped differently from its plan. The plan's bridge (a Buzz agent posting
fleet turns over the chat API) became `fleet acp`, an ACP agent in the fleet
binary that any ACP client launches, plus a small adapter for Buzz's
`buzz-acp`, which expects an agent to post its own replies. The planned
`docs/BUZZ.md` runbook is the "Buzz" section of `docs/ACP.md`.

For the current plan-of-record on anything else, prefer the GitHub issue.
