# Recovery after completed tool steps

A provider can fail while composing a response after a tool already completed.
Fleet now resumes from the next provider step's input, which includes the prior
tool calls and results. It does not restart the original tool sequence.

The checkpoint is captured after queued user input is injected and before the
provider call. Recovery is permitted only when no tool event occurred after that
checkpoint and no earlier tool step in the attempt returned an error. Failed
tool results, including contained crashes, may hide a partially executed write.
If a tool began during the failed step, recovery also remains suppressed:
the existing `ErrCommittedSideEffects` and task retry policy apply. Cancellation,
budget exhaustion, validation and authentication errors do not gain retries.

Completed step usage remains charged once. Completed steps count against the
iteration limit after recovery. The failed step's partial text is discarded;
completed records are preserved. Provider-specific reasoning blocks are omitted
from replay. System prompts are not duplicated, and the
normal context/budget/governance checks run again on the resumed input.

The provider adapter can retain a numeric HTTP code in an SSE error body while
losing its retry classification. Fleet recovers that code/type from the structured
body without logging it. Explicit 401/402 (credential/billing) errors stay
terminal; any other explicit 4xx is a per-request rejection that promotes a
configured fallback model once, from the last safe checkpoint, and is otherwise
terminal ([ADR-0067](adr/0067-fallback-on-provider-rejection.md)); 429/5xx errors
follow the bounded recovery ladder. The exact opaque `stream error: Provider
returned error` can also use bounded recovery without inventing an HTTP status.
Other unclassified errors stay terminal. Terminal provider errors include the
available status in their error description and, when a gateway relayed an
upstream failure, the upstream provider name, error type, provider code and a
redacted, bounded raw message read from `error.metadata`. Exported logs retain the model,
classification, available status (zero means unknown), and a bounded redacted
message; raw provider response bodies are not logged. This does not change `max_retries`,
network permissions, fallback configuration or any connector contract.

Regression coverage exercises a real completed tool step followed by provider
failure: the fallback sees its result, the write executes once, partial response
text is absent and completed usage is not lost. The existing tests for failure
during a tool step continue to require recovery suppression.

The scheduled verifier also receives full redacted tool records instead of UI
previews and rechecks requested repairs, bounded to three reviews. See
[conditional completion](CONDITIONAL-TASK-COMPLETION.md). Live customer sources,
schemas and schedules are not changed by this work.
