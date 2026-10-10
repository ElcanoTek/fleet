# Resume after approval: the agent carries on once a card is settled

Design note for `agent_policy.critical_tool_resume`: a bundle can say that,
for chosen critical tools, a settled approval card starts one new agent turn in
the conversation, so the agent verifies the outcome and continues without the
user typing. Recorded as
[ADR-0083](adr/0083-a-settled-approval-card-may-start-a-turn.md), because it
changes the rule that every chat turn starts from something a person sent. It
builds on [APPROVAL-CARDS.md](APPROVAL-CARDS.md) (the card lifecycle),
[APPROVED-CALL-BUDGET.md](APPROVED-CALL-BUDGET.md) (the detached `executing`
path) and [INPUT-QUEUE.md](INPUT-QUEUE.md) (queued turns).

## Why

An approval card resolves outside any turn. When a person approves, declines,
or lets a card time out, fleet writes the outcome into the conversation's
history (`SetApprovalResult`, `appendToolResultToHistory`) and stops. The model
reads it only when the user types again. In the deal-update pilot, three
updates took seven user interactions ("approve", "done", "approve", …): the
agent said "after approval I'll verify" and never did, and a batch with one
card per SSP group stalled after every card. The designer's requirement R1
asks that the agent continue on its own.

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [create_deal, update_deal]
  critical_tool_resume: [create_deal, update_deal]
  critical_tool_resume_max_per_hour: 10   # optional; 1-60, default 10
