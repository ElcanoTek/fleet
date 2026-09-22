# ADR-0072: A declared completion predicate may replace the end-of-run verifier; a verifier outage does not dead-letter an audited run

- **Status:** Accepted
- **Date:** 2026-09-22
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0001](0001-one-governed-run-loop.md). ADR-0001 places the
  end-of-run verifier inside the single governed path, and the runtime docs
  said it "applies to every scheduled run". The verifier now has a declared,
  deterministic substitute, and its own outage has a bounded fail-open.

## Context

The end-of-run verifier is a model call. It reads the task text, the tool
records and the closing message, and judges whether every required action was
attempted. On 2026.09.22.4 it was the largest single cause of dead-lettered
page refreshes, and every such run had the correct outcome already recorded by
Pages:

- `07e43224`: no new coverage, `record_refresh_check(source_not_updated)`
  written, v853 still live. The verifier demanded a publish.
- `11b2880d` and `4207d823`: the blocked branch was recorded. The verifier
  demanded an upload.
- `00d8224c`: data published. A wording nit in the report dead-lettered the
  run.
- `19c59abc`: the verifier call itself failed with `context deadline exceeded`,
  and the run dead-lettered after three checks.

Each failure cost three verifier calls plus up to two full-context repair rounds,
and then discarded correct work. The prompt producer already knows, exactly and
without judgement, which tool executions mean "done": a Pages refresh either
created a version or recorded a check.

## Decision

1. **Declared completion predicate.** A task's `EXECUTION REQUIREMENTS` may
   carry `completion.any_succeeded: [tool names]`.
   - The names take the same forms, identifier rule and dispatch roster check
     as `required_tools`. A malformed clause fails closed, and an unknown name
     is a dispatch error.
   - In the scheduled policy's `CanFinish`, **after** the audit/finish
     enforcement has cleared, a successful execution of any listed tool
     completes the run. "Successful" uses the same tool records and success
     classification the verifier reads.
   - Such a run skips the end-of-run verifier and phone-a-friend, recording a
     breadcrumb and an event.
   - The predicate never replaces the audit gate: an outstanding declared
     commitment still blocks.
   - With no listed success, the verifier runs as before. A task without the
     clause is unchanged.
   - Fleet assigns no meaning to the names; they are the producer's opaque
     contract.
2. **A verifier outage does not spend a check.** A verifier call that returns
   no verdict (timeout, provider failure, empty or unparseable reply) is
   retried once.
   - If it still returns none, and the audit cleared with no critical tool
     whose last execution failed, the run **succeeds** with a
     `completion_unverified_verifier_error` warning. The warning is written to
     the session log, and the runner prefixes it to the task's terminal
     message.
   - With a failed critical call on the record, the outage keeps the previous
     semantics: it spends a check toward the three-check cap.
   - A verifier that **answers** with missing actions keeps its repair and
     dead-letter semantics unchanged.

## Enforcement

- `internal/scheduledrun/requirements.go`: `completionRequirement`, parse
  validation, `checkTools` ("completion tool …"), and `resolveCompletion`.
- `internal/agent/scheduled.go`:
  - `scheduledPolicy.CanFinish`, with the predicate placed after
    `inner.CanFinish`;
  - `completionPredicateTool`;
  - the Gate 1 retry and fail-open, plus `failedCriticalCalls`;
  - `persistVerifierWarning`.
- `internal/runner/runner.go`: `successMessage` flags the terminal message.
- Tests:
  - `internal/agent/completion_predicate_test.go`: predicate satisfied,
    unsatisfied, blocked by the audit; outage fail-open and retry; a failed
    critical call keeps the dead-letter.
  - `internal/agent/scheduled_completion_test.go`: an unparseable verdict now
    fails open after one retry.
  - `internal/scheduledrun/requirements_completion_test.go`.
  - `internal/runner/completion_warning_test.go`.

## Consequences

- A producer that declares completion gets deterministic, zero-model-call
  finishes for the branches it names. It also takes responsibility for that
  list being right, so a wrong list finishes runs the verifier would have
  caught.
- A verifier outage can no longer dead-letter an audited run whose critical
  calls all landed. Such a run is marked, visibly, as unverified rather than
  failed.
- A verifier model that persistently returns malformed output would make every
  such run succeed unverified, and every one of them says so.
- The three-check cap now counts verdicts, not calls. The worst case is six
  verifier calls: three checks, each retried once. Each is metered in
  `aux_usage`.

## Alternatives considered

- **Teach the verifier the branches better** (prompt work). This was tried
  repeatedly. It remains a model judgement and still fails closed on its own
  outage.
- **Fail open on every verifier error.** Rejected: with a failed critical call
  on the record, the outcome is genuinely in doubt, and an unverified success
  there would hide exactly the runs the verifier exists for.
- **Let a satisfied predicate skip the audit too.** Rejected: the audit gate
  binds declared critical actions (ADR-0034). The predicate is a completion
  signal, not an authorization.
