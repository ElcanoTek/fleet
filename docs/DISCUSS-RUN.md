# Discuss this run

## The gap

A finished scheduled run was a read-only record: the log modal shows the
transcript, artifacts, and an automated error analysis (#317), but there was
no way to *ask* about it — "why did it exclude the EU deals?" meant reading
the raw transcript yourself. Making runs first-class conversations was
considered and rejected: runs live in the sched database, conversations in
the chat database (ADR-0005), and mid-run state is deliberately not
serializable (ADR-0024).

## What shipped

A **one-way orchestrator→chat bridge** — the inverse of chat's
promote-to-task (#455), composed in the web BFF so neither backend learns
about the other:

1. The task log modal grows a **"Discuss in chat"** button (shown only when
   a transcript exists).
2. `POST /api/orchestrator/tasks/{id}/discuss` (BFF) fetches the task row +
   transcript **through the caller's orchestrator credential**, so the
   sched-side visibility gates stay authoritative — no transcript the caller
   couldn't already read via `GET /logs/{id}` can leak into a chat.
3. The BFF formats a clamped digest — task metadata, result, error,
   artifacts, then the transcript with per-message truncation (2k chars) and
   a total budget (24k chars) that keeps the **tail** (the outcome) when
   over budget.
4. `POST /conversations` (chat server) gains an optional **`seed`** field:
   one user text message persisted WITHOUT running a turn, clamped
   server-side to 48k chars (tail kept). The next real user message runs a
   normal turn with the digest in history.
5. The browser lands on `/chat?c=<conversation id>` — a new chat **deep-link
   param**, consumed once on boot and stripped from the URL, which outranks
   the "most recent conversation" default.

The seeded conversation is a completely ordinary conversation: persona
defaults apply, it appears in the sidebar, and it has no live link back to
the task — the digest is a snapshot, not a reference.

## Deliberately deferred / honest scope

- The follow-up agent has the transcript digest, not the run's workspace or
  live tool state. Workspace artifacts are listed by name only.
- Long transcripts are truncated (per-message and total); the digest keeps
  the tail. The full transcript remains in the orchestrator log modal.
- One-way only: nothing in chat updates the task, and re-running the task
  does not update the conversation.
- `seed` is a generic chat-API capability (any authenticated client may
  create a pre-seeded conversation for its own user); it appends exactly one
  user text message and can never trigger a turn.