```

- Suffix matching exactly like `critical_tools` (the tool name equals the
  suffix or ends in `_<suffix>`, so a named-account variant is covered).
- A member that is not a critical suffix, or whose `critical_tool_modes` entry
  is `notify`, stages no card and so could never resume: boot drops it with a
  log line, and `fleet validate-config` reports it in the `agent_policy` floor
  check (`agentcore.ResumeAfterApprovalProblems`, the same code). So is a cap
  outside 1–60, which falls back to the default.
- Installed at boot from `cmd/fleet/main.go` like every other `agent_policy`
  value. The scheduled `fleet run` path stages no cards and does not install
  it.
- **Absent key = today's behaviour, exactly.** `noteApprovalSettled` returns
  before touching anything when no tool opted in, no card is armed, the
  approve/decline answers carry no new key, and the card payload gains no new
  field (`resume_after_approval` is only sent when true).

### The flow

1. **Staging arms the card.** `approvalStager.Stage` creates the approval row
   and, for an opted-in tool, sets `approvals.resume_state = 'armed'`
   (migration 072). The intent is durable from the moment the card exists. A
   card superseded by a newer call of the same tool is disarmed
   (`'superseded'`) by `SupersedePendingApprovals`, so it never resumes.
   Handler-only cards (`schedule_task`, `manage_tasks`, `preview_email`, model
   suggestions) are never armed.
2. **Every settlement kicks a debounce.** The approve path (after
   `SetApprovalResult` has committed the outcome and its history breadcrumb,
   including a call that answered `executing` and finished detached), the
   decline path, the expired click, the expiry sweep and the suggestion card
   all call `noteApprovalSettled(conversation)`. It (re)arms a per-conversation
   debounce of 2 s (`approvalResumeDebounce`). A card that did not opt in kicks
   too: it may be the card a waiting resume depends on.
3. **One transaction decides** (`store.ClaimApprovalResume`), when the
   debounce fires:
   - It locks the conversation row (`deleted_at IS NULL`). A deleted
     conversation is never resumed; its settled armed cards are marked
     `dropped`.
   - If any other card of the conversation is still pending (display-only
     `preview_email` cards excepted: they never expire) or still executing,
     nothing changes. That card's own settlement kicks again. This is the
     coalescing: several cards settled close together (one per SSP group,
     say) produce one turn, after the last of them.
   - Otherwise it claims every armed, settled card (`'claimed'`) and inserts
     ONE input-queue row of mode `resume` (migration 073/074), keyed
     `approval-resume:<first card id>`, all in the same transaction. A card can
     therefore resume at most once, whatever happens to the process between
     the claim and the turn.
   - Over the hourly cap (counted from the conversation's `resume` rows in the
     last hour), or with the conversation's queue already at its depth cap
     (20), it marks the cards `'skipped'` and appends a notice to the
     conversation instead ("Automatic continue skipped: … Send a message to
     continue."). No turn starts.
4. **The turn runs through the ordinary queue.** `maybeDrainQueue` launches
   the row through `launchQueuedTurn → startTurn → runTurnAsync →
   agentcore.Run`, the same path as a follow-up the user queued. So it has the
   same governance, persona, model (including the lockdown migration),
   connector selection and seats (read from the conversation), per-user
   concurrency cap, turn timeout and cost/token ceilings as any turn. If a
   turn is already running, the row waits in the queue (the web shows it as an
   **auto-continue** chip that can be removed or sent now) and that turn's
   completion drains it. A Stop with scope all cancels it like any queued
   input.
5. **The input is fleet's, labelled.** The row's message, and so the turn's
   user-role input, reads for example:

   ```
   [Approval resolved] mcp_deals_update_deal approval_id=… outcome=approved.
   The result is in the conversation; continue the task: verify it and carry
   on, or stop if nothing remains. Do not retry a declined or timed-out action
   unless the user asks. (Written by fleet, not the user.)
   ```

   Several cards read `[Approvals resolved] a approval_id=… outcome=approved;
   b approval_id=… outcome=declined. …`; an approved call that reported an
   error adds `result=error`. The outcome is `approved`, `declined` or
   `timed_out`. The turn carries `agent.InputKindApprovalResume`: the
   persisted user entry has `content.kind = "approval_resume"`, `user.message`
   carries `kind`, and both `turn.started` frames carry `input_kind`. The
   blocks that read the user's words (context handles, `/skill` invocation,
   connector hints) and memory auto-indexing are skipped for it; the
   workspace, shared-file and branch blocks are not.
6. **A resume never approves anything.** The new turn's critical calls go
   through the same stager and stage new cards. A tool in
   `critical_tool_no_session_approval` cannot carry a session pre-approval at
   all; for any other tool, only a policy the user set with "apply to all"
   applies, exactly as in a turn they typed.

### Restart: dropped with a note, never run

Choice: a resume that was due but had not started when the process stopped is
**dropped**, with a note in its conversation. It is never started by a restart.
This is the input queue's rule (boot recovery never auto-drains: a restart
must not start unattended LLM spend), and it removes any chance of a resume
running twice.

- `DropApprovalResumesAtBoot` runs at boot after `RecoverStrandedApprovals`
  (which records a card cut off mid-call as outcome unknown) and
  `RecoverInputQueue` (which returns an uncommitted running row to the queue).
  In one transaction it marks every armed, settled card `dropped`, cancels
  every `resume` row still queued, and appends one notice per conversation:
  "Automatic continue was not started because fleet restarted before it could
  run. Nothing was re-run. Send a message to continue."
- A `resume` row whose turn had committed its input before the crash is
  completed by `RecoverInputQueue` (the turn ran; stranded-turn recovery
  projects it) and is never re-run.
- A card still pending at boot stays armed: when it is settled in the new
  process it resumes normally.
- The sweep runs on its own 30 s deadline with one retry, and the guarantee
  does not depend on it: `launchQueuedTurn` refuses any `resume` row created
  before this process started (`Server.processStart`), cancels it and writes
  the same note. A settled armed card the sweep missed is only claimed if a
  later settlement in the same conversation triggers a decision, and then it
  joins that person-triggered resume.
- While the process drains for shutdown, `decideApprovalResume` decides
  nothing, so the cards stay armed for the boot sweep.

### Clients

- **Web.** The approve/decline answer carries `"resume": true` for an armed
  card, and so does any idempotent replay of it (a lost answer, a second tab,
  **Check result**) once the card is claimed. It also does for a card that did
  not opt in while another card of the conversation is still armed, since
  settling it may release the resume that was waiting on it. The card then calls `followApprovalResume`, which probes `/inflight`
  on a backoff (first look after 2.5 s) and attaches to the turn fleet started,
  which streams like any turn. It follows for 30 s after a settled card, and
  for up to 31 minutes when the answer was `executing` (the approved-call
  budget's ceiling is 30). A conversation reloaded while such a card is still
  executing follows the same way (`resume_after_approval` on the executing
  card). If nothing appears in time it reloads the transcript, which shows a
  turn that ran unseen or the note saying why none started. A hidden tab keeps
  waiting without probing; the existing tab-return handler reattaches.
- The resume input renders as a muted **Continued automatically after an
  approval — not typed by you** row (exact text behind **Show**), live
  (`user.message.kind`) and after a reload (`content.kind`), with no Edit.
  Notice entries render as muted lines under the assistant message they
  follow; they never open a user row, so the recovery code's user-turn counts
  are unchanged.
- **Exports** label the input "Fleet (continued after approval)" and include
  notices in the full-detail scope. The promote-to-task and save-as-workflow
  transcripts leave the input out (it is not something the user asked for).
- **Terminal and ACP clients** see the new turn's frames if they are attached;
  the contract scenario `approval-resume` (below) proves they consume a
  `turn.started` with `input_kind` without change. Neither renders persisted
  history, so neither shows the input or notices specially.

### Observability

- A resume skipped over the hourly cap or a full queue also sends the
  conversation owner a browser push when Web Push is configured
  ([APPROVAL-PROGRESS.md](APPROVAL-PROGRESS.md)), so a person who walked away
  learns the task is waiting for them.
- Each decision that claimed cards logs an `audit:` line naming the
  conversation, the queue row and every card with its outcome, or why it was
  skipped, and counts `fleet_approval_resumes_total{outcome="queued"|"rate_limited"|"queue_full"}`.
- The queue row (`chat_input_queue`, mode `resume`) is the durable record of
  each resume, kept for the queue's retention window; the cap counts these
  rows.
- The resume turn's own frames (`turn.started` with `input_kind`, from the
  agent's Observer stream, and `user.message` with `kind`) are persisted in
  the turn's event ledger like every frame.

## Deviations

- **The durable turn journal (#798) gets no new record kind.** Its two kinds
  (tool intent, tool result) are inputs to stranded-turn recovery; a third
  kind would have to be taught to that recovery. The observer stream, the
  ledger and the queue row record each resume instead.
- **The cap and the note are per conversation, not per user or per
  deployment**, and a skipped resume is not retried later in the hour: the
  conversation gets a note and the user continues by hand.
- **"Pending" excludes preview cards.** A `preview_email` card is
  display-only and never expires, so waiting for it could block a resume
  forever.
- **The approve POST is the web's trigger.** Nothing pushes a server-started
  turn to an open tab; the web follows after the POST (or after a reload with
  an executing card) and otherwise relies on tab return and reload. So a turn
  started because a card **timed out** (the expiry sweep, no POST) appears in
  an open, idle tab only on tab return or reload; the user guide says so.
- **The resume input counts as a user message** for the `suggest_advanced_model`
  cooldown (`CountUserMessagesAfterTimestamp`), which counts user-role text
  rows. It is a user-role row by design: the model reads it as the turn's
  input, and the edit flow's "latest user message" must be it.

## Deferred

- **Surviving a restart.** Dropping with a note was chosen over re-running at
  boot. A resume that survives would need a per-deployment decision on
  unattended spend after a restart.
- **A live push of the turn to an open tab** (a conversation-level event
  channel) instead of following after the click.
- **Hot reload.** Like every `agent_policy` value the list is installed at
  boot; a card is armed by the policy in force when it was staged, and a card
  armed under an earlier policy still settles into a decision (and a resume)
  after a restart that removed its tool.
- **Re-checking outstanding cards at the queue head.** The "nothing pending"
  check is taken when the resume is claimed. If a turn that is running at that
  moment stages a new card before it ends, the queued resume still starts when
  that turn ends. Its model sees the new card's APPROVAL_REQUIRED placeholder,
  and that card's own settlement starts another resume if its tool opted in.
  Closing the window would need the queue row linked to the cards it claimed,
  so it can be un-claimed at launch. Follow-up.
- **Scheduled runs.** `fleet run` stages no cards, so there is nothing to
  resume there.
- **Bundles adopt the key after this release ships**: the manifest decoder is
  strict, so an older fleet refuses a bundle that carries it.
