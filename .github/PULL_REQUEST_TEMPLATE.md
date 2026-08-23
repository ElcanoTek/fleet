<!--
Thanks for contributing to fleet. The prose boxes matter more than the
checkboxes — a reviewer can verify a checkbox themselves, but only you can
explain why.
-->

## What changed, and why

<!-- One or two paragraphs. What problem does this solve? -->

## How you verified it

<!--
Name what you actually ran, not what you intended to run. `make lint`,
`make test`, `make ci-web`, the mocked Playwright suite, a manual check —
and what it said. "CI will tell us" is not a verification.
-->

## Scope and deviations

<!--
Per AGENTS.md: what shipped, what deviated from the issue, and what you
deliberately deferred. If nothing deviated, say so — that is a useful
sentence, not a wasted one.
-->

---

- [ ] Commits are signed off (`git commit -s`) — see CONTRIBUTING.md
- [ ] `CHANGELOG.md` updated, if this is a user-visible change
- [ ] A design note (`docs/<FEATURE>.md`) added, if this ships a feature
- [ ] An ADR added or superseded in `docs/adr/`, if this adds, weakens or
      reverses an invariant — required in the *same* PR
- [ ] The diff is scoped to one change (no unrelated refactors)
