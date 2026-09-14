---
name: fleet-guide
description: Answer questions about how to use fleet itself — Chat and the Operations Center, run states, approval cards, memory and projects, the prompt library, connectors, scheduling, keyboard shortcuts, and what to do when a run or a turn goes wrong. Use it whenever someone asks how the product works ("how do I schedule this", "what does DEAD_LETTERED mean", "why did my chat disappear", "how do I share this with my team") rather than asking for work to be done on their data.
---

# fleet, explained to the person using it

You are running inside fleet, so a question about fleet is a question about the
room you are standing in. This skill is the product's own user guide: two
reference files that describe every surface a user meets, written for people who
did not build it.

Use it for **how-do-I** and **what-does-this-mean** questions. Do not use it for
the user's actual work — a request to pull a report is a request to pull a
report, not an invitation to explain the Operations Center.

## The two references

Read the one that covers the question, then answer from it:

| File | Covers |
| --- | --- |
| `skills/fleet-guide/chat.md` | The chat surface: the rail, transcript and composer; asking well; picking a model; getting data in (attachments, connectors, shared files); approval cards; memory and projects; sharing, branching, compacting; the prompt library; turning a conversation into a scheduled task; what to do when a turn goes wrong; keyboard shortcuts. |
| `skills/fleet-guide/operations-center.md` | The scheduling surface: reading the board and its counters; the anatomy of a task (prompt, schedule, recipients, tools, context); every run state including `DEAD_LETTERED` and the two paused states; running, editing, answering and rating a run; the five-step path when a run goes wrong. |

Both are plain Markdown. `view_file` one of them — do not guess at their contents
from this page, and do not read both when one will do.

## How to answer

1. **Find the answer in the file, then quote its vocabulary.** Users are looking
   at buttons with specific labels. "Open the conversation's `⋮` menu and choose
   **Download chat**" is useful; "you can export it somewhere in the menu" is
   not.
2. **Answer the question that was asked, at its own size.** A one-line question
   gets a one-line answer with the exact control named. Save the walkthrough for
   a question that is actually a walkthrough.
3. **Say where to go next when the answer spans both surfaces.** Building a
   report is chat; making it arrive every morning is the Operations Center. The
   loop — conversation → saved prompt → scheduled task → inbox — is the spine of
   both guides and usually the shape of a good answer.
4. **Offer to do it, when you can.** Many questions ("can you make this run every
   Monday?") are answerable *and* actionable: explain briefly, then propose the
   action and let the approval card do its job.

## Being honest about what you do not know

The guides describe fleet as it ships. A deployment can differ: features can be
switched off, connectors and personas come from a deployment's own bundle, model
names and cost bands are per-deployment, and an operator can change windows like
the conversation-retention period. So:

- If the guides do not cover something, **say so** and point the user at their
  administrator. Never invent a menu item, a setting, a status, or a keyboard
  shortcut — a plausible-sounding wrong button costs the user more time than
  "I don't know" does.
- If a user reports that the screen does not match the guide, believe the screen.
  Say which of the two it is — a feature their deployment has turned off, or a
  guide that has drifted — only when you actually know.
- The guides avoid naming the product (a deployment may be white-labeled) and so
  should you: say "the platform", not "fleet", when answering a user.
- Where a number is a deployment setting (the 14-day expiry for unpinned chats is
  the common one), name it as a default rather than a law.

## When something has gone wrong

Both guides end with a triage section, and they are the fastest thing you can
offer a frustrated user. Walk the list with them rather than reciting it: ask
what the banner or the status badge actually says, then take the matching row.
For anything that ends in escalation, tell them exactly what to attach — a
**Download chat** export in **Raw data** format with **Include the agent's work**
ticked, or a run's **Download logs** — because that file spares them describing
the problem twice.
