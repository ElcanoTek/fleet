---
name: code-review-checklist
description: Review a git diff methodically in fixed order — correctness (edge cases, error paths, concurrency), then security (injection, authz, secrets), then test coverage, then maintainability — and report findings ordered by severity with file:line references plus an explicit list of what was NOT reviewed. Use it whenever asked to review a diff, branch, commit, or pull request.
---

# Code review checklist

Review in the fixed pass order below — correctness before style — and never
skip a pass silently. If you run out of budget, stop and say which passes you
completed.

## Step 0 — get the diff and its context

```bash
git diff <base>...<head> --stat        # scope: what changed, how much
git diff <base>...<head>               # the diff itself
git log --oneline <base>..<head>       # stated intent per commit
```

Read the surrounding code of every changed hunk, not just the hunk — most
review misses come from unread context. Note the change's stated purpose; the
review question is "does the diff do that, and only that, safely?"

## Pass 1 — correctness (highest priority)

- **Edge cases:** empty/nil inputs, zero and negative values, unicode, max
  sizes, first/last iteration, off-by-one on ranges and slices.
- **Error paths:** every error checked and propagated or deliberately ignored
  with a comment; no swallowed errors; resources (files, locks, connections)
  released on early return; partial-failure states left consistent.
- **Concurrency:** shared state guarded; no data races on maps/slices; locks
  acquired in consistent order; goroutines/threads/tasks have a shutdown path;
  no blocking calls while holding a lock.
- **Logic vs intent:** does the code do what the commit message and comments
  claim? Mismatches are findings even when the code is internally consistent.

## Pass 2 — security

- **Injection:** any user- or file-derived string reaching SQL, shell, HTML,
  paths (traversal), or format strings must be parameterized/escaped.
- **Authz/authn:** new endpoints and operations check permissions; no check
  removed or weakened; object-level access verified (not just "logged in").
- **Secrets:** no credentials, tokens, or keys in code, config, tests, or
  logs; secrets never written into error messages or debug output.
- **Trust boundaries:** input validated at the boundary; sizes/timeouts bounded
  on anything attacker-influenced.

## Pass 3 — tests

- Does every new behavior have a test that would fail without the change?
- Do bug fixes add a regression test reproducing the original bug?
- Are error paths and edge cases from Pass 1 tested, or only the happy path?
- Were existing tests modified to pass? Treat weakened assertions as a finding.

## Pass 4 — maintainability

- Naming and structure match the surrounding code; no drive-by refactors mixed
  into the change.
- Duplication that should reuse an existing helper; dead code; commented-out
  code; TODOs without an issue reference.
- Public API/doc/CHANGELOG updates where the change warrants them.

## Report format

Order findings by severity, each with a file:line reference:

- **[BLOCKER]** `path/file.go:123` — would cause incorrect behavior, data
  loss, or a vulnerability. Must fix before merge.
- **[MAJOR]** — likely bug or security weakness under realistic conditions.
- **[MINOR]** — maintainability, test-gap, or style issue worth fixing.
- **[QUESTION]** — something you could not determine from the diff; ask
  rather than guess.

End every review with a **Not reviewed** section listing exactly what you did
not cover (e.g. "generated files", "runtime behavior — not executed",
"files over N lines skimmed only", "dependency updates"). Never imply full
coverage you did not do. If you found nothing in a pass, say "Pass N: no
findings" — silence is not a verdict.
