# Implementation plans for open enhancements

Working notes for implementers. Prefer the linked issue comment/body when present.

| Issue | Plan location |
| --- | --- |
| #989 | [comment](https://github.com/ElcanoTek/fleet/issues/989#issuecomment-5198861451) |
| #988 | [issue body](https://github.com/ElcanoTek/fleet/issues/988) |
| #987 | [comment](https://github.com/ElcanoTek/fleet/issues/987#issuecomment-5198925257) |
| #986 | [issue body](https://github.com/ElcanoTek/fleet/issues/986) |
| #985 | Full plan below — **shipped**; see `internal/clientconfig/builtin_skills/bento-slides/`, `docs/SKILLS.md`, `docs/FEATURE-NOTES.md` |
| #984 | Full plan below (pending issue comment) |
| #167 | Full plan below — **all three residuals resolved**; see `docs/MCP-BROKER-SCOPES.md`, ADR-0042, `SECURITY.md` |

---

## #985 — Bento built-in skill (good first issue, size S)

[Bento](https://github.com/nyblnet/bento) decks are a **single HTML file** (viewer + editor + slides). Agent edits HTML in workspace → downloadable deck **without Gamma or any external API**.

### Approach

1. **Built-in skill** in `internal/clientconfig/builtin_skills/`:
   - `bento-slides/SKILL.md` — when to use; copy template; structure slides; what not to break.
   - `bento-slides/templates/starter.bento.html` — minimal legal template.
2. **Agent workflow:** copy template → `workspace/decks/<name>.bento.html` → edit via file tools → user downloads and opens in browser.
3. **License / attribution:** confirm redistribution allowed; attribute in skill + NOTICE.
4. **Validation:** `ValidateSkills` frontmatter; optional eval "Create a 5-slide deck about X".
5. **Docs:** one line in `docs/SKILLS.md`. No new HTTP APIs.

### Non-goals

PPTX export; hosted collab editing; PowerPoint animation parity.

### Acceptance — met

- [x] Skill shows in Settings → Skills as Built-in — no code change needed;
  `httpapi.skillSource` derives `builtin` from absence in the bundle dir. Asserted
  in `web/e2e/live/skills-connections.spec.ts`.
- [x] `/bento-slides` loads instructions — `matchSkillInvocation` resolves any
  roster name, so this came for free.
- [x] Agent produces openable `.bento.html` — via the bundled
  `scripts/bento_doc.py`; round-trip, escaping and shell byte-identity are
  covered by `internal/clientconfig/builtin_skills_bento_test.go`.
- [x] License/attribution settled — Bento is MIT (© 2026 The Bento authors).
  Recorded in `templates/NOTICE.md` (pack-local; **no** root
  `THIRD_PARTY_NOTICES.md` was added), and the shell carries upstream's own
  `NOTICE` comment internally so it travels with every deck.
- [x] Works offline except model provider — the app is vendored and embedded, so
  nothing is fetched at turn time, and nothing is fetched to render a deck.
  **Caveat, recorded rather than papered over:** upstream's shell checks
  `bento.page` for its own updates when the *user* opens the deck (on by
  default, signature-verified, `localStorage`-gated so it cannot be preset from
  the file). Enumerated in `templates/NOTICE.md`; the SKILL.md tells the agent to
  mention it when handing over the deck.

### Deviations from the approach above

1. **`templates/starter.bento.html` → `templates/Bento_Slides.bento.html`, the
   full upstream v1.0.18 release artifact vendored unmodified (689KB, sha256
   pinned).** There is no "minimal legal template": a Bento deck's shell *is* the
   application, so anything smaller would not open.
2. **The agent does not edit the HTML with file tools.** It uses a bundled
   stdlib-only `scripts/bento_doc.py` (`new`/`get`/`set`/`validate`). The document
   block sits at byte 6322 of a minified bundle, so `view_file` would spend ~125KB
   of context reaching it; and the block's `<`-escaping rule fails silently rather
   than loudly. The helper also makes `collab` private-key redaction and `docId`
   preservation mechanical instead of instructions the model must remember.
3. **`ValidateSkills` does not cover this pack** — it reads
   `Bundle.BundleSkillsDir`, i.e. the bundle's own skills, not the embedded pack.
   The real gate is `TestBuiltinSkillsPackWellFormed` plus the new bento tests.
4. **No eval case.** Evals do not run in CI, need a live model plus podman, and
   `evals.Case` has no skill field — the Go tests are the honest gate instead.

### Scope discovered while shipping

Bundle skills are **interactive-chat-only**: `internal/scheduledrun` emits no
bundle-skill roster, so scheduled tasks and `fleet task run` cannot discover this
(or any) bundle skill, even though the merged dir is bind-mounted for them.
`docs/SKILLS.md` previously implied taskrun picked the pack up unchanged; that
claim is now corrected there.

---

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

---

## #167 — Three residual decisions

Delivered broker work (can't-read) is solid. Explicit decisions:

### 1. Child-side authorization → **Implement**

Parent-only gating is insufficient (Gate-2 proof). On `OpenScope`, pass policy snapshot; child enforces allowlists on every CallTool/discovery; restrict unscoped shared client for agent paths; tests for refused disallowed tools. Update `docs/MCP-BROKER-SCOPES.md`.

### 2. Approval execution seat → **Persist staged scope**

Preserve `{server, account}` at staging; reopen scope on approve; fail closed if account revoked; show account in UI. Unblocks #988. Tests: stage with B, approve later, assert B used.

### 3. OAuth control-plane tokens parent-readable → **Accept v1 + document**

Accept connect/callback/CRUD parent-side for v1; document threat model (parent compromise ⇒ remote MCP tokens). Agent runs stay child-side (ADR-0040). Optional v2: full control-plane behind child as separate issue.

### Closing criteria — resolved

| Residual | Resolution |
| --- | --- |
| 1 Child auth | **Implemented.** `cmd/fleet/mcp_broker_authz.go`; bundle-derived Gate-2 floor, `ScopeSpec.Policy` narrowing, child-side Gate-3, filtered scope catalogs, restricted unscoped client. ADR-0042; tests in `cmd/fleet/mcp_broker_authz_test.go`. |
| 2 Approval seat | **Implemented.** Migration 048 (`approvals.mcp_server` / `mcp_account`), `BindTurnMCPScope` at staging, `OpenApprovalMCPScope` at execution, fail-closed on a revoked seat, account badge on the card. Tests in `internal/httpapi/approvals_seat_test.go`, `internal/store/approval_seat_test.go`, `web/.../ApprovalCards.seat.test.tsx`. |
| 3 OAuth parent-readable | **Accepted + documented** (2026-08-14). `SECURITY.md` and `docs/MCP-BROKER-SCOPES.md` state the threat model: parent compromise ⇒ stored remote-MCP tokens. Agent runs stay child-side (ADR-0040). Full control-plane isolation would be a separate change. |
