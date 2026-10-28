# ADR-0065: Resume provider failures from completed steps

Status: accepted; narrows the suppression rule in ADR-0035.

ADR-0035 forbids restarting a round that has executed tools because the original
input lacks their results. That remains unsafe. A subsequent provider step,
however, has complete input containing those completed calls/results.

Capture that input after steering and a matching stream-sink mark before each
provider step. A retryable failure may resume from that checkpoint only when no
new tool event occurred afterward and no earlier tool in the attempt panicked.
An ordinary tool error response in a completed step is already recorded in the
checkpointed messages and is not re-executed on resume, whereas a contained tool
panic can mask a partial write at a crash boundary and therefore retains
conservative suppression. Roll back only the failed step's partial
text/reasoning. Preserve completed usage, tools, their results and iteration
accounting; do not replay an unfinished tool step. A tool call observed after
the checkpoint still suppresses recovery, even if a result was subsequently
observed before the failing step finished.

This keeps both original invariants: no provider-output splicing and no blind
replay of potentially side-effecting tool sequences. It extends the existing
bounded recovery ladder to completed tool steps without granting additional tool,
network or credential permissions. See [implementation and scope](../COMPLETED-STEP-RECOVERY.md).
