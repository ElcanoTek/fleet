# Example Protocol

Protocols are reusable playbooks a client bundle ships under `protocols/`. The
agent reads one on demand — when the user references it by name or it is clearly
relevant — and follows its steps.

This generic example exists so a bare fleet deployment has a non-empty
`protocols/` directory and the loader has something to list. Replace it with
your own protocols in a client bundle.

## When to use

When the user asks to "run the example protocol", or as a template for writing
your own.

## Steps

1. Restate the goal in one sentence.
2. Gather the inputs the task needs (ask only for what you can't infer).
3. Do the work with your tools.
4. Summarize the result and flag anything that needs follow-up.
