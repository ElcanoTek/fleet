# Changelog

- Align the recommended model lineup across Fleet, Chat, and MOC: new work
  defaults to `z-ai/glm-5.2`, while advanced escalation and task fallback use
  OpenRouter's exact `openai/gpt-5.6-sol` slug.

All notable changes to fleet are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The current release number lives in the top-level [`VERSION`](VERSION) file — the
single source of truth that the build stamps into both binaries (run
`./fleet version` or `./fleet-admin version` to read it back). fleet has not cut
a tagged release yet, so the history below starts at the Unreleased section; no
prior versions are listed because none have shipped.

## [Unreleased]

### Added

- **An A2A (Agent2Agent) protocol server (#1279).** External agents can now
  discover fleet via an Agent Card (`/.well-known/agent-card.json`), delegate
  work, stream progress, and collect results over the A2A v1.0.1 JSON-RPC +
  SSE binding (`POST /v1/a2a`: `SendMessage`, `SendStreamingMessage`,
  `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`). Every delegated
  message runs as an ordinary governed task through the same create pipeline
  as `POST /tasks` — operator-pinned persona/model (`FLEET_A2A_PERSONA`,
  `FLEET_A2A_MODEL`), creator-scoped reads (an invisible task answers
  TaskNotFound, never 403), budget/priority/rate gates intact. Off by default
  (`FLEET_A2A_ENABLED`); routes answer 501 until enabled. Authentication is
  the existing typed API keys — no new credential type. Streaming polls the
  task row (source of truth), opens with the Task snapshot, and closes at the
  terminal state per spec. Push notifications, the extended agent card, and
  the gRPC/HTTP+JSON bindings are deferred and declared off. See
  [docs/A2A.md](docs/A2A.md) and ADR-0051.

- **Prime Agent borrowings (#990).** A comparison of fleet against
  Prime Intellect's `prime-agent` harness, with the four ideas that cleared
  the high-value bar ported behind fleet's existing governance
  ([docs/PRIME-AGENT-COMPARISON.md](docs/PRIME-AGENT-COMPARISON.md)):
  the interactive compaction summarizer now produces a **structured**
  summary (Goal / Constraints / Progress / Key Decisions / Next Steps /
  Critical Context) and switches to an **update-the-previous-summary**
  prompt on repeat compactions so early facts stop eroding; both compaction
  paths **re-announce the task-tracker plan** (host-side state the finish
  gate keeps enforcing) right after the summary when open items remain; a
  new `FLEET_BUDGET_WINDDOWN_FRACTION` knob (default `0.8`, clamped to
  `(0,1]`, registered in the env-knob registry) injects a request-local
  **budget wind-down** notice into every provider call once spend crosses
  the soft threshold — wrap up cleanly instead of running silently into the
  hard ceiling — with a one-shot `fleet.budget_winddown` event; and the
  scheduled self-audit nudge now carries the **completion-audit** wording
  (intent, partial progress, and a plausible final answer are not proof of
  completion). Deliberate non-borrowings (the single-`ipython` RLM design,
  self-writing harness state, agent-to-agent messaging, kernel snapshots)
  are recorded in the comparison doc with reasons.

### Changed

- **Operations tasks now honor connector defaults (#1333).** New task forms
  start bundled connectors marked `enabled_by_default` in the selected state,
  matching new Chat conversations. The selection follows asynchronously loaded
  catalogs without overwriting a user's first toggle, is persisted explicitly
  with the task, and editing an existing task continues to show its saved
  connector list rather than applying newer deployment defaults retroactively.

- **Fonts: exactly two typefaces, self-hosted.** The web UI now ships
  **Nebula Sans** (SIL OFL 1.1) for UI/body/headings and **Hack** (MIT, plus
  Bitstream Vera for its Vera-derived glyphs) for code, logs and tabular
  output — the two faces Elcano standardised on across every product. The
  self-hosted IBM Plex Sans / IBM Plex Mono `.woff2` files and the
  `next/font/local` wrapper around them are gone; the four faces are replaced
  by one vendored sheet, `web/src/app/fonts/fonts.css` (a copy of flag's
  `design-system/fonts/fonts.css`), which `globals.css` imports and which is
  now the only place in the repo a font family is named. `--font-heading` /
  `--font-body` / `--font-code` / `--font-code-ui` read `--font-brand` /
  `--font-code-brand` / `--font-code-ui-brand` from that sheet, so no
  component or token changed. Still self-host-only: no Google Fonts, no CDN,
  no runtime font request off-box, and `font-src 'self' data:` in the proxy
  CSP is unchanged. Both licence files travel with the binaries
  (`fonts/nebula-sans/OFL.txt`, `fonts/hack/LICENSE.md`) — an OFL/MIT
  requirement, and what keeps this MIT repo's distribution compliant. Hack's
  advance width is 0.6021 em against IBM Plex Mono's 0.600, so the two `ch`-sized
  monospace rules (the 2ch diff gutter, the streaming caret) shift by ~0.3% and
  dense log/diff/table alignment is unchanged. One deliberate trade-off:
  dropping `next/font/local` also drops Next's per-face `<link rel=preload>`;
  `font-display: swap` covers the gap and no hand-written preload was added,
  because it would have to name a bundler-hashed path and would rot silently.
  One thing the swap did NOT get for free: **Nebula Sans ships proportional
  figures** (digit advances 407–625 per 1000 em units — `1` a third narrower
  than `8`) where IBM Plex Sans had every digit at 600, so columns of numbers
  in the sans face aligned for free before and no longer do. `globals.css` now
  makes tabular figures the `@layer base` default for every `table` — the
  orchestrator's task/adoption/SLA/usage grids, the admin user table, and
  assistant-authored markdown tables — so a numeric column added later cannot
  land ragged by omission, plus an explicit `font-variant-numeric: tabular-nums`
  on the numeric readouts that sit outside a table (KPI/stat tile values, count
  chips, the per-task time and SLA readouts). Measured in a real browser rather
  than eyeballed: an 8-digit cell whose text is all `1`s versus all `8`s
  differed by **25.09px** before the rule and **0.11px** after, and the widest
  spread across the 18 measured table cells and stat tiles is 0.20px.

- **Env-knob strictness now covers the knobs parsed outside the config
  loader (#1273).** #1119 made every knob `config.Load` reads fail loud on a
  malformed value; a couple of dozen knobs parsed at their point of use
  elsewhere in the binary kept silently (or warn-)defaulting, and `fleet
  validate-config` could not preflight them at all. Those knobs are now rows
  in the same `envKnobs` registry, in a third class (`scopeExternal`): the
  loader does not consume their values but it **validates** them, so a
  malformed value refuses to boot in the same one-pass error as every loader
  knob — now naming the package that reads it — and `fleet validate-config`
  reports it before you start the service. The knobs folded in: the three
  `FLEET_SCHED_RATE_LIMIT_*` windows, `FLEET_BACKUP_RETENTION_DAYS`,
  `FLEET_SANDBOX_KATA_OVERHEAD_MB`, `FLEET_TOOL_DISCLOSURE_THRESHOLD`,
  `FLEET_MODEL_CACHE_TTL_MINUTES`, `FLEET_RETRY_MAX_ATTEMPTS`,
  `FLEET_MAX_TOOL_OUTPUT_BYTES`, `FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD`,
  `FLEET_CONTEXT_COMPACTION_THRESHOLD`, `FLEET_DISABLE_PROMPT_CACHE`,
  `FLEET_DISABLE_OPENROUTER_MODELS`, `FLEET_SCHEDULED_AUTO_COMPACT`,
  `FLEET_TASK_WALL_TIMEOUT`, `FLEET_NOTIFY_TIMEOUT`, `FLEET_NOTIFY_RETRIES`,
  `FLEET_SSE_BUFFER_DURATION`, `FLEET_SSE_BUFFER_MAX_BYTES_PER_TURN`,
  `FLEET_SSE_HEARTBEAT_INTERVAL`, `FLEET_MAINTENANCE_MIN_INTERVAL`,
  `FLEET_WEBHOOK_RATE_LIMIT_PER_MINUTE`, `FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES`,
  `FLEET_PUSH_ON_TASK_COMPLETE` and `FLEET_PUSH_ON_APPROVAL_REQUEST`.

  **This is a behavior change for an operator running with a malformed
  value.** A value that used to be silently discarded — including one that is
  numerically valid but outside what its consumer accepts (`…KATA_OVERHEAD_MB=0`,
  `…BACKUP_RETENTION_DAYS=0`, a negative rate-limit window) — now stops the
  service at boot instead of running on the default. Five kill-switches
  (`FLEET_DISABLE_PROMPT_CACHE`, `FLEET_DISABLE_OPENROUTER_MODELS`,
  `FLEET_SCHEDULED_AUTO_COMPACT`, `FLEET_PUSH_ON_TASK_COMPLETE`,
  `FLEET_PUSH_ON_APPROVAL_REQUEST`) are read with Go's `strconv.ParseBool`, so
  the registry holds them to *its* narrower token set: `…=on` / `…=yes` is now
  refused with a message saying so, where before it silently resolved to
  `false` — i.e. `FLEET_DISABLE_PROMPT_CACHE=on` left caching ON and
  `FLEET_PUSH_ON_TASK_COMPLETE=off` kept notifying. Run
  `fleet validate-config` before upgrading to see anything that will now
  refuse.

  `FLEET_OTEL_SAMPLE_RATIO` is registered **documented-lenient** with its
  rationale in the table: a bad tracing ratio still means "sample everything"
  rather than stopping the service, and `validate-config` reports it as a
  **warning** instead of a blocking failure. Two knobs left the ad-hoc path
  entirely and now resolve through the registry's own parser at their point of
  use: the scheduler rate-limit trio in `fleet serve`, and
  `FLEET_BACKUP_RETENTION_DAYS` in `fleet backup` — a verb that never calls
  `config.Load`, so a malformed retention there fails the `--prune` step
  (exit 1) rather than pruning against the 30-day default.

  Eleven of these knobs were also missing from the `.env` allowlist, so a value
  set only in `FLEET_ENV_FILE` was silently dropped — including
  `FLEET_DISABLE_PROMPT_CACHE`, which `docs/OPERATORS.md` has documented as an
  env-file knob all along (the #1107 bug class). They are allowlisted now, and a
  new test keeps the registry and the allowlist paired. A repo-wide source sweep
  (`internal/config/knobs_sweep_test.go`) fails the suite if a new ad-hoc
  `os.Getenv`+parse knob is introduced without a registry row.

### Fixed

- **Kubernetes: a control plane that crashed and restarted IN PLACE still
  leaked every sandbox pod it had running (#1264).** The boot-time orphan
  sweep names an incarnation by `FLEET_POD_UID` — the downward-API pod UID —
  and kubelet restarts a crashed container inside the SAME pod, so the UID it
  reads after a SIGKILL, an OOM kill, a panic, or a failed liveness probe (the
  chart ships one) is the value its predecessor stamped on the pods it left
  running. The sweep read its own label back, concluded the pods were its own,
  and skipped them; there are no ownerReferences and no periodic sweep, so
  they stayed. Those pods are Guaranteed QoS, and on the validation cluster
  three stranded pods held 6 CPU of reservation that nothing would reclaim —
  enough that the warm pool's own refill could not schedule
  (`Insufficient cpu`), and every further restart stranded another set. The
  instance label now carries a **per-process boot nonce** alongside the UID,
  so it changes on a container restart as well as on pod replacement. The
  release-owner label still gates ownership, so a co-tenant release's live
  sandboxes remain untouchable; out of cluster the pid form is unchanged.
  (Pod *replacement* — a rolling update or a deleted pod — was already swept
  correctly, which is why this survived the #1257 fix.)

- **A lost lease is now identifiable on all three lease-guarded task writers,
  not one of three.** `UpdateTaskStatusAtomicWithContext` returned the
  `storage.ErrTaskLeaseNotHeld` sentinel, but `RequeueTaskForRetryWithContext`
  and `DeadLetterTaskWithContext` each rebuilt that sentinel's *exact* message
  with `fmt.Errorf` instead of returning it — so `errors.Is(err,
  ErrTaskLeaseNotHeld)` succeeded on one guard and silently failed on the other
  two while all three errors printed identically. That is a live trap rather
  than a cosmetic nit: the runner branches on this identity to cancel a zombie
  run's external side effects when a renewal proves the lease is gone
  (`renewActiveLeases`, #1116) and to suppress success side effects on a fenced
  commit, so extending that treatment to the retry/dead-letter paths would have
  read a false negative off an error whose text said exactly what happened.
  Both now return the sentinel, so the operator-facing message is byte-identical
  and only the error's identity changed. No behavior change today — neither
  runner call site does an identity check on these two paths yet, and no HTTP,
  CLI, chat or web surface consumes the lease error at all (it is an internal
  runner↔storage contract). Found while working #1268/#1269 (PR #1310) and
  fixed directly rather than filed.

- **Kubernetes: `FLEET_SANDBOX_PIDS` was accepted and then ignored (#1264).**
  The backend refuses podman-only knobs precisely so a setting cannot read as
  containment while imposing none — `FLEET_SANDBOX_RUNTIME`,
  `FLEET_SANDBOX_SECCOMP_PROFILE` and `FLEET_DEFAULT_NETWORK_MODE=allowlisted`
  each abort boot with the replacement named. The pids ceiling was missing from
  that list: it was accepted in silence, `fleet validate-config` reported the
  backend healthy, and the sandbox ran with `ulimit -u` unlimited, because a
  Pod spec has no per-pod pids limit (the cap lives on the kubelet, as
  `podPidsLimit`). Podman applies `--pids-limit` on every container, so one
  knob meant "bounded" on one backend and "unbounded" on the other. It now
  fails closed at boot and `validate-config` reports it too, so the problem
  surfaces before the upgrade rather than after. The per-task
  `fleet sched task set-limits --pids N` stays accepted — the value is portable
  and the same task may run on a podman deployment — but now logs one line per
  process saying it is ignored here, while the memory and CPU limits from that
  same command continue to apply. **Set `podPidsLimit` on your sandbox nodes
  and unset `FLEET_SANDBOX_PIDS` before upgrading**; `fleet validate-config`
  names it.

- **A re-imported task envelope can no longer resurrect a finished task or
  null a live lease (#1267), and API-key provenance is now immutable after
  creation (#1270).** `db.AddTask` is an unconditional full-column upsert
  whose `ON CONFLICT (id)` clause carries `status`, `lease_owner` and
  `lease_expires_at`, and two operator import paths reached it against
  EXISTING rows with no status validation — so re-importing an envelope of a
  task that had since run to `success` rewrote `success`→`scheduled` with the
  stale `scheduled_for` (the due sweep re-queued it and its emails/MCP writes
  ran again — the #1104 double-execution shape through a supported flow), and
  an import over a `running` row overwrote its lease mid-run. The upsert stays
  verbatim (that is what makes same-generation re-import idempotent); the
  policy now lives at the import seam: transient statuses (`leased`,
  `running`, both paused) are rejected at envelope validation by the SAME
  predicate that already gated legacy-bundle births, a status collision on an
  existing row is refused unless the operator passes the new `fleet sched task
  import --replace-status` (the legacy importer's pre-existing `--overwrite`
  is that path's opt-in), and NEITHER flag can touch a lease: a write over a
  `running`/`leased` row is refused outright and the lease columns are never
  importable onto an existing row. Separately, `created_by_key_id` — in the
  upsert set but never in `UpdateTaskTx`, with only "historical asymmetry" as
  its recorded reason — is now excluded from the upsert too: provenance is
  stamped by the insert that creates the row and by nothing after it, so no
  generic write path can re-attribute or clear the attribution the
  own-rows authorization checks read. See docs/OPERATORS.md ("sched task
  export · import"), docs/LEGACY-IMPORT.md, and the narrowed out-of-model
  warning on `models.TaskLifecycle`.

- **Kubernetes: a sandbox the cluster killed reported no cause (#1264).** A
  kubelet eviction (ephemeral-storage, or an `emptyDir` `sizeLimit`) and an OOM
  kill both end a turn the same way — the exec stream dies and the caller gets
  `bridge closed unexpectedly: EOF`, or a bash exec error naming nothing — while
  the actual verdict sits in the pod's own status, one GET away on a client the
  sandbox already holds. Unsaid, it gets guessed: during the #1264 validation an
  `emptyDir` eviction was explained to the user as an OOM kill, confidently and
  wrongly. Both error paths now append the cluster's own answer — `the sandbox
  pod failed: Evicted — Usage of EmptyDir volume "tmp" exceeds the limit
  "128Mi"`, `the sandbox container OOMKilled, exit code 137`, or `the sandbox
  pod is gone` when it has been deleted outright. The lookup runs only on an
  error path, on its own short-lived context, and contributes nothing when the
  apiserver cannot answer it says exactly that — `the cluster could not be asked
  why — the apiserver did not answer` — because the condition most likely to
  kill a sandbox is cluster trouble, and a silent diagnostic there just returns
  the reader to guessing.

- **A round-capped scheduled run keeps the transcript it paid for (#1271).**
  When a scheduled run exhausts the 20 enforcement rounds without its finish
  gates ever clearing, `agentcore.Run` returns the accumulated transcript and
  usage alongside the error (#1125) — but the scheduled driver's
  `if err != nil { return err }` ran before its persistence block, so up to 20
  rounds of paid assistant text never reached the session log. The driver now
  recognizes that failure through a new `agentcore.ErrMaxEnforcementRounds`
  sentinel (not the error text) and writes the carried partial text into the
  log with a `round_cap_truncated` `message_type`, preceded by a notice naming
  the rounds burned and the spend. The run still **fails** exactly as before:
  the same error message, the same `terminal` failure class, the same retry and
  notification behaviour — only transcript visibility changed.

- **Kubernetes: the riskier egress posture was the quieter one (#1264).** Boot
  printed a full paragraph under `lockdown` — every pod labeled
  `fleet.elcanotek.com/egress=none` for the deny-all NetworkPolicy — and
  **nothing at all** under `open`, which is the default mode. So the
  configuration where model-authored code can reach the fleet Service, the
  in-cluster database, the apiserver and the node's metadata endpoint was the
  one that gave the operator no signal, while the sealed one explained itself.
  `open` now logs a WARNING naming what is reachable and the single chart knob
  that closes it (`networkPolicies.openEgress.create=true` with
  `blockedCIDRs`). Neither line claims enforcement: under lockdown that is the
  CNI's job and fleet verifies only that the policy object exists, and under
  open the protecting policy may carry any name from any tooling, so fleet
  states the posture it creates pods with rather than inventing a verdict.

- **An Optional server's variant seats are now opt-in gated at registration,
  not just hidden from the prompt (#1272).** The two layers that decide whether
  an Optional MCP server's tools are available keyed their checks differently:
  agentcore's Gate-1 did an **exact** map lookup on the registered server name,
  while the system-prompt roster prefix-matched `mcp_<server>_<tool>` names and
  resolved the longest matching Optional server. For a named-account seat
  `jira_prod` whose bundle declares only `jira` as `optional: true`, Gate-1's
  exact lookup missed — the seat registered and was **callable on every run**
  while the prompt hid it from the model. Both layers now resolve through one
  helper (`agentcore.longestServerKey`, exported as `OptionalServerFor` /
  `OptionalServerForToolName`) implementing a single documented rule — exact key
  wins, else the longest key the name extends across an underscore — so Gate-1
  fails closed on a variant seat and what the model sees always matches what
  registers. Gate-2's per-server tool allowlist resolves through the same
  helper (its behaviour was already this rule). The rule is written up in
  docs/AGENT-RUNTIME.md; the roster's byte-stability guard
  (docs/PROMPT-CACHE-CONTRACT.md) is unchanged and still green.

- **Kubernetes: `open` egress mode now requires a policy to boot (#1264).**
  **Upgrade note: a deployment running `FLEET_DEFAULT_NETWORK_MODE=open`
  without the open-egress NetworkPolicy will refuse to start.** The docs called
  that policy *required, not optional*; the chart shipped it off by default and
  the preflight never asked for it, so the out-of-the-box combination was the
  unrestricted one. Measured on a stock k3s install, a sandbox in exactly that
  configuration reached the fleet Service (`GET /healthz` → `ok`), the
  in-cluster Postgres, the apiserver and the public internet — none of which is
  possible under podman, where an open sandbox is rootless pasta/slirp4netns:
  outbound-only, structurally unable to reach the fleet process. The boot
  preflight now requires the `open` policy the same way it already required the
  deny-all one, naming all three ways forward: enable it
  (`networkPolicies.openEgress.create=true`), switch to `lockdown`, or set
  `FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED=true` if the cluster shapes that
  egress with policy fleet cannot see — which is logged as a WARNING every boot
  rather than agreed to once. `fleet validate-config` reports it before the
  upgrade. Two chart changes ride along: the policy's `blockedCIDRs` default now
  covers the private ranges as well as the metadata range, because that is where
  the fleet Service, the database and the apiserver live on nearly every cluster;
  and `networkPolicies.openEgress.name` — a key the values schema already
  advertised while the template ignored it, deriving the name from the release —
  is now the name actually used. ADR-0049's sealing decision amended.

- **The runtime-secret literal set is now bounded, and the main process's
  control-plane acquisitions feed it too (#1274).** Both follow-ups deferred
  from #1124. (1) Every OAuth rotation mints a distinct access+refresh pair,
  and `redact.Redactor` retained all of them for the process lifetime — with
  hourly-expiry tokens that is ~50-70 dead secrets per server per day, each
  costing a `strings.ReplaceAll` pass on every masked-error `Redact` and
  keeping expired credentials in memory forever. Literals are now either
  PERMANENT (boot-time env secrets, static api_keys — unchanged) or SCOPED to
  one hosted-MCP server row, where each successful rotation opens a new
  generation and the row's previous generation retires after a **15-minute
  grace window** (`literalRetireGrace`), with a hard cap of 4 retained
  generations per row as a refresh-storm backstop. Retirement can only ever
  drop a value the SAME row has superseded: a re-listed secret is revived, a
  permanent literal is never demoted, and nothing else's literals are touched.
  Steady state per connection is 3 literals (client secret + live access +
  live refresh) instead of unbounded growth. (2) The main process's
  control-plane acquisitions — the OAuth callback's code exchange, the
  authorize step's unsealed client secret, dynamic client registration, and
  the add-time / rotate-time api_key probes — now register their credentials
  with the process-wide scrubber before the request that could echo them, and
  the remote-MCP HTTP error path (which relays wrapped VENDOR failure text)
  runs through that scrubber instead of shape patterns alone.
- **Kubernetes: a `nodeSelector` value that YAML typed as a bool or a number
  broke every turn (#1264).** The chart rendered the sandbox node selector with
  `printf "%s=%s"`, and Go's `%s` on a non-string prints its error form — so
  `nodeSelector: {fleet-runner: true}`, which YAML 1.1 makes a **bool**, became
  the literal string `fleet-runner=%!s(bool=true)` (a bare number gave
  `%!s(int64=12)`). `helm install` accepted it, boot and the cluster preflight
  passed, and then every single turn failed at pod creation with an apiserver
  422 quoting the mangled value back. It renders with `%v` now, so a bool or a
  number becomes the string the operator meant — Kubernetes label values are
  strings regardless — and both CI lanes assert that no chart value renders as
  a Go format error, which `kubeconform` structurally cannot catch: the bad
  text is a valid string in the rendered Deployment, and only the sandbox pod
  created at runtime is rejected.

- **Task transition guards are now one shared set, and cancelling a
  dead-lettered task no longer erases its replayability (#1268, #1269).** The
  four storage transition writers each hand-listed their own terminal refusal
  set and they disagreed: `DeadLetterTaskWithContext` refused all four terminal
  statuses while `CancelTaskAtomic`, `UpdateTaskStatusAtomicWithContext` and
  `RequeueTaskForRetryWithContext` refused only three — the latter two shielded
  from `dead_lettered` purely by the unenforced fact that a quarantined row
  holds no lease. All four now guard on `TaskStatus.IsTerminal()`, the one
  shared set `models.TerminalTaskStatuses` is cross-checked against at package
  init, so a new terminal status cannot be honored by one writer and forgotten
  by the next. Only the refusal *style* still differs, deliberately: cancel is
  an operator request and errors, the three lease-guarded runner writers return
  the row unchanged so a late idempotent report cannot fail a run that already
  landed. Two consequences: **cancel now refuses a `dead_lettered` task**
  (#1268) — the edge used to be live, so an operator sweeping old rows out of
  the UI moved a quarantined occurrence to `cancelled`, from which no replay
  path exists, silently destroying the review-and-replay the DLQ exists for;
  the error names both real options (`fleet sched dlq replay <id>`, or delete
  the row) and the UI already never offered Stop on a DLQ row, so nothing in
  the product regressed. And **the worker-report to-side is now enforced**
  (#1269): `models.TaskStatus.IsValidReportedStatus` existed for exactly this
  with zero production callers, so `UpdateTaskStatusAtomicWithContext` wrote
  whatever status it was handed — a caller passing `cancelled` from a leased
  row produced a cancelled task still holding a live lease, and no lifecycle
  test failed. The dead seam is now the guard, checked before the transaction
  opens. No edge in the lifecycle table changed behavior except the removed
  `dead_lettered → cancelled` one; the storage writer matrix grew a derived
  off-table-target probe (every non-reportable status) so the to-side guard is
  pinned the way the from-side already was.

- **Kubernetes: a sandbox that was never scheduled said only "not ready"
  (#1264).** A pod stuck Pending has no container status to explain itself —
  nothing has been handed to a kubelet yet — so the start-timeout error carried
  no reason at all, while the scheduler had already written one onto the pod as
  `PodScheduled=False` with the familiar `0/N nodes are available: …`
  breakdown. On this backend that is the likeliest way a start fails, and it
  lands precisely on the dedicated-runner-pool story the chart sells: a
  `nodeSelector` or toleration matching no node, or a node-pinned
  PersistentVolume the sandbox cannot reach (an RWO workspace claim on a
  multi-node cluster does exactly this). The timeout now reads `sandbox pod X
  was never scheduled before the start timeout (Unschedulable: 0/3 nodes are
  available: 1 node(s) didn't match Pod's node affinity/selector, 2 node(s) had
  untolerated taint(s))`. Recorded rather than fatal, unlike a pull failure:
  unschedulable often clears inside the start window when a warm pod retires or
  a node returns, so the budget is still worth waiting out. The message is
  capped so a large cluster's per-node enumeration cannot turn one error into a
  wall.

- **Scheduled runs are now told the shared file library exists (#1301).**
  Since #1290/#1296 a scheduled run's workspace has carried the readable
  `shared/` tree, but the announcement block lived only on the chat path —
  so "attach historical data once, every run uses it" worked for scheduled
  work only when a task prompt happened to name a file. The block renderer
  moved to `sharedfiles.PromptBlock` (one renderer, both drivers — chat
  turns are byte-identical to before), and `fleet serve` wires the scheduled
  runner a provider that appends the same capped block to each run's system
  prompt, computed once at run start so the prompt stays byte-stable across
  the run's turns. One-shot `fleet task run` stays out of scope (no DB, no
  library) and docs/SHARED-FILES.md now says all of this plainly.

- **Kubernetes: a sandbox pod whose delete failed was never deleted again
  (#1264).** A close-time or cancel-time `DELETE` can fail for reasons that
  have nothing to do with the pod — an apiserver restart, a network blip, a
  throttled burst — and fleet logged `the pod may linger until the boot-time
  orphan prune` and moved on. That prune could never collect it: the sweep
  deliberately skips pods carrying the **running** incarnation's label, which
  is exactly what these are, and a Pod owned by a live Pod gets no help from
  the garbage collector either. So the pod kept its Guaranteed CPU and memory
  reservation for the lifetime of the control-plane process, and enough of them
  take a node to the point where no sandbox can schedule at all — the same end
  state as the orphan-sweep bug above, reached by a transient blip instead of a
  crash. Measured during the #1264 validation: an 83-second apiserver outage
  stranded two pods, still Running 90 seconds after the cluster had recovered.
  Failed deletes are now retried in the background with backoff for ~30 minutes
  and logged when they land; the queue is bounded, and the drain goroutine
  exits when it empties, so a deployment that never fails a delete never
  carries one. The cancel path (#796) gets the same treatment, so a sandbox
  whose containment could not be confirmed stops being uncontained as soon as
  the apiserver answers again.

- **Superseded CI runs no longer paint a red X: the gate rollups treat
  cancelled needed jobs as neutral (#1302).** Every rapid dev merge cancels
  the in-flight `dev-ci` run on the now-stale tip (the concurrency group),
  and `Dev gate` / `CodeQL gate` — which run even on a cancelled run via
  `if: always()` — then reported *failure* over jobs that concluded
  `cancelled`, leaving a misleading red X someone had to manually verify as
  benign after every merge train. All three rollups (`Dev gate`, `CodeQL
  gate`, and `CI gate` on main for consistency) now conclude neutral when
  needed jobs were cancelled and none failed; a genuinely failing job still
  turns the gate red, and a skip on a non-docs-only change still fails `CI
  gate`. The one accepted caveat is documented in ci.yml: a human manually
  cancelling a PR-head run and then merging over it is deliberate, not a
  case the gate exists to catch.

- **Kubernetes: an exec stream that ended by itself leaked its keepalive
  forever (#1264).** The session context was released only by `close()` or by
  the caller's own context dying — never by the stream simply ending. client-go's
  websocket executor runs a keepalive for as long as that context lives, so a
  stream killed from the far side (the sandbox pod evicted or deleted mid-exec)
  and then abandoned left a ping loop writing to a dead socket, leaking a
  goroutine and a socket apiece. Measured on the validation cluster: one such
  session logged **3,285 `Websocket Ping failed` lines over fifteen hours** and
  was still going when it was found — and, worse than the leak itself, the noise
  buried every other line in the log, which is how the next bug hides. The
  context is now released whoever ends the stream.

- **`FLEET_SANDBOX_WARM_SIZE` now rejects every explicit negative at the
  validation seam (#1299).** The knob registry row carries `min: 0`, so boot,
  hot-reload, and `fleet validate-config` all refuse a negative depth loudly —
  including an explicit `-1`, which previously slipped through the loader's
  ad-hoc `< -1` check as an undocumented spelling of "derive". Unset still
  derives from `FLEET_MAX_CONCURRENT_AGENTS`, and `0` still means "no warm
  pool" (#1288); the safety of a negative no longer rests on the pool's
  incidental cold-start fallback.

- **Under `FLEET_DEFAULT_NETWORK_MODE=lockdown` the warm pool is now sealed
  and lockdown turns actually use it (#1291).** Two defects, one cause: warm
  spawns used the pool config verbatim (`NoNetwork=false`), while every take
  under fleet-wide lockdown forced a sealed cold start. So the pool held N
  open-egress sandboxes nothing could ever claim — dead reserved CPU, memory
  and disk, respawned forever by the TTL keeper — and on the kubernetes
  backend the parked pods were labeled `fleet.elcanotek.com/egress=open`
  directly beside a boot log line claiming every pod is labeled `none`. Warm
  spawns on a lockdown pool are now sealed (`--network=none` / the
  `egress=none` label, making that log line true by construction), and a
  sealed, no-override take (`TakeContainer`, and scheduled runs without
  per-task limits) claims a warm sandbox instead of cold-starting — lockdown
  turns get the same warm-pool latency win open fleets always had. Everything
  else keeps its posture: per-task resource overrides still cold-start
  (fresh ceilings need a fresh container), a per-conversation lockdown chat
  on a NON-lockdown fleet still cold-starts sealed (that pool's warm
  inventory is open, and a sealed take must never receive an open sandbox),
  and allowlisted takes are untouched (the per-turn proxy token still
  requires a cold start).

- **Scheduled runs and `fleet task run` can use the file tools on their own
  workspace — and read bundle docs — on every backend (#1290).** Three
  stacked, backend-independent gaps made `view_file protocols/…` (and, in the
  one-shot harness, even `write_file notes.md`) fail for all scheduled work:
  the harness never registered the workspace root or supporting-doc dirs, so
  host validation fell back to the process-cwd allowlist (`/opt/fleet/client`
  in the split control-plane image) and rejected every workspace-relative
  path; `fileOpRoot`'s forced-working-dir branch lacked the read-only
  supporting-doc exception the conversation branch already had, so a doc read
  was refused even when it validated; and nothing seeded the
  `protocols`/`personas`/`system_prompts`/`skills` symlinks into scheduled or
  one-shot workspaces, so the system prompt's bare-path convention — which the
  audit enforcement itself relies on ("read protocols/self-audit.md") — silently
  broke. `fleet task run` now registers both globals with its minted workspace
  (chmod'd container-readable), `configureRunWorkspace` seeds the symlinks for
  every scheduled and one-shot run, and the forced-dir branch admits a
  host-resolved doc symlink with the same narrow shape as chat: unresolved
  path beneath the forced root, resolved target beneath a registered doc
  root, reads only. Writes into doc mounts, non-doc symlink escapes, absolute
  doc-mount paths, and `..` traversal out of the forced root stay refused,
  with regression tests pinning each.

- **The Kubernetes sandbox backend can now actually run tool calls: exec
  streaming rides client-go instead of a hand-rolled WebSocket client
  (#1264).** The first real cluster this backend ever met — the example
  bundle's own kind rehearsal — showed the hand-rolled `v4.channel.k8s.io`
  client losing exec stdin nondeterministically for payloads beyond a few KB.
  The 28KB bridge upload wedged until its two-minute deadline on roughly four
  of five attempts, every warm pod churned on that cycle forever, and not one
  `bash`/`run_python`/file tool call could execute; the identical uploads
  through client-go's `remotecommand` executor succeeded five of five on the
  same cluster, pod and payloads, as did kubectl at 7MB. Exec now uses that
  executor (protocol `v5.channel.k8s.io`, with a real stdin half-close on the
  wire).

  The adoption is deliberately narrow, and ADR-0049 is amended in place: its
  own recorded revisit trigger fired. client-go is a **transport, never a
  config loader** — pod CRUD and the boot preflight stay on the hand-rolled
  REST client, and kubeconfig handling stays fleet's strict parser (exec
  plugins and `insecure-skip-tls-verify` still refused; the `rest.Config`
  handed to client-go is built from the already-validated material). The
  in-package session API and the #1257 teardown choreography are unchanged,
  and the fake-apiserver suite now speaks `v5` — including the stream-close
  frame — because that is what real kubelets serve. Session teardown prefers a
  clean end over a cancellation: stdin is half-closed first so the bridge's
  read loop exits on EOF and the stream finishes by itself — cancelling an
  active stream made client-go's copy goroutines log spurious "use of closed
  network connection" errors on every sandbox retirement — with the cancel
  kept as the bounded backstop for a process that ignores EOF.

- **`FLEET_SANDBOX_WARM_SIZE=0` now means what it says: no warm pool**
  (#1264). Before, `0` was indistinguishable from unset, so the engine
  silently derived a 2..8-deep pool from `FLEET_MAX_CONCURRENT_AGENTS` — and
  the Helm chart reinforced the confusion by omitting the env var at
  `warmSize: 0` and documenting `0` as "derive". Found in the #1264 kind
  rehearsal, where the bundle's overlay set `warmSize: 0`, documented itself
  as running with no warm pool, and ran a two-pod Guaranteed-QoS pool anyway.
  Now: unset derives (unchanged default), an explicit `0` disables warming
  (every take pays a cold start — the pool always supported this), a positive
  value pins the depth, and a negative value fails loudly at load. The chart's
  `sandbox.warmSize` default is now `null` (= let the engine derive) and any
  set value — including `0` — is passed through. **Semantic change:** an
  operator who set `FLEET_SANDBOX_WARM_SIZE=0` (or chart `warmSize: 0`)
  expecting derivation now gets no warm pool — delete the key to keep the
  derived depth.

- **`fleet validate-config`'s model_api check now actually verifies the API
  key** (#1264). It probed OpenRouter's `/api/v1/models`, which is public —
  it returns 200 with no Authorization header and with a garbage one — so any
  non-empty `OPENROUTER_API_KEY` was blessed with "API key authenticates".
  The #1264 kind rehearsal hit the consequence: a mis-created secret holding
  64 hex characters of junk passed the check, then the first real completion
  failed with `401 Missing Authentication header`. The check now probes
  `GET /api/v1/key`, which requires auth (401 on a bad or missing key, 200
  with the key's own metadata otherwise). The fake-LLM seam serves
  `/api/v1/key` with the same auth contract, so the check stays meaningful —
  not a spurious 404 warning — in E2E ladders running against
  `OPENROUTER_BASE_URL`.

- **`/readyz`'s sandbox check is backend-aware** (#1264). Under
  `FLEET_SANDBOX_BACKEND=kubernetes` the probe ran `podman --version` on the
  control-plane host — a binary that deployment never has or uses — so
  readiness reported a permanent, misleading `degraded`. The kubernetes
  backend now probes what its sandboxes actually run on: one cached apiserver
  `GET /version` (the same call the boot preflight opens with), reporting the
  cluster version in the detail. The podman backend's `<runtime> --version`
  probe, its #217 binary-name mapping, and the #215 unauthenticated-endpoint
  cache bound are unchanged.

### Added

- **The node-LTS move reminds itself (#1300).** The node runtime major is
  invisible to Dependabot (see the notes in `.github/dependabot.yml`), so
  moving to a new LTS line was initiated by a dated issue someone had to
  remember. A weekly `node-lts-reminder.yml` cron now files that issue
  itself — once v26 is past its scheduled Active-LTS date (2026-10-28)
  while `web/.nvmrc` still reads 24 — carrying the full checklist: the
  LTS/schedule.json and Fedora `nodejs26`+`nodejs26-npm` RPM preconditions,
  then every declaration point (`web/.nvmrc`, both `engines.node` fields,
  `@types/node`, the rampart `node:26-slim` base) bumped together under
  `TestNodeMajorAgreesEverywhere`. Deliberately offline: the date is
  hardcoded from nodejs/Release so a fetch hiccup can never make the
  reminder lie; the filed issue dedupes by title.

- **The post-promotion ancestry merge is automated (#1298).** Promotions
  squash-merge dev into main, so the merge-base never advances and every
  re-touched region read as a conflict on the next promotion PR. The new
  `Promotion ancestry` workflow fires on every push to `main`, verifies
  `main^{tree}` is byte-identical to `dev^{tree}` (the precondition that
  makes `-s ours` provably safe), and pushes the `git merge -s ours`
  ancestry merge to dev — failing loudly when the trees differ (dev moved
  mid-promotion; a human should look). The manual fallback, with the same
  tree-identity precondition, is documented in CONTRIBUTING.md
  ("Promotions") instead of living as tribal knowledge.

- **Shared files: a native cross-chat file library.** Admins publish files
  once (Settings → Shared files, or `POST /shared-files`) and every
  conversation's agent can read them at `shared/<folder>/<name>` — on BOTH
  sandbox backends: canonical bytes stay host-side under
  `<DataDir>/shared_files/`, a staged copy under `<WorkspaceRoot>/shared/` is
  mounted read-only into every sandbox (a nested `:ro` bind on podman, a
  read-only subPath of the workspace claim on kubernetes), and a reconciler
  (boot + every mutation + the hourly maintenance pass) heals any drift. Each
  chat turn gets a capped "Shared file library" prompt block with paths,
  sizes, and descriptions. Members list/download; admins upload into one
  optional folder level, rename/move/describe, delete. The library total is
  capped by the new live `shared_files_max_total_mb` admin setting
  (`FLEET_SHARED_FILES_MAX_TOTAL_MB`, default 10 GiB, 0 = unlimited).
  Migration 053. Design note: `docs/SHARED-FILES.md`.

- **Multiple logins for hosted (official) MCP connections** (#988). A user can
  hold several seats under one connection name — a work and a personal GitHub,
  two Gamma workspaces — each with its own sealed token and its own share
  grants, and pick which one a chat or a scheduled task uses. Settings →
  Connections groups seats per name with "Set default", "Rename" and "Add
  another account"; the chat Tools picker gains a per-conversation seat
  override (bundled connectors too — `conversations.mcp_accounts`); the task
  modal pins a hosted seat via `mcp_selection {server, account}`. A run mounts
  exactly one seat per name, registered under the bundle seat formula
  (`mcp_<name>_<account>_*`), and a pinned seat that is not connected is
  skipped — never replaced by another account. Approval execution against a
  hosted connection reopens the exact seat the card recorded. Migrations 051
  (`remote_mcp_servers.account`/`is_default`, uniqueness per seat) and 052.
  Design note: `docs/REMOTE-MCP-MULTI-LOGIN.md`; ADR-0050.

### Fixed

- **Chat attachments now reach the agent under the kubernetes sandbox
  backend.** A sandbox pod mounts only the workspace claim, so the uploads
  root (control-plane state) was invisible: the attachments prompt block
  advertised absolute paths no pod could resolve, and every non-image
  attachment read failed. The chat server now copies validated non-image
  attachments into `<workspace>/<convID>/attachments/` — inside the claim —
  at send time under that backend and advertises the staged paths; the copies
  live and die with the conversation workspace. Podman keeps its zero-copy
  read-only uploads mount; image attachments were never affected (vision
  bytes are read host-side).

- **A self-audit abort now retires what it abandons, and a confirmed audit says
  what is still outstanding.** Field case: a daily refresh audited the inline
  Pages write, staged its payload by reference, was BLOCKED on the upload
  variant, aborted (nothing had run), re-audited the upload tool and published
  the page — yet finish enforcement kept demanding the abandoned inline
  declaration and forced a second abort, landing a live page as status
  `error`. `confirm_audit(success=false)` now zeroes every declared-but-
  unexecuted commitment (and reports which), so the later re-audit's execution
  is what the run is judged on. The success trailer also stopped saying
  `All 0 critical actions executed. Finish now.` when it had just registered
  a declaration; it now names the outstanding call(s) to make.

- **Scheduled runs could not stage files where their own sandbox could read
  them.** `download_url` resolved a relative `output_dir` against the process
  cwd instead of the run's forced working dir, then refused its own choice as
  "path escapes the scheduled-run worktree" — dozens of failed calls per daily
  refresh. Relative and omitted `output_dir` now anchor to the forced dir, and
  an absolute path outside it is refused with an error naming the worktree.
  Runs with a forced working dir also get a `## Working directory (this run)`
  message tail (the system-prompt section only reached interactive turns), so
  MCP file tools are told the absolute `output_dir` to write into — an email
  attachment saved relative to the MCP server's own directory had reported
  success while the sandbox never saw it, stalling a dashboard for three days.
- **`confirm_audit` turned finished work into `error` status.** A re-audit of
  the same unbound critical tool (a managed-data write retried after
  `stale_version`) stacked a second commitment instead of superseding the
  first, so the single successful retry left a phantom obligation, finish was
  refused, and the only exit was an abort that recorded a live, correct page
  as a failed run. Unbound same-tool re-audits now supersede like record-bound
  ones; an abort after every declared action has executed is refused with
  guidance to finish instead of flagging the run terminal; and an abort no
  longer demands the `critical_actions` unlock list it does not use (every
  field abort was first rejected on that check).
- **The npm override canary demanded an unsafe `adm-zip` override removal.** It
  checked `onnxruntime-node@latest`, which now accepts the patched dependency,
  while Fleet's locked Transformers release still pins `onnxruntime-node 1.24.3`
  and its vulnerable `adm-zip ^0.5.16` range. The canary now evaluates every
  parent version actually present in the lockfile and fails only when all of
  them accept the patched line.
- **The web typecheck rejected the Chat API route.** It re-exported the shared
  `MODELS_PAGE_URL` constant even though Next.js route modules may only expose
  recognized handlers and route configuration. Consumers already import the
  constant from `lib/openrouterModels`, so the invalid route export is gone.
- **Kubernetes: the sandbox backend could not execute a single tool call on a
  cluster that enforces RBAC.** fleet streams exec over a WebSocket upgrade —
  an HTTP GET — so the apiserver authorizes `get pods/exec`, but both the chart
  Role and the boot preflight only ever asked for `create`. The preflight
  passed, readiness went green, and the first `bash`/`run_python`/file-tool
  call 403'd, leaving a churn of created-then-deleted sandbox pods and an
  operator debugging the CNI. The Role now grants both verbs and the preflight
  checks both, so a short Role fails at boot where it is visible.
- **Kubernetes: a crashed control plane leaked its sandbox pods.** The orphan
  sweep reused podman's "is the owner pid still alive?" test, which is
  meaningless across containers: the labelled pid came from a namespace that no
  longer exists, fleet is pid 1 in its own container, and the 120s start
  tolerance meant a fast restart concluded the previous owner was still running
  and skipped every pod. Those pods are Guaranteed QoS, so each held its full
  CPU/memory reservation with nothing left to reclaim it. Ownership is now
  identity, not liveness: the pod UID names the incarnation and the release
  name marks the owner, both via the downward API. The same change stops a
  second fleet release sharing the namespace from deleting a live neighbour's
  sandboxes. Out of cluster, where there is no such identity, the pid heuristic
  remains.
- **Kubernetes: `Sandbox.Close()` could block forever and never delete the
  pod.** The bridge's stdout crosses an `io.Pipe` whose writer is the exec
  demux loop and whose only reader stops at the first newline; anything the pod
  wrote past that line parked the loop in `pw.Write`. Teardown then joined on a
  channel that only the parked loop could close — under the sandbox mutex, so
  the pod delete was never reached and `Pool.Close` hung behind it. Teardown
  now closes the read half first, and the join is bounded.
- **Kubernetes: the web tier was unreachable and wired to env names the app
  does not read.** `npm start` binds `${FLEET_WEB_HOST:-127.0.0.1}`, so the
  Service could never reach the pod; the chart set `ORCHESTRATOR_URL` where the
  app reads `ORCHESTRATOR_SERVER_URL` (silently falling back to a loopback
  address, killing the whole Orchestrator tab); and `web.port` never reached
  Next, so any non-3000 value 502'd.
- **Kubernetes: boot failed before the sandbox preflight on a stock image.**
  `EMAIL_ATTACHMENT_DIR` defaults CWD-relative and nothing derives it from
  `FLEET_DATA_DIR`, so with no `WORKDIR` the control plane died trying to
  create `/data/attachments/uploads`. The chart now sets it from `dataDir`.
- **`fleet validate-config` reported OK on a config that refuses to boot.** It
  checked the runtime and network-mode knobs the kubernetes backend rejects but
  not `FLEET_SANDBOX_SECCOMP_PROFILE`. That variable was also missing from the
  `FLEET_ENV_FILE` allowlist — its kubernetes counterpart was already there —
  so a value set only in an env file was dropped.
- **`fleet cleanup` was recommended as a Kubernetes maintenance CronJob.** It
  prunes podman image layers and Go build caches; a control-plane pod has
  neither. Corrected in `DEPLOYMENT-KUBERNETES.md` and `TIMERS.md`.

### Added

- **One unified Admin permission in user management.** The Users popover now
  keeps ordinary Chat and Ops Center roles separate while presenting Admin once
  in its own section. Selecting it grants both the chat-admin and scheduler-admin
  roles, matching the server's existing two-plane admin semantics without two
  misleading Admin choices or duplicate badges. The Add user form now presents
  the same Admin, Chat, Ops Center, and Team fields, allowing the complete role
  assignment at account creation. Every role choice includes an immediate hover
  and keyboard-focus description of the access it grants.
- **A `values.schema.json` for the chart.** Helm renders nothing for a key the
  templates do not read, so a misspelled value passed `helm lint`, passed
  `helm template`, and surfaced only as missing behaviour in production —
  `sandbox.kubernetes.bundleDocsInImage` being the sharpest case, where the
  symptom is `view_file` refusals on a live cluster. Objects with a known key
  set are now closed; free-form maps stay open.
- **The Helm CI job schema-validates instead of rendering to `/dev/null`.**
  Both lanes now run `helm lint --strict` and pipe every render through
  `kubeconform -strict`, so a manifest the apiserver would reject fails the
  gate. The previous step proved only that the template executed.
- **A `startupProbe` and a `terminationGracePeriodSeconds` on the control
  plane.** Boot order is migrations → sandbox preflight → listener, so liveness
  used to start counting during migrations and kill a slow first boot at ~t+120s,
  forever. And with no pod grace period the kubelet SIGKILLed at its own 30s
  default — the same value as `FLEET_SHUTDOWN_GRACE_SECONDS`, so the documented
  drain had no slack and raising the fleet knob alone did nothing.
- **Honest-scope entries for the parity gaps that were not written down**: an
  open sandbox pod is a full citizen of the cluster network (including the
  node's metadata endpoint, now in the chart's default `blockedCIDRs`);
  `RuntimeDefault` is a weaker syscall filter than fleet's bundled profile;
  there is no PID-1 reaper; the workspace has no per-file cap where podman's
  `ulimit fsize` reaches its bind mount; a sealed sandbox's network calls hang
  rather than failing fast; the NetworkPolicy is verified by name only; the pod
  start budget is a fixed, unconfigurable 2 minutes; and a bundle's Python MCP
  servers run in the control-plane pod, which therefore needs their runtime.

### Added

- **Task-lifecycle transition table (#1127)** — the task state machine now
  exists as a tested constant: `internal/sched/models/task_lifecycle.go`
  enumerates every guarded status edge (from → to, with its one authoritative
  writer; the two verbatim-upsert import paths that bypass the guards are
  documented on the table as out-of-model restore surgery)
  plus the derived status sets (terminal, active, claimable, paused,
  cleanup-eligible, recurrence-spawning, worker-reportable, retired), with the
  full lifecycle narrative as its doc comment. Deliberately NOT a runtime
  framework: every claim/recovery/pause/wake/settle query keeps its own
  guarded SQL; the only runtime derivation is `claim.go`'s already-named
  `taskActiveStatuses`, which now reads `models.ActiveTaskStatuses` (same
  `{leased, running}` it always was). Drift fails loudly instead of silently:
  behavioral per-writer matrices in `sched/db` and `sched/storage` seed a row
  in every status, drive each db/storage transition writer, and assert the
  outcomes match the table exactly; an SQL-literal source scan (the #1126 drift-test
  treatment) rejects any tasks-table status literal the table doesn't know
  (including the retired `analyzing`, rewritten away by migration 063); and
  init-time validation makes a status added without lifecycle rows unreachable
  and therefore a boot/test failure. Zero behavior change — surprising
  existing edges (e.g. cancel's terminal-refusal list omits `dead_lettered`,
  so a DLQ row can be cancelled) are encoded and flagged as current reality,
  not fixed.

### Changed

- **The web tier no longer receives the control plane's secret.**
  `config.existingSecret` carries `OPENROUTER_API_KEY` and both database DSNs
  and was mounted wholesale into the Next.js pod, which reads none of them and
  is the one component behind the Ingress. The web tier now takes its own
  `web.existingSecret` with just what it needs.
- **The web and Postgres pods satisfy a `restricted` Pod Security Standard.**
  Both lacked a container `securityContext`; Postgres lacked `runAsNonRoot`;
  the web pod set `runAsNonRoot` with no numeric uid, which fails
  `CreateContainerConfigError` against an image using a named `USER`.
- **`podServiceAccount.create=false` no longer names a ServiceAccount that does
  not exist** — a new `external` flag distinguishes "I provision it myself"
  from "don't use one", instead of failing every pod create after a green
  preflight.

### Added

- **The fast lane got the Postgres client the full gate had, and a test that
  keeps both lanes honest.** `dev-ci.yml`'s Go job runs the same
  `go test ./...` against the same server-18 service as `ci.yml`, but installed
  no matching client — so `TestBackupRestoreRoundTrip` hit its
  client/server-major `t.Skipf` and the **only** coverage of `fleet backup` /
  `fleet restore` ran as a SKIP on every dev push. It also left `ci.yml` as the
  sole home of that step, which is why a broken version of it could not surface
  until a dev→main promotion PR. Both lanes now install
  `postgresql-client-18`, put its versioned bin dir on `$GITHUB_PATH`, and
  assert the major twice (install by absolute path, PATH resolution in the next
  step). `TestGoSuiteLanesInstallMatchingPgClient` asserts both halves for both
  lanes — the install and the `$GITHUB_PATH` append — because each has failed
  once already, invisibly, one per lane. Measured, not assumed: the round-trip
  test **passes** (not skips) once the majors agree.
- **A workflow + shell lint gate (`actionlint`, `shellcheck`):** nothing checked
  the ~3.1k lines of workflow YAML that decide what every other gate runs, and
  nothing checked the ~6.2k lines of bash that *are* the deploy path
  (`update.sh` / `bootstrap.sh` / `doctor.sh`). Both now block in `ci.yml` and
  `dev-ci.yml` and run from `make lint` via `lint-actions`, pinned and
  checksum-verified like `gitleaks` and `grype`. `actionlint` covers what
  Semgrep's `p/github-actions` pack and CodeQL's `actions` language do not —
  `${{ }}` expression syntax and types, undefined contexts, invalid
  `needs:`/`runs-on:`/cron — plus shellcheck over every `run:` block.
  Both start at **zero findings**, measured: the 5 actionlint and 3 shellcheck
  items were fixed or annotated with reasons in the same change, so a failure is
  a regression rather than a backlog.
- **Two new CI invariant tests**, in the spirit of the existing pin/gate tests:
  every workflow must declare a top-level `permissions:` block (without one it
  silently inherits the repository default), and the `actionlint`
  version/checksum must agree across both lanes.
- **PR and issue templates.** The PR template prompts for what/why, how it was
  verified, and scope-and-deviations, plus the obligations that were previously
  a reviewer's job to remember (CHANGELOG entry, `docs/<FEATURE>.md` note, an
  ADR in the *same* PR when an invariant moves). `ISSUE_TEMPLATE/config.yml`
  disables blank issues so the private security-disclosure link is unmissable —
  `SECURITY.md` says "do not open a public issue for a vulnerability" and the
  New Issue button was offering a blank box with no such warning.

### Changed

- **Automatic merging removed.** `auto-merge-dependabot.yml` is deleted and every
  reference to it across `SECURITY.md`, `dependabot.yml`, `CODEOWNERS` and
  `docs/SCANNING.md` is gone. Its own header had argued the case against it: it
  explained that `gh pr merge --auto` holds a merge only on *required* checks,
  named `dev` as a branch that requires none, and then listed `dev` in its own
  `branches:` filter — so the mitigation the docs credited was in fact the
  delivery mechanism for same-day patch bumps landing unattended. Every
  dependency bump now waits for a human.
- **DCO sign-off is no longer requested.** The requirement was documented in
  `CONTRIBUTING.md` and enforced nowhere; rather than add a gate for it, the
  requirement was dropped.
- **`ci.yml` gained a `concurrency:` group** (the expensive lane had none, so
  stacked pushes ran full suites to completion). Cancellation is scoped to
  `pull_request` — a push to `main` is the only tree-wide CodeQL verdict and must
  not be cancelled. **`timeout-minutes` is now set on every job** (it was present
  in 2 of 13 workflows).
- **`issues: write` no longer sits at workflow scope** in the two scheduled scan
  lanes, where it was live for every step of a long job running
  `govulncheck@latest` and a podman build pulling ~400 RPMs. It moved to a
  dedicated alarm job that checks out nothing — the shape `scan-cron-alarm.yml`
  already used. `persist-credentials: false` added to the write-scoped checkouts.
- **`make ci-web` now mirrors the real web job.** It ran 4 of its 8 steps,
  dropping both `npm audit` gates, the override canary and the explicit
  `npm run typecheck` — a clean local run and a red PR.

### Fixed

- **`AGENTS.md` reconciled with the tree it describes.** The Kubernetes backend
  (ADR-0049) landed an index row and nothing else, so the headline paragraph and
  the "sandbox is mandatory" invariant still called the sandbox
  *rootless-Podman* — an agent reading only that file would treat
  `FLEET_SANDBOX_BACKEND=kubernetes` as an invariant violation rather than a
  supported backend. Both now say pluggable-backend/mandatory-sandbox, and the
  invariant additionally names the mechanism it had left implicit: the
  `fleet_host_executor` build-tag fence (#159) and the kubernetes backend's
  fail-closed preflight. Also corrected: the `make test`/`test-race`/`test-cover`
  lines omitted `-tags fleet_host_executor` (a bare `go test ./...` builds a
  different tree than CI); the web block was a stale copy of the CI job that
  dropped the second npm tree (`scripts/rampart-service`) and the override
  canary, where `make ci-web` now runs all eight steps; `make govulncheck`,
  `ci-go`, `ci-web` and `ci-local` were missing from the target list; CI was
  described as one lane when `ci.yml` fires on `main` only and `dev-ci.yml` is
  `dev`'s only signal (so the promotion PR is the first full-gate run); the
  merge-gate enumeration omitted actionlint/shellcheck, the Helm lint and the
  Playwright suites; and the repository map omitted `deploy/`, the harness
  binaries under `cmd/`, and the `/settings` + `/admin` web routes.

- **Two green-but-vacuous holes in `ci.yml`.** The `postgresql-client-18` install
  was best-effort (`|| echo`), so an unreachable PGDG left client 16 in place and
  `backup_test.go`'s major-mismatch `t.Skipf` turned the *only* coverage of
  `fleet backup` / `fleet restore` off behind the single required check on
  `main`; it now asserts the major — and asserts it twice, because the first
  version of that assertion could not pass: installing `postgresql-client-18`
  does not change what `pg_dump` resolves to. `/usr/bin/pg_dump` is
  postgresql-common's `pg_wrapper`, which dispatches on the version/cluster in
  `~/.postgresqlrc` or `/etc/postgresql-common/user_clusters` rather than on the
  newest client present, so on a runner carrying a PostgreSQL 16 cluster the
  wrapper kept selecting 16 and the step failed with `got=16` over a successful
  install. The versioned bin dir now goes first on `$GITHUB_PATH` — which is
  what the round-trip test needs anyway, since it execs `pg_dump` off PATH — and
  the install is asserted by absolute path in that step, PATH resolution in the
  next one (`$GITHUB_PATH` only applies from the following step on). And the
  docs-only classifier initialised
  `docs_only=true` and only ever cleared it inside its loop, so an **empty** diff
  classified as docs-only and skipped the suite — which `ci-gate` then waved
  through, because an empty diff is the absence of evidence, not evidence that
  only prose changed.
- **`dev-ci.yml`'s `go test` could report cached results.** It lacked the
  `-count=1` that `ci.yml` passes on all three of its invocations; `setup-go`
  restores the build cache, which holds test *results*, and Go's cache key cannot
  see a Postgres service container.
- **Semgrep's deferred-failure path was unreachable.** `set -uo pipefail` does not
  clear the `-e` the runner supplies, so the step aborted on the `semgrep` line
  and `status=$?` never ran. The outcome was still correct; the documented design
  was not what executed.
- **The docs advertised a CodeQL waiver route that does not work.** Five places —
  `SCANNING.md`, `CODEQL.md`, ADR-0048, `CONTRIBUTING.md` and the gate's own
  runtime advice — offered an in-source `// codeql[rule-id]` comment, while
  `codeql.yml` recorded that all three forms were tried on #1249 and none
  produced a `suppressions` array. `codeql.yml` carried *both* claims, the stale
  one directly above the note refuting it. The accepted-findings register is the
  only route that works.
- **ADR-0048's normative Decision stated the gate threshold as an OR** where
  `codeql-gate.jq` implements a fallback; read literally it blocks on
  `go/log-injection` (`error` at security-severity 6.1) — the exact deadlock the
  ADR exists to undo.
- Assorted doc drift: `CONTRIBUTING.md` named Go 1.26.x (`go.mod` says 1.27.0);
  `CODEOWNERS` enumerated 7 of `ci-gate`'s 13 `needs`; `semgrep.yml` claimed its
  `pip install` had the same checksum guarantee as the `gitleaks`/`grype`
  downloads, which it does not.

- **Kubernetes as a first-class deployment (#989 / ADR-0049):** the fleet
  control plane can now run in a cluster with agent sandboxes as **ephemeral
  pods**, selected by one knob — `FLEET_SANDBOX_BACKEND=podman|kubernetes`
  (env overrides the bundle manifest's `sandbox.backend`, mirroring
  `sandbox.runtime`'s precedence; an unrecognized value refuses to boot).
  What landed, in one pass per the issue's plan:

  - **A third sandbox backend** in `internal/sandbox` (`k8sImpl`) behind the
    same internal interface as the podman and host executors: one pod per
    sandbox (`sleep infinity` + exec over the apiserver's WebSocket channel
    protocol), bash as one-shot execs, the python bridge as a held exec
    session, and file ops running the same embedded `fileops.py`. Pods are
    read-only-rootfs, non-root (uid 1000), all capabilities dropped, seccomp
    RuntimeDefault (or an operator-installed Localhost profile),
    `automountServiceAccountToken=false` — and the workspace is a shared
    ReadWriteMany PVC mounted at the **same absolute path** as the control
    plane, preserving the same-path invariant. The #796 poison-and-retire
    containment carries over: a cancelled/timed-out call deletes the pod with
    zero grace and retires the sandbox. A boot-time orphan-pod sweep mirrors
    the podman container prune. No client-go: a minimal hand-rolled REST +
    WebSocket-exec client (gorilla/websocket was already in the tree — zero
    new modules), with kubeconfig support deliberately narrow (token /
    client-cert; exec plugins and `insecure-skip-tls-verify` refused).
  - **Fail-closed preflight** when the backend is selected, at boot and in
    `fleet validate-config`: apiserver reachability + credentials, the exact
    RBAC verbs (pods create/get/list/delete, pods/exec create), the workspace
    claim, the sealed-egress NetworkPolicy object, and the RuntimeClass when
    configured. Podman-only knobs are refused rather than silently ignored
    (`FLEET_SANDBOX_RUNTIME` → use `FLEET_SANDBOX_K8S_RUNTIME_CLASS`;
    `FLEET_SANDBOX_SECCOMP_PROFILE` → `FLEET_SANDBOX_K8S_SECCOMP_PROFILE`;
    `FLEET_DEFAULT_NETWORK_MODE=allowlisted` is unsupported — the host egress
    proxy is unreachable from pods).
  - **Dedicated runner pools**: sandbox pods can be pinned to their own node
    pool with `FLEET_SANDBOX_K8S_NODE_SELECTOR` ("k=v,k=v") and
    `FLEET_SANDBOX_K8S_TOLERATIONS` (a JSON array), or the manifest's
    structured `sandbox.kubernetes.node_selector` / `.tolerations` — fleet's
    horizontal scaling story made concrete (more runner capacity = a bigger
    pool, never more fleet replicas). Malformed values refuse to boot. Sandbox
    pods also pin `imagePullPolicy: IfNotPresent` explicitly (the API's
    `Always`-for-`:latest` default breaks side-loaded kind images and re-pulls
    a mutable tag mid-run).
  - **A Helm chart** (`deploy/helm/fleet`): single-replica control-plane
    Deployment (strategy Recreate, deliberately no replica knob — the
    scheduler is single-owner), the runner RBAC Role/Binding, workspace/data
    PVCs, the `fleet-sandbox-deny-all` NetworkPolicy (selecting pods labeled
    `fleet.elcanotek.com/egress=none`), optional egress shaping for open
    pods, optional evaluation Postgres, optional web tier + Ingress. Linted
    and template-rendered in CI (new `helm` job inside both gates).
  - **Docs**: `docs/DEPLOYMENT-KUBERNETES.md` — the one Kubernetes reference:
    15-minute kind path, the two image builds (the control-plane
    Containerfile's `FROM golang:` stage is now pinned to go.mod by
    `scripts/check_versions_test.go`), production checklist, provider notes
    (EKS/GKE/AKS), day-2 operations (the CronJob equivalents of the systemd
    timers), troubleshooting, and an explicit honest-deviations list
    (NetworkPolicy enforcement belongs to the CNI, no per-pod pids limit, no
    #263 resource telemetry, no bundled-seccomp/supporting-doc mounts). Plus
    updates to `DEPLOYMENT.md` and `SANDBOX-RUNTIMES.md`
    (`FLEET_SANDBOX_BACKEND` documented next to `FLEET_SANDBOX_RUNTIME`).
    [ADR-0049](docs/adr/0049-kubernetes-backend-split-control-plane.md)
    amends ADR-0004: the single-box podman install **stays the default and is
    unchanged**; only the no-k8s-artifacts enforcement clause is superseded.

- **Kubernetes: bundle docs can serve the file tools again
  (`sandbox.kubernetes.bundle_docs_in_image` /
  `FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE`).** A sandbox pod mounts only the
  workspace claim, so the supporting-doc bind mounts (`protocols/`,
  `personas/`, `system_prompts/`, skills) do not apply — and because the
  fileop path anchor only trusts roots that are actually mounted, dropping
  them made `view_file protocols/foo.yaml` a *refusal*
  (`fileop root is not inside a sandbox bind mount`), not a miss. For a
  protocol-driven bundle that is most of the product. An operator whose
  sandbox image carries the bundle's doc dirs at the **same absolute paths**
  the control plane reads them from can now declare it, and those roots keep
  their read anchors inside a pod, so the file tools work exactly as they do
  under podman.

  The declaration cannot widen anything: it re-admits *read-only* anchors for
  roots the operator already configured, the read still executes inside the
  sandbox, and fleet cannot inspect an image — so a wrong declaration surfaces
  as a not-found read (the podman missing-dir behavior), never as a boundary
  change. It covers only the bundle's own doc dirs; other entries in the mount
  list (the uploads root) stay dropped, each with a log line. A malformed
  value refuses to boot, at boot and in `fleet validate-config`, which also
  now reports which way it resolved.

  One case no declaration can fix, now stated plainly in the docs: a bundle
  that inherits fleet's built-in skills pack resolves `SkillsDir` to a merged
  tree under the control plane's data dir, which no sandbox image can carry —
  so in-sandbox skill reads need the bundle's `skills_builtin: false`, and
  there is no configuration that yields both the built-in pack and working
  in-sandbox skill files.

### Removed

- **`docs/EKS-DEPLOYMENT.md`** — the hand-verified recipe for running the
  whole single-box model (rootless Podman included) inside one privileged pod
  on one large EKS node. Retired in favor of the first-class path above
  rather than kept as a parallel track: an unmaintained privileged-pod recipe
  beside a supported unprivileged one would imply support it never had (it
  was explicitly "hand-verified, not CI-exercised"). Its durable content —
  EFS/RWX storage, ECR/IRSA, NetworkPolicy-enforcement caveats, the backup
  CronJob, day-2 mappings — was folded into `docs/DEPLOYMENT-KUBERNETES.md`.

### Fixed

- **Completed-task resubmits silently discarded connector edits.** Operations
  lets an operator open a terminal task, change its connector picker, and save;
  because history is immutable, that action creates a new one-off re-run. The
  browser omitted `mcp_selection` from the re-run overrides and the server did
  not accept it, so the new run inherited the source task's stale connectors
  even though the editor showed the new choice. Re-run/clone overrides now
  distinguish an omitted selection (inherit) from an explicit list, including
  an explicit empty list (clear), and the terminal editor always sends its
  complete visible selection.

- **Three own-rows authorization holes on the task surface.** The read path for
  task rows was narrowed to own rows in #1082 and run logs in #980; three
  surfaces never got the same treatment and authorized on a *permission* alone,
  so any client-role principal reached every principal's rows:

  - `GET /tasks/paused` — `ListPausedTasks` selects on status alone with no
    principal predicate in SQL, and the projection carries each task's prompt.
    Its siblings (`/tasks/export`, `/tasks/upcoming`) both call `visibleTasks`;
    this one did not. It leaked other principals' paused prompts and their task
    UUIDs.
  - `PUT /tasks/{id}` and `POST /tasks/{id}/tags` — loaded the task with the
    unscoped `GetTask` and never checked ownership, so a client-role principal
    could rewrite a teammate's pending run: `prompt`, `model`, `mcp_selection`
    and `credential_allowlist` included. Only `run_if` was gated (admin-only).
  - `POST /tasks/{id}/feedback` and `GET /tasks/{id}/learned-instructions` —
    `taskFromPath` is lookup-only by contract ("a handler that needs an
    authorization decision makes it on the returned task") and neither caller
    made one. A down-vote with an attacker-authored critique fed `maybeDistill`,
    which mints a proposal from the victim's prompt at unmetered model spend;
    the GET disclosed their learned instructions.

  The write gate is a new `taskWritableByPrincipal`, deliberately **not**
  `principal.ownsTask`: `ownsTask` resolves through `ownerID()`, which is nil for
  every API-key principal, so it would deny a scoped intake-app key the right to
  edit the task it just created. `taskCreatedByPrincipal` matches a creating user
  **or** a creating key (`CreatedByKeyID`) — the model #980/#1082 established. A
  write surface must be no looser than the read surface guarding the same row.
  `TestScopedAPIKeyAuthorization` previously asserted "client key can edit an
  editable task" against an unattributed row, which *was* the vulnerable
  behaviour; it is split into the owned case (must keep working — the intake-app
  path) and the unowned case (must 403). Every fix is mutation-tested: stripped,
  the tests fail with the exploit visible.

- **Secret material and untrusted text reaching error strings, logs and the
  persisted transcript**, from the same audit sweep:

  - `internal/config/config.go`: `ValidateScheduled` interpolated the first 6
    bytes of `OPENROUTER_API_KEY` into a validation error — the only place in the
    tree where secret material reached an error string. Removed. Its doc comment
    also claimed "Called at startup" and has no production caller; corrected.
  - `internal/agent/scheduled.go`: run-error strings now pass through
    `agentcore.RedactSecrets` before both the log and the **persisted
    transcript**. Tool output, the stream sink, hooks and the session log were
    already scrubbed; run errors were the one path that skipped it, and the
    transcript is the larger surface.
  - Log-injection sinks carrying genuinely untrusted text are now `logSafe`/`%q`,
    matching each line's already-sanitized sibling: the task-create log
    (`task.Prompt`), the pre-validation client attachment path on the reject
    branches, the client-echoed attachment `Name`, the upload filename, and the
    API-key name.
  - `web/e2e/test-auth-key.ts`: the Ed25519 private key was written to a fully
    predictable path in the world-writable temp dir at default `0644`. Now
    `O_EXCL` at `0600` with random bytes in the sibling name.

- **Two input-validation gaps with a shell/URL surface.**
  `internal/mcpoauth/discovery.go` now refuses a non-`http(s)` scheme on the
  remote-derived discovery URLs (a `WWW-Authenticate` `resource_metadata`
  pointer, a PRM-declared issuer) *before* the request — already contained by
  `SafeHTTPClient` and the transport, so this makes the argument explicit rather
  than dependent on transport behaviour. And `internal/sched/models/models.go`
  validates `WorktreeConfig.BaseBranch`: it is the trailing positional of
  `git worktree add -b <b> <path> <base>` with no `--` separator, so a
  leading-dash value was parsed by git as an option — and `worktree_config` is
  settable by any task creator, unlike `run_if`.

- **Two stale claims that an auditor would have read as capabilities.**
  `models.MaxLogSubmissionSize` declared a 24 MB body cap that **nothing
  enforced** — the cap actually applied is `MaxJSONBodySize` (1 MB, wired through
  `BodySizeLimitMiddleware`), so the real posture was 24× stricter than the
  constant claimed. `config.DefaultFromEmail`'s doc comment called it "the
  fallback From address for outgoing mail" with no code path consuming it. Both
  deleted rather than left standing, along with four other exported-but-unreached
  identifiers (`mcpoauth.IsInvalidClient`, `apikeys.Manager.LogAction` — worth
  naming because it reads as API-key *audit* surface — and the two halves of the
  retired v1 remote-worker protocol, `TaskAssignment`/`LogSubmission`) and the
  tree's only commented-out code block. `golangci-lint`'s `unused` already makes
  unexported dead code structurally zero, which is why every deletion here is an
  exported identifier in `internal/` — the class `unused` deliberately does not
  report. Confirmed with `deadcode -test -tags fleet_host_executor ./...`.

- **CI permission gaps and an alarm that ignored the failure it was built for.**
  `scan-cron-alarm.yml` only fired on `conclusion == 'failure'`, which ignores
  `startup_failure` — the exact failure its own header describes as the
  motivating incident, and the one where *no scanning ran at all* — and
  `timed_out`, which matters given codeql.yml caps at 30 minutes and semgrep.yml
  at 15. It now alarms on any conclusion that is not `success` or `skipped`, and
  the daily real-model canary joins the watched list (it had no alarm at all).
  Noted in the file: the watcher matches on workflow **display name**, so
  renaming `name:` disarms it. Separately, `ci.yml` carried `pull-requests: read`
  at *workflow* level for golangci-lint-action's `only-new-issues`, which is
  explicitly `false` — a scope with no consumer that nonetheless reached every
  job not overriding it, including `web`, `playwright` and `e2e-live`, which
  npm-install and run thousands of third-party packages.
- **The CodeQL gate was armed on a measurement that could not mean what it was
  read to mean, and it deadlocked `dev`.** The `Fail on findings` step shipped
  with a threshold of *any finding at any severity*, justified by the security
  suite reporting zero across all four languages. That zero was real and it was
  measured — on **Dev CI run 525, a `pull_request` event**. On PR events the
  CodeQL action runs **diff-informed**: it builds the full database, evaluates
  every query, and then reports only results whose location falls inside the PR's
  diff. Run 525's own log says both halves (`Persisted 204 diff range(s) across
  43 file(s)`; `file coverage information is only enabled when analyzing the
  default branch and protected branches`). It measured the **diff**, not the tree.

  The first full-tree evaluation was therefore the **push** that merged that work:
  **Dev CI run 527**, which reported **38 Go and 17 javascript-typescript
  findings** and turned `Dev gate` red — with no PR-shaped way out, since a PR
  into `dev` is scanned diff-informed and comes back green while `dev` itself
  stays red. The generalisable rule, now written into the docs: **a PR-event
  CodeQL run certifies a diff, not a tree.**

  All 55 were triaged individually against the code. **Four were reachable and are
  fixed in code, not waived:**

  - `internal/sched/handlers/handlers.go` logged `task.Prompt` unsanitized on the
    task-**create** path while the **update** path's twin line was already wrapped
    in `logSafe`. `POST /tasks` is reachable with a scoped `create_task` key, so
    this was genuine log forgery.
  - `internal/httpapi/attachments.go` logged the raw, pre-validation client
    attachment path with `%s` on the two branches where the containment guard had
    just *failed* — precisely where the value is hostile by construction.
  - `internal/agent/session.go` logged the client-echoed attachment `Name`, which,
    unlike `Path`, is never re-sanitized on the `/chat` path.
  - `web/e2e/test-auth-key.ts` wrote an Ed25519 private key to a fully predictable
    path in the world-writable temp dir at default `0644`.

  **The gate was then redesigned rather than switched off**
  ([ADR-0048](docs/adr/0048-codeql-severity-gating.md)). A finding blocks when it
  is unwaived and either its rule publishes `security-severity >= 7.0` (CodeQL's
  own High/Critical cut) or, for a rule publishing no security-severity, its SARIF
  level is `error`/`warning`. Level is deliberately **not** used for rules that do
  publish a security-severity: nearly every CodeQL security query is
  `@problem.severity error` — `go/log-injection` at 6.1 included — so banding on
  level would block on all 23 log-injection findings and reproduce the deadlock.
  Below the band is **advisory**: printed and uploaded to the Security tab, not
  blocking.

  The remaining 51 findings live in `.github/codeql-accepted-findings.json`, a
  register of accepted `(rule, file)` pairs each with a mandatory written reason.
  Per-**file** is the point: a `query-filters` exclude would switch a
  security-severity 9.1 query (`go/request-forgery`) off repo-wide, while a
  register entry leaves it live everywhere else. Severity alone does not separate
  the true from the false positives here — that 9.1 fires on the deliberate `@url`
  fetch tool behind `internal/netguard`'s resolve-then-dial SSRF guard, and a 7.5
  `go/weak-sensitive-data-hashing` fires on SHA-256 used as a lookup index over a
  32-byte `crypto/rand` token — which is exactly why the band ships *with* a
  reviewed register rather than instead of one. An in-source `// codeql[rule-id]`
  comment waives too.

  One classifier, `.github/codeql-gate.jq`, is consumed by **both** the summary
  and the gate, so the report and the block can never disagree about what
  "blocking" means; the job log prints three tiers (BLOCKING / ACCEPTED by name /
  ADVISORY). It **fails closed** on a missing register, a missing filter file,
  unparseable SARIF, and — a vacuity check — findings present with **zero rule
  metadata resolved**. That last one is not hypothetical: CodeQL writes query
  metadata to `tool.extensions[].rules[]`, not `tool.driver.rules[]`, and a first
  cut of the filter read only the driver, resolved nothing, scored every finding
  at severity 0 and reported "0 blocking" over a tree that was not clean.
  `scripts/check_codeql_register_test.go` keeps the register honest in `make test`.

- **Two action pins were the annotated tag object of a mutable major tag, not a
  commit.** `git ls-remote` returns the *tag object's* SHA for `refs/tags/v4` on a
  repository that publishes annotated tags — for `github/codeql-action` that is
  `4c0873ef…`, while the commit (`refs/tags/v4^{}`) is `db488dde…`. A pin taken
  from the unpeeled form is a 40-hex string that passes every "is it a SHA" check
  and still resolves to a **moving major tag**, which is the exact defect the
  pinning exercise existed to remove. Two distinct pins were in that state across
  7 usages; both are repinned to the peeled commits with exact `# vX.Y.Z`
  comments, and `scripts/check_action_pins_test.go` now asserts the shape so the
  next pin cannot be taken from the wrong ref. Count for the record: **13**
  workflow files, 12 of which reference an action, **53** third-party action
  references, all SHA-pinned.

- **`ci.yml`'s docs-only classifier skipped the suite over compiled product
  content, and `CI gate` reported green over it.** The classifier matched `*.md`
  at any depth plus all of `docs/*`. `*` spans `/` in a shell `case` pattern, so
  that swallowed the `go:embed`'d `internal/clientconfig/builtin_skills/*/SKILL.md`
  files (asserted by three test files), the shipped
  `config/default/system_prompts/{default,chat}.md` — which *are* the system
  prompts `docs/PROMPT-CACHE-CONTRACT.md` exists to protect — and
  `docs/openapi.yaml`, which `cmd/fleet/openapi_drift_test.go` asserts against the
  Go models, plus `docs/scripts/*.py` and `docs/img/*.py`, which are inside the
  ruff, Semgrep `p/python` and CodeQL python scopes. A PR touching only a shipped
  prompt or the OpenAPI spec therefore skipped the tests that validate it while
  the gate went green. Narrowed to an explicit prose allow-list, and **`ci-gate`
  now refuses to pass over a `skipped` job unless the classifier actually said
  docs-only** — previously a skip from any cause was read as the docs-only case.

- **Dependabot could rewrite CI on a branch with no required checks.**
  `.github/dependabot.yml` targets `dev` for the `github-actions` ecosystem daily
  with **no `cooldown`** (Dependabot supports `cooldown` for gomod and npm only),
  `auto-merge-dependabot.yml` auto-merged patch bumps, and a `github-actions`
  bump *is a rewrite of `.github/workflows/*`*. Since `gh pr merge --auto` only
  holds a merge for checks that are **required**, and the `dev` ruleset requires
  none, a same-day third-party action patch could land on `dev` with no CI and no
  review. Mitigated on the workflow side: the `github_actions` ecosystem is now
  **excluded from auto-merge at any bump level**, the workflow carries an explicit
  `branches: [main, dev]` filter so its central assumption cannot silently stop
  holding, and its write scopes moved to the job. **The remaining fix is a
  repo-settings action nobody can perform from a PR** — adding `Dev gate` to the
  `dev` ruleset's required status checks — and it is now documented as an open
  item in [`docs/SCANNING.md`](docs/SCANNING.md) ("Known gaps") rather than
  implied away.

- **`fleet_ref` is validated against an allow-list shape before checkout** in
  `build-sandbox-image.yml` and `publish-sandbox-image.yml`. The `pin` step
  refuses an empty value, any character outside `[A-Za-z0-9._/-]` (so a newline
  cannot smuggle a second step-output line), an implausible ref name,
  `refs/pull/*` / `pull/*`, and a raw commit SHA — fork PR commits are reachable
  by SHA from this repository, so only named refs are accepted. Both workflows
  execute the checked-out build script and one of them holds `packages: write`.

- **Documentation corrected against the shipped workflows** for an
  enterprise-security review. The scanner docs had accumulated claims that were
  true of an intermediate state and false of the merged one: CodeQL "fails on any
  finding" and "zero findings across all four languages"; `docs/CODEQL.md`'s
  entire Triggers section (it documented `push`/`pull_request` triggers the
  workflow does not have, and justified the missing `push` on `dev` with the
  reasoning that the PR run analyzes "identical content" — the very trap that
  broke `dev`); `docs/TESTING.md` claiming the fast lane *skips* CodeQL when it in
  fact runs both scanners; the Grype threshold in three places; "12 workflows";
  and `ruff.toml`'s own gate marker contradicting its `select`. `SECURITY.md`
  gained the SAST section it had never had, plus the npm CVE gate it had omitted,
  and the `dev`-ruleset gap is now stated wherever a doc claims something
  "blocks".

- **Scanner follow-ups from the same effort: rule families widened and fixed,
  CodeQL at `security-extended`, grype tightened, override canary, cron alarms,
  and one reasoned rejection.**

  - **ruff `B`/`SIM`/`S` (bandit) enabled after fixing all 21 measured
    findings.** Both `zip()` sites got `strict=True` — each provably
    equal-length (one appends to both lists in lockstep; the other sits behind
    an explicit `len(row) != len(cols)` guard) — so a future desync fails loud
    instead of silently truncating. The unclosed `NamedTemporaryFile` in
    bento_doc.py moved inside its `with`. Fourteen deliberate best-effort
    `try/except-pass` sites (kernel cleanup, duck-typed pandas/numpy probes,
    unlink-on-failure paths) became explicit `contextlib.suppress` with the
    intent stated at each. The one `subprocess.Popen` carries a reasoned
    `# noqa: S603` (argv is `sys.executable` plus internal literals;
    mutation-tested — stripping the noqa re-fires the rule). Full Go suite
    green on the result.

  - **CodeQL widened to the `security-extended` suite** on all four languages.
    Its one `actions`-language finding was real:
    `actions/untrusted-checkout/medium` on `build-sandbox-image.yml`'s
    `fleet_ref`-fed checkout. Fixed, not waived (the `actions` language has no
    `AlertSuppression.ql`, so a comment waiver does not even exist) — see the
    `fleet_ref` entry above, and note that the identical hardening went into
    `publish-sandbox-image.yml`, the *unflagged* twin that holds `packages: write`
    and escaped the name-heuristic query only because its ref plumbing was named
    differently. The suite's no-findings result in CI (Dev CI run 525) was a
    `pull_request` run and is therefore a statement about that diff, not the tree
    — see the first entry above for what the tree actually held. The
    CodeQL/Semgrep log summaries also now print **`file:line` per finding** plus a
    database file count as the coverage line, and the override canary is invoked
    via `$GITHUB_WORKSPACE` so it survives the job's `working-directory: web`.

  - **Grype gate tightened to fixable CRITICAL + HIGH**, after measuring: the
    published sandbox image carries zero fixable Critical/High RPM findings (its
    only fixable findings are two Medium openssh advisories, which the next
    routine image rebuild picks up). The policy stays **RPM-only**
    (`.artifact.type == "rpm"`), because the Python `dist-info` Grype catalogs
    alongside Fedora's RPMs carries upstream versions and advisories — gating on
    it produced pip wheels layered over distro-owned files. Those records still
    upload to SARIF. Policy change mutation-tested in three directions: real scan
    passes, injected fixable High fails, injected fixable Medium still passes.

  - **`scripts/check-npm-overrides.sh`**: the rampart sharp/adm-zip overrides
    are forks of upstream's intent, correct only while upstream is broken — so
    both CI lanes fail with removal instructions once every locked
    `@huggingface/transformers` / `onnxruntime-node` instance accepts the
    patched lines. Mutation-tested in both directions.

  - **A red scheduled scan files an issue** (all four lanes; deduped by title,
    re-failures comment). A cron failure has no PR to surface it — the rot
    pattern that let the CodeQL toolchain break sit red for weeks. For the two
    reusable workflows the alarm lives in `scan-cron-alarm.yml`, a
    `workflow_run` watcher, because a called workflow may not request
    permissions its caller did not grant — the check fires at PLAN time, before
    any `if:` can skip the job, and the first attempt (an `issues: write` job
    inside codeql.yml/semgrep.yml) startup-failed the entire calling Dev CI
    run. Verified fixed on the next run.

  - **Semgrep rule vendoring investigated and rejected on license grounds**:
    the Semgrep Rules License v1.0 permits internal use only and states "This
    license does not allow you to distribute the rules" — committing them to
    this public MIT repo would be redistribution. The binary stays pinned; the
    rules stay registry-fetched with the failure mode documented.

- **The scanning stack, as shipped.** This entry supersedes four earlier ones that
  described intermediate states of the same work and contradicted each other on
  every threshold that matters — whether the scanners gate through `ci-gate` or
  stay advisory, whether `ruff format` gates, whether Semgrep ships one pack or
  four, and whether CodeQL's code-quality suite was restored or dropped. The
  shipped end state, stated once:

  - **Python had no linter at all, and ruff is now a blocking gate.** Go had
    golangci-lint and the web tier had oxlint; the tree's 13 Python files — the
    sandbox FileOp helper, the python bridge, the bento-slides and data-profiler
    skill scripts, MCP test servers, icon/doc generators — had nothing. Rule
    selection is narrow on purpose and `ruff.toml` records the numbers:
    `E4,E7,E9,F` found 3 real findings (an unused import, a lambda assignment, and
    a **byte-identical duplicate `has_guard` definition** in `bento_doc.py` where
    the second copy silently shadowed the first); `B`/`SIM`/`S` found 21 more, all
    fixed and those families then enabled; a broad selection finds 333, of which
    176 are `%`-format style and 43 are magic values, so the style tiers stay out.
    **`ruff format --check` also gates** — the whole tree was ruff-formatted in one
    dedicated commit (9 of the 13 files, ~3.7k lines, validated by the full Go
    suite, since the bento/fileops golden tests exercise these scripts). `F401` is
    waived for `internal/mcp/testdata/*.py` and `cmd/fleet/testdata/*.py`, where an
    unused import can be the point of the fixture.

  - **CodeQL narrowed to security queries only — the code-quality suite was
    enabled, measured, and dropped.** It produced 32 findings, every one
    note-level: for Go and the web tier it duplicated golangci-lint and oxlint,
    which already block; 28 of the 32 were Python, now ruff's job at a fraction of
    the runtime and with autofix; and 3 were false positives on correct code
    (`value != value`, the idiomatic NaN test). CodeQL keeps the thing nothing else
    here can do — interprocedural taint, which is the actual shape of "a credential
    must not reach a log sink".

  - **Semgrep runs all four packs and blocks** — `p/github-actions`, `p/golang`,
    `p/javascript`, `p/python`, with `--error` and no `continue-on-error`. The 6
    non-Actions findings are false positives, suppressed at the line with
    `nosemgrep: <rule-id>` plus a reason: three were *already* triaged and
    suppressed for gosec, and one (`0o644` for a sandbox directory) would have been
    a security regression if followed. Every suppression was mutation-tested —
    removing it makes the finding reappear, so a green scan means the waivers work
    rather than the rules having silently stopped matching. `p/github-actions` also
    found the one real class nothing else here checks: **51 actions referenced by a
    mutable tag**, all now SHA-pinned (see the pin correction above).

  - **`npm audit` is a blocking gate** for both npm trees, lockfile-only and
    failing on any severity — the npm counterpart of the govulncheck gate. `web/`
    was already clean. `scripts/rampart-service` **had no lockfile at all**, and
    generating one exposed **5 high-severity vulnerabilities** it had been hiding:
    `sharp <0.35.0` (four libvips CVEs) and `adm-zip <0.6.0`
    (GHSA-xcpc-8h2w-3j85) via `onnxruntime-node`. No upstream release fixes either
    — latest `@huggingface/transformers` still pins `sharp ^0.34.5`, and npm's
    suggested "fix" was a breaking transformers downgrade — so the package carries
    `overrides` to `sharp ^0.35.3` and `adm-zip ^0.6.0`, each the release
    immediately after the vulnerable line. The overridden stack was installed and
    load-tested, not just resolved: sharp renders a PNG through the new libvips,
    transformers loads on it, rampart exports its API, adm-zip round-trips a zip.
    Both trees now audit at 0.

  - **Gate wiring.** `codeql.yml` and `semgrep.yml` are **reusable workflows**
    (`on: workflow_call`) that ci.yml and dev-ci.yml call as jobs, and those jobs
    sit in `ci-gate`'s / `Dev gate`'s `needs` — so a scanner finding reaches the
    aggregate check with no settings change. Their own push/pull_request triggers
    are removed (nothing runs twice); the weekly re-scan crons and a
    `workflow_dispatch` stay. An earlier entry here claimed this needed a
    branch-protection click, reasoning from "`needs` cannot cross workflow files":
    true of a job's `needs`, but a `workflow_call` brings the called jobs into the
    caller's file. **Whether that red check blocks a merge is a separate,
    branch-dependent fact** — `CI gate` is required on `main`; the `dev` ruleset
    requires no status checks at all.

  - **All three semgrep parse errors fixed**, so no file is partially covered:
    `${{ steps.build.outcome }}` interpolated into a `run:` script in
    build-sandbox-image.yml (moved to `env:` — also the injection-safe form), a
    `${tag:-(…)}` expansion default whose bare paren choked the bash sub-parser
    (hoisted to a plain assignment), and an inline `import("@playwright/test")`
    type in fixtures.ts (now a named `import type`; web lint, tsc and all 1104
    vitest tests pass on it). The scanners' coverage lines read **0 parse/scan
    errors**.

  - Two knock-on fixes found while doing this: SHA pinning broke two regexes in
    `scripts/check_versions_test.go` that matched `golangci-lint-action@v\d+`, and
    they fail *open* by skipping — so they were widened to tolerate a pinned ref
    plus its trailing version comment, and mutation-tested to confirm they still
    bite. And a standalone `nosemgrep` comment inside a Go import block breaks
    `goimports`, so that one waiver is a trailing comment instead.

- **CodeQL had stopped analyzing the repo's Go code, and then stopped analyzing
  anything.** Default setup's Go analysis failed on every main-targeting PR from
  the Go 1.27 bump (#1240, promoted in #1242) onward — it installed the Go its
  extractor was built with and pinned `GOTOOLCHAIN=local`, so against a `go.mod`
  requiring 1.27 it could neither use nor fetch a workable toolchain:

  ```
  go: go.mod requires go >= 1.27.0 (running go 1.26.6; GOTOOLCHAIN=local)
  Extraction failed for all discovered Go projects.
  CodeQL job status was configuration error.
  ```

  The failure was red but never blocking (`ci-gate` is the only required check on
  main), so it was annotated in promote commit messages and lived on. Default
  setup is zero-config, with no Go version input and no env, so there was nothing
  to fix in place; switching it off to replace it left the repo scanning nothing
  at all in the interim.

  Replaced with an advanced-setup workflow, `.github/workflows/codeql.yml`, which
  restores security analysis over go, python, javascript-typescript and actions,
  and resolves Go's interpreter from `go.mod` via `actions/setup-go` — never a
  literal version, the bug class #1240 and #1241 already fixed twice for node.
  (The code-quality suite was also restored at this point, then measured and
  dropped — see "The scanning stack, as shipped" above for where that landed.)

  Two things the first cut got wrong, both of which ran **green**:
  `analysis-kinds` turns out to be GitHub-internal and unusable in a custom
  workflow (it logged `##[error]` and silently continued with security only), and
  Go extraction missed exactly one file — `internal/sandbox/host.go`, the
  unsandboxed host executor, invisible to the default build behind
  `//go:build fleet_host_executor`. Fixed with `queries: code-quality` (at the
  time) and `GOFLAGS: -tags=fleet_host_executor`, the same tag `ci.yml` and
  `dev-ci.yml` already pass to `go vet` and `go test`.

  Verified from the extractor's own output rather than the check mark:
  `extraction succeeded for all 2 discovered project(s)`, 916 packages, 426 `.go`
  files including `host.go`, and distinct queries evaluated rising from 72→116
  (go), 90→292 (python) and 178→374 (javascript-typescript) as the quality suite
  came in, with `actions` unchanged at 36 by design. See
  [`docs/CODEQL.md`](docs/CODEQL.md).

- **`fleet update` built the web tier on the node it had just refused.** Every
  update on a Fedora box printed `✓ web tier will build+run on /usr/bin/node-24
  (v24.x)` and then, a few lines later, npm's own rejection of that claim:

  ```
  npm warn EBADENGINE Unsupported engine {
  npm warn EBADENGINE   package: 'fleet-web@0.1.0',
  npm warn EBADENGINE   required: { node: '>=24' },
  npm warn EBADENGINE   current: { node: 'v22.23.1', npm: '10.9.8' }
  ```

  `fleet_node_build_path` pinned the bare name `node` with a private shim
  directory at the head of PATH, on the documented reasoning that npm's shebang
  is `#!/usr/bin/env node`. On Fedora it is not: because the node streams are
  parallel-installable, the spec rewrites npm's shebang to the **absolute**
  `#!/usr/bin/node-<major>`, so `npm ci && npm run build` stayed on the default
  stream no matter what PATH said — and `next`, launched by npm, inherited it.
  The tier was built on node 22 and served on node 24, which is the same
  claimed-but-not-done fault the shim was introduced to remove, one link
  further down.

  The shim now also carries `npm` and `npx` wrappers that exec the resolved
  interpreter against the matching `npm-cli.js` / `npx-cli.js` — the one form of
  the answer no shebang can override. Around that: `node_probe` resolves npm
  **separately** from node (Fedora ships it as its own `nodejs<major>-npm`
  package, so "node 24 is installed" never implied "npm builds on node 24") and
  the gate hands a missing one to the same `doctor.sh --node` repair — for a
  *versioned* interpreter only, since a single-node layout's unreadable npm is
  not evidence of a wrong interpreter; the build
  step reads the interpreter back **from npm** instead of from the symlink it
  just made, and refuses below the `web/.nvmrc` floor; `doctor.sh` checks and
  repairs the pairing, so `fleet doctor --node` and `fleet update --check` fail
  on it too; and `bootstrap.sh` reports the version the build actually ran under.
  Design note: [`docs/NODE-TOOLCHAIN-HANDOFF.md`](docs/NODE-TOOLCHAIN-HANDOFF.md)
  ("The npm interpreter pin").

### Changed

- **Go 1.27 (`go.mod` 1.26.7 → 1.27.0)** — the pin had gone stale: Go 1.27.0
  shipped 2026-08-18 and nothing in the repo tracks upstream Go, because
  Dependabot does not update the `go` directive (dependabot-core#9527), so
  `fleet update` kept fetching a 1.26 toolchain. The bump moved four things
  that had to move with it:
  - **The two silent copies of the version are now asserted, not remembered.**
    `web/go.mod` (a no-package boundary module, so a stale `go` line compiles
    fine forever) and the `FROM golang:<minor>` stage in
    `docs/EKS-DEPLOYMENT.md` (which an operator copies verbatim) both drifted
    unnoticed. New `TestGoMinorAgreesEverywhere` in `scripts/check_versions_test.go`
    fails on either, the same way `TestNodeMajorAgreesEverywhere` already
    guards the node major.
  - **golangci-lint v2.12.2 → v2.13.1**, because the lint gate cannot lag the
    toolchain: golangci-lint refuses to run when the Go it was built with is
    older than the version go.mod targets. Upstream ships the supporting
    release days after a Go lands (v2.13.0 on 2026-08-19, one day after Go
    1.27.0), and that coupling is now written down in `.golangci.yml` next to
    the pin.
  - **`make govulncheck` builds the scanner with go.mod's toolchain.**
    `go run <tool>@latest` resolves its toolchain from the TOOL's go.mod, and
    GOTOOLCHAIN=auto never upgrades past that, so on the configuration this
    repo explicitly supports — a distro Go lagging go.mod — the scanner got
    built with 1.26 and then refused the tree ("package requires newer Go
    version go1.27"). A deliberate `GOTOOLCHAIN=local` or version is still
    passed through untouched.
  - **Two staticcheck deprecations v2.13.1 surfaced**, both fixed by deleting
    dead code rather than migrating: `zip.FileHeader.ModifiedTime/ModifiedDate`
    in `internal/httpapi/xlsx_sanitize.go` were fed values `archive/zip`
    immediately recomputes from `Modified`, and `SendDefaultPII: false` in
    `internal/observability/sentry.go` was already the zero value. The Sentry
    one is deliberately NOT migrated to the replacement `DataCollection` field:
    a hand-written `DataCollection` cannot reach the unexported
    `sensitiveTerms` deny list and re-defaults `UserInfo` to true, so
    "modernizing" it would have loosened PII scrubbing (verified: omitting the
    field resolves to `UserInfo:false, HTTPBodies:[], sensitiveTerms:[forwarded
    -ip remote- via -user]`, identical to before).
- **Tool-output format classification no longer copies the whole result
  (`internal/agentcore`)** — `json.Valid([]byte(content))` picked the
  truncation envelope's `format` label, and through Go 1.26 the compiler elided
  that conversion, so validating a 64MiB tool result allocated ~600 bytes. Go
  1.27 no longer elides it: the same line allocated the full 64MiB and tripped
  `TestBoundModelVisibleToolResponse_LargeInputHasBoundedAllocation`. Relying
  on that elision was always the wrong shape for a file whose other paths are
  careful never to materialize an attacker-sized string, so `looksLikeJSONDocument`
  now validates exactly (unchanged `json.Valid` semantics) up to 1MiB and falls
  back to a structural first/last-byte sniff above it. `format` is metadata that
  only selects between two bounded, self-describing envelopes — neither re-emits
  the original content — so the one behavioral consequence is that a
  multi-megabyte brace-wrapped-but-invalid blob is labelled `json` instead of
  `text`. Both tiers are pinned by `TestLooksLikeJSONDocument_TieredClassification`,
  including that the exact tier still agrees with `json.Valid` case for case.

- **`agent.RunTurn` and `runner.executeTask` extracted into named phase
  helpers (#1127)** — the two ~300-line functions the audit flagged (the
  confirmed bugs #1105/#1117 partly stemmed from how much state they juggle)
  now read as a narrative of phases. `RunTurn` delegates to
  `admitInteractiveTurn`, `composeTurnSystemPrompt`, `assembleTurnMessages`,
  `interactiveRunSelection`, and `openTurnRemoteOverlay`, and its three
  outcomes to `failedTurnResult` / `cancelledTurnResult` (pre-existing) /
  `completedTurnResult`; `executeTask` delegates to `buildTaskRunContext`,
  `captureRunFailure`, and one named helper per terminal outcome —
  `parkForQuestion`, `finishStopped`, `finishLeaseLost`, `finishSuccess` —
  joining the existing `parkForWake`/`failWallTimeout`/`failAuditAborted`/
  `failInterrupted`/`handleRunFailure` family. Pure extraction with zero
  behavior change: every extracted body is line-identical to its original span
  modulo parameter threading (verified span-by-span against HEAD; all other
  declarations in both files verified byte-identical via a hash inventory),
  and every function-exit defer (limiter release, sandbox/workspace/MCP-scope
  cleanup, overlay close; wall-deadline cancel, stream seal, terminal SSE
  frame) stays registered in the parent so its firing scope is unchanged.
- **`internal/sched/db/db.go` split by domain (#1127)** — the 2,882-line god
  file is now thirteen domain files in the same package: connection lifecycle,
  the cross-domain (un)marshal helpers, and transaction support stay in
  `db.go`; the rest moved to `users.go`, `tasks.go` (CRUD core),
  `task_values.go` (per-column value mapping + JSONB codecs), `task_queries.go`
  (listings/filters/rollups), `claim.go` (claim/lease/serialization gate/lease
  recovery), `scheduling.go` (gate settlement, skip recording, batch status,
  recurrence-spawn sweep), `pause.go`, `wake.go`, `sla.go`, `logs.go`,
  `cleanup.go` (retention + log-archival sweeps), and `task_iterations.go` —
  matching the granularity of the existing `budgets.go`/`datasets.go` siblings.
  Pure structural move with zero behavior change: every declaration and its
  comments relocated byte-identically (verified declaration-by-declaration
  against a hash inventory of the old file), no visibility changes, no renames,
  and the `task_columns.go` registry (#1126) untouched.

- **`internal/httpapi/server.go` split by resource (#1127)**: the 3,626-line
  file now keeps only the Server struct and options, the inflight-turn
  registry, `/healthz`, and shared helpers; route registration + top-level
  middleware, memories, the config reads (personas / server-config /
  client-config), the MCP-server catalog surfaces, the conversations
  collection + sub-route dispatcher, the chat turn path, SSE stream reattach,
  turn-event pages, workspace files, and the audit read each moved to their
  own file, byte-intact. Two intentional changes beyond the move, both
  behavior-preserving: the ~630-line `conversationByID` switch (which carried
  a `//nolint:gocyclo`, now retired) became a flat `(sub, method)` dispatch
  table over extracted per-action methods, and the per-branch ownership-check
  and JSON-decode boilerplate deduplicated into `withOwnedConversation` +
  `decodeJSONBody`. Divergences among the old branches are preserved verbatim
  and now explicit: the 404 body is "not found" or "conversation not found"
  per historical branch (and `http.NotFound` with its conditional check on the
  model route), and the two lenient decodes (cancel scope, legacy bare
  DELETE /conversations) keep their inline decode. No route, method,
  middleware-order, or response change.
- **The installer's node handoff: `fleet update` now repairs a node shortfall
  instead of refusing** — on a box a node major behind the checkout, the
  documented sequence (`bootstrap → update → status/doctor`) hard-failed on the
  second step, and the fix lived in a verb that comes later in that line.
  Running doctor *first* would not have helped either: both scripts read the
  floor from the checkout's `web/.nvmrc` and doctor never pulls, so on a box
  provisioned before `.nvmrc` existed a pre-update doctor run reads the old
  hardcoded floor, sees node 22 and passes. New `scripts/doctor.sh --node`
  performs just the node repair (install `nodejs<major>` + `-npm`, stamp
  `FLEET_NODE_BIN`, assert the resolved value) and `update.sh` hands the
  shortfall to it, then **re-resolves** rather than trusting its exit code —
  the box repairs in place with no re-run and no prior knowledge of the order.
  Scoped to `--node` on purpose: a full doctor pass adopts drifted units, a
  write `fleet update` performs only behind `--adopt-units`. `--no-node-repair`
  (`FLEET_UPDATE_NODE_REPAIR=0`) restores the refusal for boxes whose node comes
  from nvm/NodeSource. The gate also **moved** to after the pulls and before the
  sandbox rebuild: it used to fire after a 2-3 minute image build and a layer
  prune while printing "nothing has been built or installed yet".
  `update --dry-run` now runs the resolver for real (it printed
  "would resolve node" without ever calling it, then closed with a green
  "fleet rebuilt at <sha>" banner and exit 0 on a box the real run refuses), and
  `fleet update --check` reports node readiness and exits non-zero rather than
  recommending a command that cannot succeed. `doctor.sh` and `update.sh` now
  assert `FLEET_NODE_BIN` by reading it back through the same last-wins reader
  systemd uses, instead of trusting the writer's return code. `doctor.sh --node`
  also installs the versioned `nodejs<major>-npm` (the old stream's npm
  satisfies `command -v npm`, so the missing-tool loop never fired on the box
  this is for), advises the `fleet-web` restart a scoped run cannot perform
  itself, and no longer reports a false shortfall when run unprivileged against
  a 0600 `fleet-web.env` it cannot read. Design note:
  [`docs/NODE-TOOLCHAIN-HANDOFF.md`](docs/NODE-TOOLCHAIN-HANDOFF.md).
- **The build now really runs under the interpreter the gate resolved** —
  `fleet_node_build_path` prefixed PATH with the resolved binary's *directory*,
  which does not work on the layout it was written for: Fedora's node streams
  are parallel-installable **into the same directory**, so putting `/usr/bin` in
  front still resolves the bare name `node` to the default stream. Measured — a
  build "on /usr/bin/node-24" ran npm's `#!/usr/bin/env node` shebang under node
  22. It now puts a private shim directory holding a single `node` symlink in
  front (mktemp'd, not a predictable /tmp path — that would be a world-writable
  directory at the head of root's PATH during a build), and both callers remove
  it. `update.sh` also reports the version the build actually ran under, read
  back from the build PATH, rather than the one the gate intended.
- **`scripts/fleet-upgrade.sh` brings the web tier back up** —
  `deploy/fleet-web.service` carries `BindsTo=fleet.service`, so the script's
  `systemctl restart fleet` stopped fleet-web and nothing restarted it; the run
  then printed `✓ fleet upgraded + healthy`, a claim its readiness gate (which
  polls only the Go backend's `/readyz`) never covered. It now restarts
  fleet-web on both the upgrade and rollback paths, asserts `systemctl
  is-active`, and the banner reports what it measured — including on the
  no-systemd and no-curl paths, where the readiness gate is skipped entirely and
  the banner now says health was **not** verified instead of asserting a
  `/readyz` 2xx that was never polled. It remains deliberately
  node-unaware: it never builds the web tier, so the node major is not its
  business.
- **`--help` no longer truncates or leaks shell** — bootstrap, update and
  fleet-upgrade each rendered help with a hardcoded `sed -n '2,Np'` range that
  rots as the header grows. `fleet-upgrade.sh --help` was printing raw script
  body; `update.sh` and `bootstrap.sh` were silently dropping their last
  paragraphs. All three now derive the header block and stop at the first
  non-comment line, guarded by a test that fails on either truncation or leak.
  `doctor.sh --help` also stated a stale `Node >= 20` floor while the resolved
  floor was 24.
- **The oxlint burn-down is finished: every rule is `error`, zero findings** —
  `web/.oxlintrc.json` carried 11 rules at `warn` covering 55 real findings
  (documented as a burn-down list, not policy). All 55 are fixed and every rule
  is promoted to `error`; `npm run lint` runs with `--deny-warnings` so a rule
  re-introduced at `warn` fails the build. These were real accessibility
  repairs, not lint appeasement: three connections dialogs gained Escape,
  focus-on-open and focus-return (they had no keyboard dismissal at all — the
  users popover already closed on Escape and gained the focus handling); six
  `autoFocus` attributes became effects tied to the user action that opens the
  field, so focus no longer moves on an unrelated remount, and a seventh was
  removed outright because `Menu` already moves focus into a surface it opens,
  so the attribute was being overridden anyway; four
  task-modal switches got a real name/description split instead of a paragraph
  of prose as their accessible name; the toast gained a labelled dismiss button
  (it was mouse-only); the cost estimate became a real `<button>` (its
  `tabIndex={0}` was the only keyboard route to the breakdown); `MenuSeparator`
  became an `<hr>`; and internal `<a href>` navigation became `<Link>` where the
  target is genuinely a page. **Behavior change:** a toast is no longer
  dismissed by clicking anywhere on it — the dismiss target is the new × button
  (auto-expiry is unchanged). That is a deliberate trade: the toast is a
  `role="alert"` live region, so making the whole thing a control both
  mis-labels the announcement and puts a self-destructing element in the tab
  order. One rule is scoped off for `*.test.*` —
  `nextjs/no-html-link-for-pages` on a `vi.mock` factory — argued in the config
  on the merits: the defect it prevents is a runtime navigation that a module
  stub cannot perform, and the alternative rewrite would make the stub diverge
  from the DOM the component under test actually queries.
- **TypeScript 7 all the way down; ESLint replaced by oxlint** — the web tier now
  runs TypeScript 7, the native Go compiler, everywhere: `npm run typecheck`,
  the `next build` type pass, and editors. No TypeScript 6 is kept anywhere.
  The blocker was never the compiler — on TS 7, `npm ci` succeeds,
  `tsc --noEmit` is clean and `next build` succeeds; only ESLint failed, with an
  explicit *"typescript-eslint does not support TS 7.0"*. TS 7 is the native
  rewrite (a ~3.6 MB shim around a platform binary, `tsc` but no `tsserver`), so
  the JS compiler API typescript-eslint is built on is absent; it targets
  TS >= 7.1, skipping 7.0, and reached this repo only via `eslint-config-next`.
  The documented side-by-side workaround (TS 6 for the linter, TS 7 aliased for
  compilation) was rejected: two TypeScripts can disagree about the same code.
  **oxlint** has no such coupling — a Rust linter with its own TS parser, so the
  TypeScript version is irrelevant to it. Coverage was compared rule by rule
  before switching: nextjs 22→21, typescript 20→39, react incl. hooks 33→42,
  jsx-a11y 6→35, import 1→8. Exactly **one** rule is lost,
  `no-location-assign-relative-destination`, whose 17 findings were
  long-standing warnings — and oxlint's `no-html-link-for-pages` enforces the
  adjacent `<a href>` mistake more aggressively, catching 8 real cases ESLint
  reported none of. `npm run lint` is no longer type-aware (that is *why* it
  works on TS 7), so the type rules `@typescript-eslint` used to contribute now
  come from a dedicated **`npm run typecheck`** step that runs in CI ahead of
  the build — types are still gated, by the compiler rather than the linter.
  The `warn`-severity rules in `.oxlintrc.json` are an explicit burn-down list,
  not policy: oxlint's a11y coverage is far wider than the old gate's and
  erroring on all of it would have failed on ~121 pre-existing findings, so they
  are visible without conflating "adopt TS 7" with "fix every a11y issue".
  Measured: lint **30,418 ms → 607 ms (~50×)**, typecheck ~2 s, 1080/1080 tests.
  **Both Dependabot `ignore` entries are gone** — the eslint-major and
  typescript-major holds existed for the same reason (eslint-config-next pinning
  plugins to peer `eslint <=9`, typescript-eslint refusing TS 7), so removing
  ESLint removed the cause and both are ordinary updates again. There are now no
  `ignore` entries anywhere in `.github/dependabot.yml`.
- **Deleted the dead `web/scripts/` tree; Go to 1.26.7; Dependabot cadence** —
  three loose ends from the version audit. `web/scripts/` was an unreferenced
  8-file legacy deployment stack (its own `bootstrap.sh`, `update.sh`,
  `provision.sh`, `e2e-boot-server.sh`, …, three of them shadowing live
  `scripts/` equivalents), untouched since July, still calling the product
  "Elcano Chat", and installing **Node 20 — EOL 2026-04-30 — via NodeSource**,
  which `scripts/doctor.sh` explicitly forbids ("fleet does not use
  NodeSource"). It was the largest concentration of version rot in the repo and
  nothing could break, because nothing referenced it. Go moves `1.26.6` →
  `1.26.7`, the patch on its own supported line, where Go's security fixes land
  — now a one-line change rather than the eight it would have been before the
  consolidation; `web/go.mod` drops its patch pin entirely (major.minor only)
  since that sentinel module has no packages and the patch was just a second
  copy to keep in sync. `ONBOARDING.md` stops restating the patch number.
  Dependabot's `interval` goes **weekly → daily** on all five entries: cooldown,
  not the polling interval, is what guards against a compromised fresh release,
  so polling weekly *on top of* a 7-day cooldown added up to another week before
  anyone learned an update existed — daily plus the existing grouping keeps
  routine minor/patch traffic to one in-place PR per ecosystem while a major
  appears the day it becomes eligible. Also recorded in that file: it is read
  from the **default branch**, so config changes are inert until promoted to
  `main`; and `target-branch: dev` means security updates bypass every option
  here (ungrouped, unprefixed, `ignore` rules do not apply) while no version
  update ever targets `main` directly.
- **Version-pin audit: one declaration point per tool, and it is now enforced by
  a test** — a fan-out audit of the change above found the same pathology
  everywhere else, plus real bugs in the node work itself.
  *Consolidated:* Go was pinned `1.26.6` in **eight** functional places; the five
  `go-version:` literals now read `go-version-file: go.mod` (two workflows
  already did), and `.golangci.yml`'s `run.go` pin is deleted because
  golangci-lint's documented default is already the go.mod version. `@types/node`
  was `^26` against a node-24 runtime — its major tracks the runtime, so the app
  was type-checked against API surface it does not have.
  *Enforced:* new `scripts/check_versions_test.go` (runs in `make test`, same
  idiom as the migration/grype linters) asserts `web/.nvmrc` agrees with both
  `engines.node` blocks, `@types/node`, and the rampart Containerfile; that no
  workflow carries a literal `node-version:`/`go-version:`; and that
  twice-declared tool pins (Grype version+SHA, gitleaks, golangci-lint) match
  across workflows. Each assertion was mutation-tested.
  *Bugs fixed in the node change before it shipped:* the web build ran on
  whatever `node` PATH resolved to — npm's shebang is `#!/usr/bin/env node` — so
  bootstrap/update printed "will run node 24" and built on 22; `bootstrap.sh`
  asked for the unversioned `npm` package, dragging the old stream back onto the
  box, and `doctor.sh` did the same via `npm) pkg=nodejs`; the shim was installed
  *after* the build, so a failed build on a fresh box left the unit pointing at a
  non-existent ExecStart (a permanent 203/EXEC loop under `Restart=always`);
  `upsert_web_env` corrupted any env file lacking a trailing newline, destroying
  `NODE_ENV` and never setting the key; it rewrote only the *first* duplicate of
  a key while systemd's `EnvironmentFile` is last-wins, so the check passed
  forever while the box used a stale value; `resolve_node_bin` claimed to find
  the "newest" node but matched only the exact major, refusing to deploy on a box
  with a *newer* one; an unreadable `.nvmrc` silently defaulted to a hardcoded
  `24`; and `update.sh` aborted with "nothing was changed" after already
  rebuilding and installing binaries — that gate now runs before the first side
  effect. The three copies of the resolver are now one
  `scripts/lib/node-version.sh`. bootstrap and update now assert the **resolved**
  `ExecStart` before reporting success, since neither installs a drifted unit.
  *Dependabot:* three genuinely uncovered manifests are now watched —
  `scripts/rampart-service/package.json` (two `^` ranges, no lockfile, and an
  unpinned `npm install` at image build time) and both `Containerfile`s. Podman's
  `Containerfile` name **is** supported (dependabot-core matches
  `/dockerfile|containerfile/i`), contrary to most published advice. Added a
  `typescript` major-ignore mirroring eslint's, for a narrower reason than first
  recorded: on TS 7 `npm ci` succeeds, `tsc --noEmit` is clean and `next build`
  succeeds — only `npm run lint` fails, because typescript-eslint explicitly
  refuses TS 7.0 (it is the native Go rewrite, with no JS compiler API to drive)
  and is targeting >= 7.1. Documented in docs/TESTING.md along with the
  side-by-side workaround that does work (~3.7x faster) and why it is not
  adopted: CI has no separate `tsc` step, so nothing CI runs would get faster. `semver-major-days` cut from 14 to 7: majors are never auto-merged, so
  the cooldown delayed visibility without protecting anything.
  *EOL and stale:* Grype `0.115.0` → `0.117.0` (checksum taken from the release's
  own checksums file and verified against the downloaded artifact — a CVE scanner
  should not be the stalest tool in the pipeline); `fedora:41` (EOL 2025-12-15) →
  `44`, `nginx:1.27-alpine` (EOL 2025-06-24) → `1.30-alpine`, and
  `grafana/grafana:11` (EOL 2026-06-25) → `13` in operator-pasteable docs; the
  obsolete `sharp` override is dropped (Next's own `^0.35.3` is *stricter*, so
  ours could only ever loosen it — verified by resolving to the same 0.35.3
  without it) while the `postcss` override stays, re-documented, because it is
  what unpins Next's exact `8.5.23`. Three contributor docs still said "Node.js
  22" — one of them promising it matched CI — and now point at `web/.nvmrc`.
- **node 24 everywhere, declared once** — the repo targeted three different node
  versions at the same time: CI pinned `'22'` as a literal across **six jobs in
  four** workflow files, `scripts/doctor.sh`'s floor said `20`, and the box ran whatever
  `dnf install nodejs` happened to mean. Nothing reconciled them, and Dependabot
  could never have caught it (it updates action *refs*, not the inputs passed to
  them, and nothing it watches covers an OS package — `.github/dependabot.yml`
  now records that where someone would look). The major is now declared once in
  **`web/.nvmrc`** and read by CI (`node-version-file`), `bootstrap.sh`,
  `doctor.sh`, `update.sh` and `web/package.json`'s `engines`. Raised to **24**,
  the Active LTS line (22 is maintenance-only). Two traps handled along the way:
  Fedora's node packages are **parallel-installable**, so `dnf upgrade nodejs`
  can never cross a major — the reason a box sat on 22 through repeated doctor
  runs — and installing `nodejs24` leaves `/usr/bin/node` pointing at the older
  default stream, so a hardcoded `ExecStart=/usr/bin/node` would keep serving
  the old major. systemd cannot fix that either: it does not expand a variable
  used as the executable (`ExecStart=${FLEET_NODE_BIN} …` is a literal path).
  `ExecStart` therefore names `deploy/fleet-web-start.sh`, a shim that resolves
  the interpreter and **`exec`s** — the cgroup still holds exactly one process
  (verified: same pid, SIGTERM → `exit(143)` in 15 ms), so this is not the npm
  wrapper returning; npm's fault was *lingering* as a supervisor. bootstrap,
  doctor and update install the shim and stamp/assert `FLEET_NODE_BIN` so the
  tier runs the node that was installed rather than whichever one the distro
  defaults to. Installing the versioned `nodejs<major>` package is bootstrap's
  and doctor's job — update is an updater, not a provisioner, so it hands a
  shortfall to `doctor.sh --node` and re-resolves (see below). `rampart-service`
  and the EKS deployment examples move to `node:24-slim` too. Verified on node
  24.19.0: `npm ci` clean, lint 0 errors, build clean, 1080/1080 web tests
  (identical with and without this change), and five start→SIGTERM cycles all
  `exit(143)` with no segfault. **Not** verified: that node 24 fixes the
  residual teardown segfault — that fault does not reproduce off the affected
  box, so it stays an open operator experiment.
- **`boxdoctor` can now see a crash loop and a directive that did not take** —
  the admin-facing doctor (Settings → Admin → Doctor, `/admin/doctor`) probed
  units with `is-active` only, and both app units run `Restart=always`, so a
  unit segfaulting in a loop is `active` again ~5s later and the panel stayed
  green throughout. That blind spot is why the `fleet-web` shutdown crashes went
  unnoticed for three days. Two checks added: `restarts: <unit>` reads
  `NRestarts` for the app units (0 ok / 1–4 advisory / ≥5 failure) — the right
  property because it counts only `Restart=`-driven restarts and a manual
  `systemctl restart` clears it, so deploys raise no false alarm; and
  `fleet-web stop policy` asserts the value systemd *resolved* for
  `TimeoutStopFailureMode`, the check that would have caught a directive sitting
  inert in the unit body for a release. Stated honestly in `docs/DOCTOR.md`:
  restart churn would **not** have caught the fleet-web crashes themselves,
  since those happened on operator-initiated stops that reset `NRestarts` — the
  stop-policy check is the one that covers that fault. Both are read-only and
  unprivileged, and the non-`kill` verdict is a warn here where `doctor.sh`
  fails, because only `doctor.sh` attempted a repair first.
- **`fleet-web` shutdown: verify the resolved directive, not the file** — a
  follow-up to the two fixes above, which had a shared weakness: each one
  asserted a systemd directive and never checked what systemd actually
  resolved. That is how `TimeoutStopFailureMode=kill` shipped inert on Fedora
  in the first place, and comparing *files* (the first guard) cannot see a
  stale checkout, a drop-in installed to the wrong directory, a later-sorting
  drop-in in the same directory, or a missing `daemon-reload`. `scripts/doctor.sh`
  now asserts `systemctl show -p TimeoutStopFailureMode --value fleet-web` is
  `kill` after any reload, failing with a pointer to `systemctl cat fleet-web`
  when it is not (empty = pre-246 systemd, an advisory rather than a failure).
  Two install-path bugs fixed alongside it: `bootstrap.sh` installed the drop-in
  only when `fleet-web.service` was **absent**, so a re-run on a box
  provisioned before the drop-in shipped — the exact case that needs it — never
  got it; and `update.sh`'s drop-in check compared bytes where the unit path
  deliberately compares functional lines via `unit_functional_body()`, so
  editing the drop-in's header (11 of its 13 lines are comments) would have
  claimed Fedora's `abort` drop-in was overriding us on a box where it was not.
  Also corrects an overclaim in `doctor.sh` and `docs/WEB-TIER-SHUTDOWN.md`: a
  missing drop-in does **not** bring back the 130 MB-per-restart dump pile —
  `LimitCORE=0` suppresses dumps for every signal on its own, since
  systemd-coredump stores one only when the process's `RLIMIT_CORE` is
  sufficient. The drop-in decides how an overrun stop dies (SIGKILL, not
  SIGABRT), which is a correctness and hygiene matter, not a disk one.
- **`fleet-web` no longer dumps core on nearly every restart** — see
  [docs/WEB-TIER-SHUTDOWN.md](docs/WEB-TIER-SHUTDOWN.md). ~793 MB of `node-22`
  SIGSEGV dumps had accumulated in `/var/lib/systemd/coredump`, one or two per
  deploy, all at service *stop* — never mid-request — so `Restart=always` kept
  the tier serving and disk was the only symptom. Three separate faults:
  (1) `ExecStart` was `/usr/bin/npm run start`, putting **two** processes in the
  cgroup, and on stop npm forwarded SIGTERM to a child it had already reaped —
  segfaulting in `uv_kill` → `node::Kill` → `ProcessWrap::OnExit`. The unit now
  runs `node node_modules/next/dist/bin/next start` directly, which deletes that
  crash and lets SIGTERM reach next-server unrelayed; the loopback-bind default
  that lived in `web/package.json`'s shell `${FLEET_WEB_HOST:-127.0.0.1}` moves
  into the unit as `Environment=FLEET_WEB_HOST=127.0.0.1`, placed before
  `EnvironmentFile=` so the env file can still override it (`next start` with no
  `-H` binds `0.0.0.0` and would expose :3000 past Caddy). (2) A stop that
  overran `TimeoutStopSec=30s` was **SIGABRT**ed, because Fedora's
  `systemd-system.conf` drop-in sets `TimeoutStopFailureMode=abort`; the unit now
  states `TimeoutStopFailureMode=kill`, so an overrun is a journal entry instead
  of a memory image (the rare overrun's own cause remains unknown). (3) The
  residual dump — `next-server` SIGSEGV at `0x0` in V8/libuv **teardown**, after
  it has stopped serving — is not fixable here: Next 16.3.1/16.3.2 contain no
  shutdown fix, so `next` deliberately stays at 16.3.0 rather than implying one,
  and the lead is the node build (the same Next build exits cleanly under node
  22.22.2; the crashing box runs 22.23.1), making it an operator action.
  Separately, the unit now sets **`LimitCORE=0`**: a core dump is a full memory
  image, and this process holds `CHAT_SERVER_TOKEN`,
  `ORCHESTRATOR_SERVER_TOKEN`, `APP_SESSION_SECRET` and every in-flight user's
  session, so persisting one per crash wrote down exactly what the credential
  invariant says must never reach disk. The failure is still logged — only the
  image is declined. Also recorded in
  the design note: the obvious **graceful-drain theory was measured and refuted**
  (Next 16.3.0 exits in ~105 ms with an open-ended SSE response in flight), so
  no application-side stream-abort machinery was added for a hang that does not
  exist.
- **`fleet-web` shutdown fix completed on live verification** — two gaps found
  when the unit above met a real Fedora 44 box: (1) Fedora's
  `TimeoutStopFailureMode=abort` lives in the **global**
  `/usr/lib/systemd/system/service.d/` drop-in directory, and drop-ins are read
  after the unit body, so it overrode the unit's own
  `TimeoutStopFailureMode=kill` — the fix now also ships
  `deploy/fleet-web.service.d/10-timeout-kill.conf`, a per-unit drop-in at the
  precedence level that wins, and `bootstrap.sh`, `update.sh` (`--adopt-units`)
  and `doctor.sh` all install/reconcile it; (2) Next handles SIGTERM and exits
  with code **143** rather than dying by the signal, so systemd logged every
  clean stop as "Failed with result 'exit-code'" — the unit now declares
  `SuccessExitStatus=143`, which names Next's deliberate clean-stop code only
  (crash signals still fail loudly). Verified live on fleetdev: `systemctl
  restart fleet-web` now stops with "Deactivated successfully", no core dump,
  no segfault, and the tier is Ready again in ~150 ms.
- **Approval cards reworked end to end** — see
  [docs/APPROVAL-CARDS.md](docs/APPROVAL-CARDS.md) for the full design note.
  The pieces: (1) non-email critical tools (a pages deploy, a deal write) get a
  **generic action card** — honest verbs, the tool's own arguments, "ran
  without asking" chrome for notify-mode records — instead of falling through
  to the email card ("Send this email?" / "Email sent ✓" for a page publish);
  their decline history now names the tool instead of claiming an email was
  involved. (2) **Resolved cards survive reload**: the conversation GET returns
  `resolved_approvals` and the client re-anchors each card to the message that
  staged it, which also makes notify mode (#1153) keep its own promise — the
  "ran without asking" record and its bundle-authored undo hint now reach the
  away-from-page user it exists for, not just a live SSE stream nobody was
  watching. (3) **`preview_email` no longer expires** (display-only; nothing to
  deny) — previews used to auto-deny after the window and tell the model "the
  action was not taken" about an action that never existed. (4) The **approval
  timeout default is now 3600s** (was 300s — measured from whenever the agent
  staged the card, five minutes mostly denied the final, wanted action of a run
  the user had stopped watching) and is **admin-settable live** from Settings →
  Admin → Features (`approval_timeout_seconds`, 60s–24h; per-tool bundle
  windows and the per-conversation override still win). (5) A click on an
  **expired card resolves it as timed out immediately** instead of silently
  resetting to pending until the next sweep tick, and timed-out cards offer a
  one-click **"Ask again"** that submits a user turn asking the agent to
  re-stage.

### Added

- **A `?connector=<name>` deep link into Settings → Connections**, and a
  Featured-shelf spot for the Browserbase card. `/settings/connections?connector=browserbase`
  opens the connector directory filtered to that entry with its guided API-key
  form already open and focused — one paste and Add away from connected — then
  strips the one-shot param like the OAuth callback params. Works for any
  directory entry by name; the built-in `browserbase` skill hands users the link
  when a chat has no connector, and `docs/BROWSERBASE.md` documents it. Covered
  by page unit tests (the first for the connections page) and a mocked
  Playwright spec asserting the POST carries `api_key_query`.
- **Sandbox image freshness: a max-age rebuild backstop in `fleet update`**
  ([`docs/SANDBOX-IMAGE-FRESHNESS.md`](docs/SANDBOX-IMAGE-FRESHNESS.md)). The
  update gate only ever detected *change* (Containerfile hash, resolved tag,
  tag missing from the store), so a healthy box with a stable bundle served
  the same sandbox image for months — weeks-old base layers and unpatched
  package CVEs on boxes that updated regularly. `fleet update` now also
  rebuilds when the installed image is `FLEET_SANDBOX_MAX_AGE_DAYS` (default
  7; `--sandbox-max-age <d>` per run; `0` disables) or more days old, and an
  age-triggered rebuild runs `--no-cache` so the package layers actually
  refresh instead of replaying the stale cache into an identical image. Every
  sandbox build (`build-sandbox-image.sh`, and `bootstrap.sh`'s inline
  systemd-path build) now passes `--pull=newer`, so an unpinned base like the
  generic bundle's `fedora-minimal:latest` is re-checked against the registry
  on each build instead of silently reusing the stale local copy (offline-safe:
  podman suppresses the pull error when a local base exists; digest-pinned
  bases are unaffected). `build-sandbox-image.sh --no-cache` /
  `FLEET_SANDBOX_BUILD_NO_CACHE=1` is available for manual full refreshes.
  Bundles that pin a prebuilt `sandbox.image` still skip the on-box build
  entirely — registry freshness stays the publisher pipeline's job.

  Two follow-up fixes close the paths on which that backstop reported success
  without running (same design note). (1) **The self-update re-exec now
  forwards flag state.** When an update changes `update.sh` itself, the script
  re-execs the freshly pulled copy — with no argv, so every flag-settable
  value has to be restated as an env var on that line. `--sandbox-max-age` was
  not, so it was silently downgraded to the default `7` on exactly the run
  that pulled the fix; `--adopt-units` and `--no-timers` were dropped the same
  way. All three are forwarded now, the deliberate non-forwards are documented
  at the call site, and a new test derives the flag-settable variables from the
  arg parser so a *new* flag fails until it is forwarded or exempted with a
  reason. (2) **An unresolvable sandbox tag no longer reports "up to date".**
  Both store-aware gates need a resolved ref, so an empty one used to fall out
  of the gate's if/elif chain with no build reason and print
  `sandbox image up to date (…, tag unresolved)` — a pass for an image nothing
  had looked at. It now warns, naming what was skipped (the presence check and
  the n-day backstop), how to diagnose the tag, and the by-hand rebuild; an
  inconclusive store probe reports through that same single path instead of a
  warning followed by a reassuring `ok`; and `--print-tag`'s stderr is kept so
  the warning can name the cause. Neither case is fatal — the fail-closed
  `die` stays on a failed *build* whose ref is not known to exist.

- **`fleet timers install` — one-command setup for the scheduled-maintenance
  timers** ([docs/TIMERS.md](docs/TIMERS.md)). A box provisioned before the
  `fleet-backup` / `fleet-maintenance` timer pairs shipped had no path to them
  except copy-pasting a four-command hint out of `fleet doctor`. The new verb
  installs the missing units from `deploy/`, creates the 0700 backup
  directory, daemon-reloads and `enable --now`s the timers — idempotently, and
  without ever overwriting an already-installed unit (drift stays
  `doctor`/`update --adopt-units` territory). Doctor's absent-pair advisories
  (script and in-process) now hand the operator that one command; an
  interactive `fleet update` offers the install when a pair is fully missing
  (default No; `--no-timers` / `FLEET_UPDATE_OFFER_TIMERS=0` silences the
  offer for boxes that deliberately run without them), and update's
  unit-drift adoption now covers the `fleet-maintenance` pair too. On a host
  without systemd (a container platform, Kubernetes) the verb explains what
  to schedule instead — daily `fleet backup --db=all --prune` and `fleet
  cleanup` — and exits non-zero rather than pretending.

- **One maintenance loop, and a disk guard that acts on what it measures**
  ([`docs/MAINTENANCE.md`](docs/MAINTENANCE.md)). Reclamation used to be a side
  effect of chat traffic: the database retention sweeps, the attachment sweep
  and the orphan-workspace sweep ran only at the tail of a completed chat turn.
  An idle box — a scheduler-only deployment, or any box whose chat went quiet —
  therefore grew its turn ledgers, expired conversations, terminal input-queue
  rows and orphaned workspace dirs without bound, while a busy box ran the same
  global sweeps once per turn, inline on the turn goroutine. Now an hourly
  in-process loop runs the pass (plus the orchestrator temp-upload cleanup, the
  remote-MCP OAuth flow sweep, and the git-worktree reclaimer), and the
  post-turn path is the same pass behind a compare-and-swap rate gate
  (`FLEET_MAINTENANCE_MIN_INTERVAL`, default 5m) so concurrent turns cannot
  stampede it. Two reclaimers that previously had no automatic caller at all —
  `fleet worktree prune` and `fleet cleanup` — are now driven: the worktree
  sweep from the loop (`FLEET_WORKTREE_PRUNE_AGE`, default 24h), and the podman
  image prune from a new `fleet-maintenance.timer` that `bootstrap.sh
  --enable-service` installs by default (`--no-maintenance-timer` opts out;
  image pruning stays out of the serving process on purpose).
- **Disk backpressure** (`internal/diskguard`, `FLEET_DISK_MIN_FREE_PERCENT`,
  default 5). Below the floor the scheduler stops *claiming* tasks and `/readyz`
  reports the `disk` check degraded (207, never a 503 — `/healthz` stays 200
  because draining the box would remove the chat interface an operator needs to
  reclaim the space); interactive chat is never gated and running tasks
  are untouched — a full disk is nearly always made by unattended runs, and chat
  is how an operator fixes it. Fails OPEN (an unmeasurable filesystem never
  sheds) and has a 2-point recovery margin so it cannot flap. Exposed as
  `fleet_disk_{total,free}_bytes`, `fleet_disk_free_ratio` and
  `fleet_disk_shedding`, charted in a new **Host resources** row on the Grafana
  dashboard, and checked by `fleet doctor` alongside the new maintenance timer.
- **Go runtime gauges** `fleet_goroutines`, `fleet_memory_heap_bytes` and
  `fleet_memory_sys_bytes`. Both numbers already existed in the admin health
  JSON, where the shape that actually matters — a count climbing across a day —
  is invisible. `GOMEMLIMIT` guidance added to `deploy/fleet.service`.

### Fixed

- **The guided api_key connector add was broken for query-auth servers:**
  `GET /mcp-catalog` dropped `api_key_query` on the wire (the projection in
  `internal/httpapi/mcp_catalog.go` never copied it), so the directory card's
  paste-a-key form sent the key as a bearer header and the add-time probe
  failed against servers — Browserbase among them — that take the key as a
  query parameter. The field now survives to the wire, pinned field-by-field
  by a projection test.
- **Browserbase live-view hardening after #1220's merge review.** The key
  resolver treated a nil per-conversation opt-in list as "no filtering" while
  the overlay treats the same nil as "no connectors on", so a conversation
  whose opt-in list was never seeded could register `browserbase_live_view`
  and unseal the connector key with every connector switched off — nil now
  gates exactly like empty. Also: the connector URL match survives an explicit
  `:443` (compare `Hostname()`, not `Host`); a 401/403/404 on the debug
  endpoint no longer blames a two-key project mismatch when the user's own
  connector key made the request; session auto-resolution returns the trimmed
  id it validated; and the per-call HTTP client closes its idle connections
  instead of leaking one TLS connection per mint.
- **`paused_awaiting_wake` had no terminal backstop.** `WakeDueTasks` filters on
  `wake_at IS NOT NULL`, so a row without one could never wake, and
  `ExpirePausedTasks` covers `paused_awaiting_input` only — such a task waited
  forever with no terminal record and no operator signal. `ExpireStrandedWakeTasks`
  now fails rows that are unreachable (NULL `wake_at`) or more than 24h past
  their deadline, anchored on `paused_at` so a legitimate 30-day sleep is never
  touched, and running after the wake sweep so a merely-due row is woken rather
  than expired. Preserves the recurrence chain, like the awaiting-input expiry.
- **The persistent-sandbox session cap is now enforced by the idle reaper**, not
  only on the create path. Eviction skips sessions with a turn in flight, so a
  burst that overshot the cap while every other session was busy stayed
  overshot until the idle TTL expired or another create happened to arrive —
  and that cap is what bounds the box's container memory. It remains a soft cap
  (a busy session is never evicted) but an overshoot now self-corrects on the
  next tick. fleet also warns at boot when `FLEET_PYTHON_REPL_MAX` ×
  `FLEET_SANDBOX_MEMORY` claims more than two thirds of host RAM — the defaults
  multiply out to 16 GiB.
- **The remote-MCP OAuth flow sweep outlived shutdown.** It was a
  `for range ticker.C` goroutine using `context.Background()` per sweep, so it
  was unreachable by the shutdown context and could touch a closing store. Folded
  into the maintenance loop, which is ctx-bound.
- **`/admin/storage` walked the workspace tree twice and ignored cancellation.**
  The panel sized the tree once for the total and again, directory by directory,
  for the largest-workspaces rows; both walks ran to completion even after the
  client gave up. Now one pass yields both, and the walks check `ctx`.
- **Three copies of the same `statfs`** (`hoststats`, `admin_storage`,
  `boxdoctor`) could drift. `diskguard.Usage` is now the single implementation,
  so what is charted, what is reported and what is enforced always agree.

- **Corrected what the sandbox publisher's docs said about GHCR package linking
  and visibility.** Two claims were wrong and both are now measured rather than
  reasoned. *Linking*: the workflow header and all five caller workflows said a
  first publish of a new image name needs its package linked to the repo by hand
  or the push is denied. It does not — the pushing repo creates and links a new
  package automatically, verified by two first-ever pushes on 2026-08-20
  (`ghcr.io/elcanotek/fleet-sandbox` from this repo, `…/fleet-sandbox-example`
  from example-config), both clean with no package settings touched. That claim
  was over-generalised from fleet's six red June runs, which are a *different*
  case: they targeted `ghcr.io/elcanotek/sandbox`, a package that already existed
  at the org level and was not linked to the repo, hence
  `denied: permission_denied: write_package`. *Visibility*: a package published
  here takes the visibility of the repo that published it — the images from the
  public `fleet` and `example-config` repos are anonymously pullable, those from
  the private `elcano-config` and `reklaim-config` are not. GitHub's own docs
  describe the opposite (private by default, with access permissions "but not
  the visibility" inherited), so an org-level setting may be responsible and the
  docs now record this as observed behaviour in this org with an explicit
  instruction to check a new package's visibility rather than assume it. The
  deployment docs carry the operational consequence: a public image needs no
  pull credentials, a private one means the box or cluster authenticates to
  GHCR. None of these images carries anything sensitive regardless — no shipped
  Containerfile has a `COPY` or `ADD` — but a public `fleet-sandbox-<client>`
  package name discloses that client as a customer, which is the reason the
  private-repo default matters.

- **A chat socket that dies while the agent is *thinking* is now caught in
  about a minute, and every parallel chat is watched, not just the one on
  screen.** These were the two limitations left open by #1211's stream
  recovery; both are closed.

  **Silence is now evidence.** Detection previously needed the server to have
  emitted events the client never received — which never happens while the
  agent sits in a long tool call, because the server has nothing to emit. A
  socket dying in that stretch stayed invisible until the five-minute idle
  timeout. The keepalive doesn't care what the turn is doing: an attached
  stream writes at least one byte every interval, so missing several in a row
  proves the connection is gone.

  Using that required the client to know the cadence, so **the server now
  advertises it** — `X-Fleet-Heartbeat-Interval-Ms` on the stream response and
  `heartbeat_ms` on the `fleet.capabilities` frame, the two halves of the
  existing discovery surface. Guessing would have been wrong in both
  directions: too short kills healthy streams on a slow deployment, too long
  leaves the blind spot open. Keepalives disabled is advertised honestly as
  `0`, and the client then treats silence as proving nothing rather than
  assuming a cadence it will never be sent. The threshold is four intervals —
  a full minute at the default — because a few keepalives can be lost to a GC
  pause without the socket being dead, and the confirming grace window still
  has to agree before anything is torn down.

  **Background conversations are covered.** Chats stream in parallel and the
  sidebar paints a working dot for each, but the liveness check only ever
  looked at the active conversation — a background chat's socket died the same
  way with nobody watching. The check now sweeps every attached conversation,
  with per-conversation failures contained so one bad chat cannot abort the
  sweep. Both the sweep and the watchdog stay no-ops while the tab is hidden,
  where timers are throttled and sockets may be legitimately frozen.

  Net effect: on a visible tab with default settings, a dead socket is found in
  roughly a minute instead of five — or inside the 2.5s grace window if you're
  returning to the tab — for any chat, whether or not the agent is mid-tool-
  call. Detection latency, and the one case that still falls through to the
  idle timeout, are in
  [`docs/CHAT-STREAM-RECOVERY.md`](docs/CHAT-STREAM-RECOVERY.md).

- **The sandbox publisher derives its own destination, and lowercases it.**
  `image_name` was a required input, so a manual dispatch meant typing a full
  image ref and every client bundle hand-wrote one. It did not need to be:
  the bundle already names itself in `sandbox.tag`
  (`localhost/fleet-sandbox-<client>:latest`). That tag is a *local* podman tag
  and so not itself pushable, and the manifest's other ref, `sandbox.image`, is
  the *consume* side and must stay empty (that emptiness is the build-on-box
  default) — which is why a destination had to come from somewhere. But its
  basename is the one client-specific token needed, so the destination is now
  derived as `ghcr.io/<owner>/<basename of sandbox.tag>`, verified
  byte-identical to what all five bundles used to pass by hand. Two real bugs
  stop being expressible: `omnicom-config` published to `fleet-sandbox-example`
  purely because the string was hand-copied from its example-config fork, and
  the recommended `ghcr.io/${{ github.repository_owner }}/...` form is an
  **invalid OCI reference** for an owner with capitals — `github.repository_owner`
  preserves account casing (`ElcanoTek`), and OCI repository names must be
  lowercase, so podman rejects it before the registry is ever contacted. The
  callers only worked because they hardcoded a lowercase owner; the derived name
  is normalized with `tr`, and an explicit override is normalized too. The
  resolver reads inputs from env rather than interpolating them into the script
  body, and fails closed on an empty derivation, a name carrying a tag (which
  would collide with the `{sha}`/`:latest` this workflow adds), or any character
  outside a plausible tagless ref. `image_name` remains as an optional override
  for a non-GHCR registry, e.g. the ECR ref in `docs/EKS-DEPLOYMENT.md`. The
  concurrency group moves from `image_name` to repo + `bundle_dir`, because an
  omitted `image_name` would otherwise collapse every publish into one group;
  the derived name is a function of exactly those two things, so two bundles in
  one repo still publish concurrently while two pushes of one bundle serialize.

- **A long-running chat turn that is *still working* when you come back now
  resumes streaming immediately, instead of showing a thinking indicator that
  never moves.** This completes the walk-away recovery of #1208 (the entry
  below), which fixed the case where the turn had already finished but
  deliberately left the still-generating case to the five-minute idle timeout.

  The problem in both cases is that **"attached" is not the same as "alive"**:
  a phone that locks mid-turn leaves a socket whose reader neither delivers
  another chunk nor rejects, so the conversation goes on looking attached long
  after its connection is gone — and every recovery path that short-circuits
  on "already attached" does nothing.

  A liveness check now runs on tab return and on a 10s watchdog while the tab
  is visible. When the turn has finished it adopts the persisted transcript, as
  before. When the turn is **still generating** it replaces the dead socket and
  resumes the stream from the last event the client actually applied, so the
  partial answer already on screen is kept, nothing is replayed twice, and
  tokens start landing again.

  Telling a dead socket from a merely quiet one is the whole difficulty, and
  the proof deliberately does not depend on the (operator-configurable)
  heartbeat: the server must report having emitted *past* the client's last
  applied event id, **and** the socket must then produce no bytes at all during
  a 2.5s grace window. A socket that was only frozen flushes as soon as the
  page thaws; a severed one never does. A genuinely stalled turn — a long tool
  call — fails the first condition and is left alone. Being wrong is cheap by
  construction: the replacement resumes from the applied event id, so the cost
  of a false positive is one reconnect, never a lost or duplicated token.

  Retiring the old socket needed care, because both stream owners treat an
  ending socket as an ending turn: `submitPrompt` reads an abort as the user
  pressing Stop, and both teardown paths release the conversation's handles and
  settle its assistant slot. A superseded-stream marker now tells them a
  replacement owns the conversation, so the abort lands as a handover rather
  than a cancellation or a "connection dropped" failure.

  The watchdog is nearly free on a healthy stream: a silence gate means an
  unforced tick on a socket that has produced bytes recently reads a ref and
  returns without spending a request. The five-minute idle timeout is kept as
  the backstop. Design note and honest scope:
  [`docs/CHAT-STREAM-RECOVERY.md`](docs/CHAT-STREAM-RECOVERY.md).

- **Walking away from a long-running chat turn no longer comes back as "the
  assistant finished without a written reply".** Lock your phone mid-turn and
  the OS severs the TCP socket while fleet keeps generating; the turn finishes,
  the transcript lands in Postgres, and the browser hears none of it. That part
  was always fine. What wasn't: every finalizer in the chat turn loop settled
  the orphaned assistant message by stamping `state: "done"` in place, turning
  *"we don't know how this ended"* into a terminal success with no content. The
  transcript then told the user the assistant had said nothing — while the full
  answer sat in the database, which is why refreshing the page always fixed it.

  A finalizer may now only claim an outcome it actually observed. Everything
  else reconciles against Postgres first — programmatically doing what the
  user's manual refresh did — and adopts the canonical transcript when it
  already answers the turn being held open. A guard on user-turn coverage keeps
  that from adopting a *stale* transcript whose last reply belongs to the
  previous turn. Only a turn the database has no answer for is settled locally,
  and then honestly: any partial answer is kept, the message is marked failed,
  and Retry is offered. A slot waiting on a pending approval or memory proposal
  is left alone — it's blocked on the user, not the network.

  Two supporting fixes. On tab return, a conversation that merely *looks*
  attached is no longer skipped: a phone that locked leaves a zombie socket
  that never delivers another byte and never errors, so the old early-out meant
  a completed task showed a stuck thinking indicator until the multi-minute
  idle timeout fired. It now probes, and adopts the persisted answer as soon as
  the turn is provably over. And a turn whose events outran the server's
  sliding replay window no longer reports "No response returned." on an empty
  slot — a replay gap means the answer was missed, not absent, so it is
  reconciled too.

  A turn still *generating* when you return is unchanged: it shows a truthful
  thinking indicator and resumes as before. Design note and honest scope:
  [`docs/CHAT-STREAM-RECOVERY.md`](docs/CHAT-STREAM-RECOVERY.md).

### Added

- **The agent can hand you a real browser when a page needs a human (#987).** Ask it to
  do something on a site with no API and it drives a hosted Browserbase session; when it
  hits a login form, a captcha or a 2FA prompt it posts a **live-view link**, stops, and
  waits. You open the link, take over the very same browser, finish the sign-in, and
  reply — the agent picks up in that session and carries on.

  Two pieces ship: the `browserbase` built-in skill (the seventh in the pack) and a
  host-side `browserbase_live_view` tool. The tool exists because the hosted MCP's
  `start` returns only a session id — the viewer URL comes from an authenticated
  Browserbase API call, and credentials never enter the sandbox or the model context, so
  minting it has to happen host-side. It joins the "host network / brokered fetch"
  exception class ADR-0036 already enumerates, alongside `web_fetch` and `download_url`;
  it drives no browser, so ADR-0044's "browser automation is a connector" still holds.
  This is the follow-on that ADR named when it removed the in-sandbox browser tool.

  **One key, in one place:** add Browserbase under Settings → Connections and enable
  it in a chat's Tools picker. The link-minting tool resolves the running user's own
  connector credential host-side, so the same key that drives the browser mints the
  link — and the capability is scoped to that user rather than the whole box. A
  box-wide `BROWSERBASE_API_KEY` remains as a fallback for paths with no per-user
  connection (scheduled runs), installed with the new guided writer
  `sudo fleet config set-browserbase-key` beside `set-openrouter-key`. The tool is registered
  per turn **only when a credential is actually reachable** — you have a Browserbase
  connection and this chat has it switched on, or the operator set the box-wide key — so
  everyone else sees no new tool at all, and the credential stays inside the same
  per-conversation gate the connector's own tools obey. Where it is absent the skill falls
  back to telling you to open the session from the Browserbase dashboard, which uses your
  own login and needs nothing from fleet.

  Two safety details worth knowing. With no `session_id` the tool resolves your running
  session itself — but only when the key came from **your own** connection; a shared
  box-wide key can see every session in a shared project, so it demands an explicit id
  rather than risk minting a control link to someone else's logged-in browser. And
  `BROWSERBASE_API_KEY` is now a parent-owned env name, so a client bundle declaring the
  same key for its own MCP connector will **fail server boot** with a clear overlap error
  instead of having the value silently scrubbed — deliberate, but a behaviour change for
  such a bundle.

  One thing to know before you rely on it: **the live-view link is a capability.** It
  needs no password, so anyone holding it can drive that browser until the session ends,
  and fleet cannot revoke it — the skill says so and tells the agent not to forward it.
  The connector's catalog URL also gained `?keepAlive=true`, which upstream documents as
  what keeps a session alive across a disconnect; in testing a connection predating that
  flag survived the handoff anyway, so it is belt-and-braces rather than a proven
  dependency, and an existing connection only needs re-adding if handoffs actually lose
  the session.

  Honest scope: the skill is discoverable in interactive chat only (bundle skills are not
  in the scheduled-run roster, so the tool's own description carries the protocol for
  headless runs); the handoff is two turns by construction, because interactive chat
  cannot block on a human; there is no embedded viewer, since the web proxy's CSP forbids
  framing; and only a manual run with a real key exercises the live API call. Full design
  note, including why the key is separate from the connector's own credential, in
  `docs/BROWSERBASE.md`.

- **A reusable sandbox build canary (`build-sandbox-image.yml`) — build only, no
  push.** Every bundle's sandbox image derives from exactly two inputs: the
  `FROM` base and the Containerfile's own `RUN` lines. `build-sandbox-image.sh`
  sets the build context to the Containerfile's own directory, and no shipped
  Containerfile has a single `COPY` or `ADD`, so nothing outside
  `<bundle>/sandbox/**` can change the image — which is why the publisher's
  trigger is `paths: ["sandbox/**"]` rather than every push. But every shipped
  Containerfile tracks `fedora-minimal:latest` unpinned, so image content drifts
  with *time* rather than with commits: if Fedora renames or drops a package one
  of those `RUN` lines installs, the build breaks. That build is not optional —
  `fleet bootstrap` / `fleet update` runs the same script on the box, which is
  what every deployment does by default, since every bundle ships
  `image: "${FLEET_SANDBOX_IMAGE:-}"`. No client bundle's CI had ever built its
  Containerfile (gitleaks + ruff + pytest only, and three of the five had no CI
  at all), so the breakage would have surfaced on a customer's box at deploy
  time. Callers get a weekly `schedule:` plus a `pull_request` trigger on
  `sandbox/**` — the latter matters as much, because the publisher fires only on
  push to `main`, so a Containerfile edit was previously never built until after
  it merged. fleet's own `config/default` deliberately does not call this: the CI
  gate builds that sandbox on every PR via the live Playwright job, and
  `grype-scheduled.yml` rebuilds and scans it every Monday. This is
  build-only on purpose — a weekly ~1.3 GB push per bundle to GHCR would have no
  consumer today, since nothing pins `sandbox.image` and so nothing pulls;
  scheduled *publishing* is worth adding to a bundle at the point it gains a real
  puller (a Kubernetes deployment, or a box setting `FLEET_SANDBOX_IMAGE`), when
  a stale digest becomes a live exposure. It therefore requests only
  `contents: read` — no registry login, no `packages: write`, no repo writes.

- **A built-in `bento-slides` skill: the agent can now produce a real,
  downloadable presentation offline (#985).** A [Bento](https://bento.page) deck
  is one self-contained `.bento.html` file that is simultaneously the slides, the
  viewer and a full editor, with the document stored as plain JSON in a single
  `#bento-doc` script block. Ask for a deck in chat and the agent writes one into
  the workspace; the user downloads it and opens it in any browser — no Gamma, no
  PowerPoint, no PPTX toolchain, and no network call at all, either to author the
  deck or to open it.

  **Decks are offline-only, by construction.** Upstream has two behaviors that
  would make a delivered deck a network client, and both are disabled. The first
  is an update check to `bento.page` on every launch: fleet embeds and
  sha256-pins the shell, so it can only report a version the reader cannot
  install, while telling a third party they opened the deck. The second matters
  more — live collaboration. `bornWithCollab = !!doc.collab` is the entire
  eligibility test, so a deck that merely *carries* a collab block opens a
  `wss://sync.bento.page` session the moment it is opened, with no click, and
  retries on failure. Such a file is a live, writable door into whoever opens it,
  and #1197's `set` deliberately restored those keys.

  `new` now plants two layers ahead of the runtime: a CSP `<meta>` with
  `connect-src 'none'` — enforced by the browser, so it holds without the app's
  cooperation, without `localStorage`, and even against markup a model wrote into
  a slide — and upstream's own offline switch, so the app refuses network at its
  own chokepoints and never attaches a session instead of retrying into the CSP.
  The CSP also blocks iframes, plugins, form posts and remote images, making
  several of the skill's authoring rules browser-enforced rather than remembered.
  A third layer sits in the document: `set` now **removes** any `collab` block,
  from the target file or the incoming document, and says so on stderr — dropping
  keys does not retract an invitation already shared, and only the user can decide
  to rotate.

  **Hand-off is the deck plus a PDF.** The deck's own *Export PDF (print)* button
  is the pixel-exact route — the same renderer the reader is looking at, with
  selectable text and embedded font subsets (measured: five pages, 46KB,
  `/ToUnicode` present) — and the skill names it whenever a page has to match
  exactly. The agent can now also produce a PDF itself, without a browser, so it
  can attach one in the same turn; see the `bento_doc.py pdf` entry below. There
  is no
  PowerPoint export and the skill says so plainly rather than implying a
  conversion exists; hand-rolling one would mean a second renderer for seven
  element types, six shape kinds, connectors, gradients, the motion effects,
  morph, state slides and layouts, and a deck that is almost right is worse than
  an honest PDF. A test inflates the vendored runtime and checks both claims
  against the app's own strings, so a re-vendor that renames the button or adds a
  PPTX path fails CI instead of leaving the instructions quietly wrong.

  Verified by hand in Chromium with the page instrumented and every request
  intercepted: an unguarded shell carrying a collab block attempts the session
  socket five times and fetches the update manifest; a fleet deck attempts
  neither; and with `localStorage` denied so only the CSP is left, the app tries
  both and the browser refuses both. The vendored template stays byte-identical
  and sha256-pinned (the guard goes into the produced deck, not the template), and
  a deck the *user* hands the agent keeps its shell — `validate` reports an
  unguarded one rather than rewriting it. Honest residue, recorded in
  `templates/NOTICE.md`: the app mints a collab block into files it saves through
  its own UI, so such a deck carries inert key material even though both guard
  layers survive the save and nothing connects.

  The pack ships in `internal/clientconfig/builtin_skills/bento-slides/` and
  needed **no Go change**: `//go:embed all:builtin_skills` is recursive, so its
  `templates/`, `references/` and `scripts/` materialize into the merged skills
  dir and land on the sandbox's read-only mount exactly like
  `data-profiler/scripts/profile.py`. It shows up in Settings → Skills with a
  **Built-in** badge and answers to `/bento-slides`, both for free.

  Two deliberate departures from the issue's sketch. The template is the
  **upstream v1.0.18 release artifact, vendored unmodified** (689KB, MIT, ©
  The Bento authors) rather than a "minimal" one, because a Bento shell *is* the
  application — there is no smaller file that opens. And instead of editing the
  minified bundle directly, the pack ships a stdlib-only `scripts/bento_doc.py`
  (`new` / `get` / `set` / `validate`): the document block sits at byte 6322, so
  reaching it with `view_file` would burn ~125KB of context on runtime code, and
  the block's escaping rule for `<` is the kind of thing that corrupts a file
  silently rather than loudly. The helper also turns two format-and-safety rules
  into mechanisms instead of reminders — `get` keeps a shared deck's `collab`
  live-session private keys out of the model context while `set` restores them
  from the file untouched, and a deck's `docId` is carried across an edit rather
  than regenerated. Every refusal leaves the deck byte-identical, and the app
  shell outside the document block is preserved exactly.

  Honest scope: discovery is **interactive chat only**. `internal/scheduledrun`
  composes its own prompt and emits no bundle-skill roster, so scheduled tasks
  and `fleet task run` do not see this skill (true of every bundle skill, not new
  here — `docs/SKILLS.md` now states it plainly instead of implying otherwise).
  Decks are always delivered as a download, never rendered inline, so the skill
  requires a new filename per revision — workspace downloads are cached
  `immutable` for 24 hours. No PPTX export, no hosted collaborative editing, and
  no pixel parity with PowerPoint animations. The vendored bundle's own
  JavaScript dependencies are watched by no CI scanner (`govulncheck` is Go-only;
  Grype scans the sandbox image), so a sha256 pin in
  `internal/clientconfig/builtin_skills_bento_test.go` guards the bytes and
  re-vendoring is a documented manual act — see the pack's `templates/NOTICE.md`.

- **`bento_doc.py pdf`: the agent can now export a deck to PDF and attach it.**
  Building a deck and *sending* one used to be two different jobs: the deck is a
  `.bento.html` the reader opens, so "email these slides to the board" ended with
  the agent telling the user to open the deck and click the printer icon
  themselves. `bento_doc.py pdf Q4_Review.bento.html` now writes
  `Q4_Review.pdf` beside the deck — one page per visible slide, on the standard
  960x540pt 16:9 slide page — so a turn can end with the deck AND an attachable
  file. The deck is only read, never touched.

  **Nothing was added to the sandbox image.** The renderer
  (`scripts/bento_pdf.py`) is standard-library-only: no browser, no reportlab, no
  new RPM. That was the deciding constraint. Reusing Bento's own export is not
  possible — `exportPdf()` builds a print DOM and calls `window.print()`, so the
  export *is* Chromium — and putting a browser in the sandbox would add ~400MB,
  a hand-rolled CDP driver (there is no Node in the sandbox), a large new parser
  surface where model-authored code runs, and chromium under the
  `check-grype-policy.sh` fixable-CRITICAL RPM gate, which would block every
  merge in the repo rather than just Bento work.

  So this is a second renderer for the **static** form of a document — and static
  is what a Bento PDF already is: the app's own export renders each slide through
  the same static renderer it uses for thumbnails, so morph, count-up, entrances
  and ken-burns are absent from both. The geometry is ported from the vendored
  runtime rather than re-imagined: the app's `!stateOf && !hidden` page filter
  (so `{{page}}`/`{{pages}}` agree between exports), the CSS line box and wrap
  behaviour, `pd(angle)` gradients including per-stop alpha via a luminosity soft
  mask (which is what makes the documented photo + scrim recipe fade instead of
  going black), tables, PNG/JPEG images, and the charts-lite engine function for
  function — palette, grid insets, tick algorithm, number formatting,
  bar/line/pie/scatter, dual axes, area fills, pie leader lines, legend.

  **Measured against the real thing**, by driving Chromium, clicking the app's
  own *Export PDF (print)*, and diffing the two PDFs: identical page counts and
  identical words on every page (140/140 across a five-slide deck with charts, a
  table, a hero image, gradients and dynamic fields); on a deck whose font both
  sides share, **all 150 words in the same position** (mean Δx 0.03%, max 0.44%
  of page width) with every wrap point identical and baselines within 3.3px on a
  720px canvas; charts visually indistinguishable across bar, dual-axis
  bar+line, smooth line with area, scatter on a numeric axis, negative values
  with a pinned axis and formatter, donut, `label:false` pie, 18-category
  three-series, single-datum and empty-series cases. Every generated PDF parses
  and renders clean under MuPDF — an independent implementation — with selectable,
  searchable text. Where the two differ it is the substituted typeface, not the
  layout: headless Chromium resolves `system-ui` to DejaVu Sans.

  **Honest scope**, and the skill prints a `note:` for each when a deck hits it:
  fonts are the PDF core 14 mapped from the CSS stack, so a deck that *embeds* a
  woff2 face renders in the fallback (embedding it would need a brotli decoder
  and a TrueType subsetter — not in the standard library); text is WinAnsi, so
  CJK, Greek, Hebrew, Arabic and emoji come out as `?` and need the in-app
  export; `svg` elements, KaTeX math, blur, shadow and blend modes are skipped;
  media becomes the same poster block the app's print path draws; remote image
  URLs are left out because a PDF has no network. The deck's own button remains
  the authority and the skill says so rather than overselling this one. No
  `.pptx` (unchanged), no speaker-notes layout, and no mail integration — the
  PDF is an ordinary workspace file, and sending it is whatever tool a
  deployment's bundle provides.

  The cost of a second renderer is drift, so four tests make it loud:
  `TestBentoPdfAssumptionsStillHoldInTheApp` inflates the vendored runtime and
  pins the facts the port copied (print filter, `#bento-print`/`.bp-page` print
  DOM, chart palette in both directions, axis greys), so a re-vendor that changes
  one fails CI; `TestBentoPdfExportMatchesTheAppsPageSelection` pins the page
  set, page size, selectable text and that the deck stays byte-identical;
  `TestBentoPdfExportFailsClosed` pins every refusal path (no `..`, no absolute
  or link-breaking names, `.pdf` only, nothing printable, missing directory) and
  that no partial or temp file survives; and
  `TestBentoPdfRendererIsStandardLibraryOnly` allowlists the renderer's imports
  so the "adds nothing to the image" claim cannot rot. Full design note:
  [docs/BENTO-PDF-EXPORT.md](docs/BENTO-PDF-EXPORT.md).

- **Admin-configurable model tiers (#1187).** The default and advanced
  ("recommended") models are now workspace settings — Settings → Admin →
  Features → Model tiers — instead of compile-time constants, so a lab
  refresh no longer needs a code push. `default_model` / `advanced_model`
  join the workspace-settings registry (admin override > `FLEET_DEFAULT_MODEL`
  / `FLEET_ADVANCED_MODEL` env > compiled-in default), apply live through
  agentcore holders, and reach the web on every shell mount via
  `/client-config`. The admin rows use the existing catalog picker, which
  unions OpenRouter with admin-configured workspace providers
  (`provider/model` — Bedrock, OpenAI-direct, …) and accepts any typed slug.
  Admin-only, like every workspace setting. The scheduled-task default
  (`FLEET_TASK_MODEL`) deliberately stays env-only — it is boot-bound in the
  scheduler ([ADMIN-SETTINGS.md](docs/ADMIN-SETTINGS.md)).

- **Unit tests for six thinly-covered helpers**, from the consolidated Jules
  batch (#1188). Test-only — no production code changed:

  - `agent.MCPLoadServers` — drives a real JSON-RPC handshake against an
    `httptest` server and pins the `StopTurn` contract (true when a server
    binds, false when nothing new loaded), the `loadedServers` bookkeeping, and
    that a missing config, missing client, or unknown/disabled server comes back
    as a *response* the model can read rather than a hard error.
  - `mcpoauth.RevokeToken` — pins the RFC 7009 credential split: with a client
    secret the request uses Basic auth and omits `client_id` from the body;
    without one it does the reverse.
  - `mcpoauth.IsTerminalRefreshError` — the terminal-vs-transient table, now
    also covering wrapped errors so the classification is proven to survive
    `fmt.Errorf("%w")` annotation on the way up.
  - `clientconfig.validSkillName` — the accepted charset, including that a name
    of just `"-"` is currently accepted.
  - `agentcore.EnvPrefix.lookupFloatDefault` — unset, valid, empty, whitespace,
    and unparseable all resolve as intended.
  - `config.ValidateEnvKnobs` — that a problem message carries the offending
    value and the constraint it violated, not just the key, and that a range
    violation is reported distinctly from a parse failure.
  - `chattui.Resolve` — whitespace trimming on the `Model` and `Persona` flags.

  Honest scope: these fence existing behavior. They found no bugs in the code
  under test and fixed none. Coverage is an advisory signal here, not a merge
  gate, so the value is the pinned behavior rather than the percentage.

### Removed

- **The sandbox publisher no longer rewrites its caller's manifest or opens a
  pin PR (`publish-sandbox-image.yml`).** That half of the workflow never worked
  once: across 8 recorded runs in 6 repos between 2026-06 and 2026-08 it created
  zero pull requests. The two runs that reached it — reklaim-config 2026-07-29,
  elcano-config 2026-08-06 — both built and pushed their image successfully and
  then died on `GitHub Actions is not permitted to create or approve pull
  requests`, an org Actions setting no workflow can fix. It was redundant
  anyway: every bundle README already documents adoption as setting
  `FLEET_SANDBOX_IMAGE` per box, and fleet's own `config/default` must stay
  unpinned. Gone with it: the `update_manifest` input, the
  `peter-evans/create-pull-request` dependency, and the `contents: write` +
  `pull-requests: write` permissions those steps required — the publisher now
  asks only for `contents: read` + `packages: write` and cannot write to a
  caller repo at all. The build-and-push half, which is the half that
  demonstrably works and the only half a Kubernetes deployment can use, is
  unchanged and now exposes `image_ref` / `image_digest` as workflow outputs
  plus a run summary carrying the ref to adopt. Caller workflows in the five
  client bundles were updated to match and now publish under
  `ghcr.io/${{ github.repository_owner }}/…` as the documented contract always
  said; `omnicom-config` was additionally publishing to
  `fleet-sandbox-example`, a copy-paste from its example-config fork that would
  have overwritten the template's `:latest` with Omnicom's bundle image the
  first time it fired.

- **Dropped a tautological `HostExecutorCompiledIn` test** that arrived in the
  same batch. It asserted `HostExecutorCompiledIn() == hostExecutorCompiledIn`
  against the very constant the function returns — `x == x`, unfailable, and
  tag-agnostic where the property that actually matters is tag-*dependent*: a
  release build must report `false` so the MockMode path fails closed instead of
  running tool calls unsandboxed on the host (#159).

  A test that pinned the real invariant would have to assert `false` under
  `!fleet_host_executor`, and every `go test` in CI runs with
  `-tags fleet_host_executor` (the untagged lane is `go build` only), so such a
  test would never execute as the pipeline stands. Rather than add a test that
  silently never runs, the invariant is left to the tag wiring and the existing
  `host_disabled.go` fail-closed path. Adding an untagged `go test` lane would
  make it testable and is the prerequisite for trying again.

- **Dropped the Codecov upload step and `codecov.yml`.** The repo has no
  `CODECOV_TOKEN` secret, so `codecov/codecov-action` could never upload: every
  `go` job ended with a missing-token warning and the thresholds in `codecov.yml`
  (project drop ≤2%, patch ≥60%) were never evaluated by anything. Nothing was
  gating on it — `fail_ci_if_error` was already `false` — so removing the step
  changes no merge gate, it just stops the noise and the dead config.

  Coverage itself is unchanged and still collected: `go test` keeps
  `-coverprofile=coverage.out -covermode=atomic`, `Coverage summary` prints the
  project total and writes `coverage.html`, and `Per-package coverage summary`
  still writes the full `go tool cover -func` table to the Actions job summary.
  Those two steps are now the whole coverage signal. Docs (`AGENTS.md`,
  `docs/TESTING.md`) updated to say so instead of describing a Codecov check.

### Changed

- **The tool-call governance framing is now one shared sequence instead of two
  hand-kept copies (#1127).** `policyGuardedTool.Run` (native/loader/bridge
  tools) and `mcpTool.Run` (MCP tools) each carried a full copy of the
  gate→journal→execute→govern→bound→record sequence, including duplicated
  post_tool_use appending — an ordering bug fixed in one copy would have been
  invisible in the other. The sequence now lives once in
  `runGovernedToolCall` (`internal/agentcore/tool_call_framing.go`); the two
  wrappers inject only their genuine differences through explicit seams: the
  MCP argument parse (which deliberately sits between the policy gate and the
  intent journal, so an unparseable call never journals) and the per-type
  execute step. The two deliberate divergences are preserved and documented
  at the seam — a native failure returns a non-nil Go error
  (`boundedModelToolError`) while an MCP failure is always an error response
  with a nil Go error, per the MCP spec. No behavior change; the one cleanup
  is that the native error path no longer runs the model-output boundary
  twice — a no-op for all realistic inputs, and in the one adversarial
  re-detection corner (an over-cap error text whose head/tail preview cut
  manufactures a binary-looking run the full content lacked) the single-pass
  journal/audit bytes are the more consistent choice: they match MCP's
  long-standing single-bound behavior, and the model-visible bytes converge
  at the outer boundary either way. The load-bearing MCP gate ordering
  (arg-parse before the intent journal) and the refusal paths'
  journal-nothing property are now pinned by tests
  (`TestTurnJournal_InvalidMCPArgsJournalNothing`,
  `TestTurnJournal_GateRefusalsJournalNothing`). Same batch:
  `buildConfirmAuditPolicyTool` now resolves its orchestration through
  `policyOrchestration` (the one Policy→orchestrationState unwrap walk,
  #1125) instead of a third hand-rolled copy of the loop.

- **The seven hand-maintained task-row column enumerations are now one
  table-driven registry (#1126).** `taskColumnRegistry`
  (`internal/sched/db/task_columns.go`) is the single source of truth for the
  76-column tasks row: the SELECT list + `scanTask`'s positional scan, the
  INSERT column list and its placeholder count, the `ON CONFLICT` upsert
  clause, and `UpdateTaskTx`'s UPDATE all derive from per-row
  `read`/`insert`/`upsert`/`txUpdate` flags at package init (hot paths keep
  their once-built statements; per-row work is unchanged). The manual
  `taskInsertColumnsCount` — whose drift broke every batch insert in #710 —
  is retired: statement and arguments now come from the same slice, so that
  drift class is structurally impossible. The excluded-column doctrine
  (result-like/pause/wake columns, `effective_priority`,
  `recurrence_spawned` are deliberately absent from the generic writes) is
  machine-checked: every exclusion carries a required per-column reason, and
  the round-trip test proves the doctrine in SQL in both directions — values
  seeded into excluded columns' Task fields do not persist through the
  insert, and non-NULL values seeded directly into those columns survive
  both a repeat upsert and an `UpdateTaskTx` whose in-memory task carries
  zeroes for them, so flipping an exclusion flag turns the suite red. The
  `export` flag pins the portable-definition set against
  `models.TaskExportRecord` by JSON tag, chaining into the #1104
  completeness tests so export→overlay drift stays impossible; a new
  schema↔registry test diffs `information_schema` against the registry, so a
  migration without a registry row (or vice versa) fails loudly. Pure
  structural refactor — no behavior change; every existing sched/db,
  storage, handlers, models, scheduler, admincli and export/import test
  passes unchanged. A new task column is now one migration + one registry
  row (+ the model field); the AGENTS.md "New task fields thread one way"
  bullet describes the new flow.

- **Restored nine transient-error cases to the `IsTerminalRefreshError` table.**
  Moving that test into its own file (part of the batch above) had cut it from
  eleven cases to eight, dropping `invalid_request`, `temporarily_unavailable`,
  `http_500`, and `http_503` — precisely the "a blip must stay transient" half of
  the contract, whose loss would let a 5xx start forcing users through a
  reconnect. It also stopped setting `HTTPStatus`, so nothing pinned that the
  decision keys off the OAuth error code rather than the HTTP status. A mutation
  that marks `http_503` terminal passes the trimmed table and fails the restored
  one. The two new wrapped-error cases from the move are kept, and the
  explanatory comment the move had orphaned in `flow_test.go` now sits with the
  test again.

- **Trimmed the new `ValidateEnvKnobs` scenarios to what was not already
  covered.** Three of its five cases duplicated existing tests — one used the
  same two knobs and the same values as `TestLoad_MalformedKnobsAllReportedAtOnce`,
  and blank-is-unset was already in `TestLoad_KnobValueNormalization`. What
  remains asserts message content, which nothing covered before.

- **Batched four per-iteration database writes that ran one round trip per
  item.** A static review flagged five query-in-a-loop sites; four were real and
  are now single (or chunked) statements, with identical semantics:

  - `store.SweepExpired` per-user cap eviction (`internal/store/store.go`) —
    scanned for overflowing users and then issued one `DELETE` per user. Now one
    statement: `ROW_NUMBER() OVER (PARTITION BY user_email ORDER BY updated_at
    DESC, id DESC)` selects exactly the rows each user's `OFFSET cap` selected,
    for every user at once, and the `HAVING` pre-scan is gone. This path runs
    after *every* successful turn, so it was the costliest of the five.
  - `Store.UpsertTaskMemory` LRU eviction (`internal/sched/taskmemory.go`) —
    deleted one key per round trip until the cap was met. Now one `DELETE` with
    `LIMIT <overflow>` in the same oldest-first order. Usually the overflow is a
    single key; lowering `maxKeys` made it arbitrarily large.
  - `Store.ReplaceRelationsForMemory` (`internal/store/memorygraph.go`) — one
    `INSERT` per extracted triple inside the transaction. Now chunked multi-row
    `INSERT`s (the `RecordToolCalls` pattern), bounded by a new `maxBatchRows`
    so the parameter count stays far under Postgres' 65535 cap.
  - Crash-recovery synthesized-result markers (`internal/store/turn_journal.go`)
    — one `INSERT … ON CONFLICT DO NOTHING` per unknown-outcome call. Now the
    same chunked multi-row form, still idempotent across repeated recoveries.

  The fifth flag, the per-row `UPDATE` in `Database.ArchiveOldLogs`
  (`internal/sched/db/db.go`), is **not** an N+1: each row carries a different
  payload compressed (and optionally encrypted) in Go, so there is nothing to
  join against, and folding a page of multi-megabyte blobs into one statement
  would trade the documented per-row commit boundary for a large memory spike.
  Left as-is with a comment recording why. New tests pin per-user cap
  independence, bulk LRU eviction, the relation chunk boundary (including
  re-extraction idempotency), and multi-marker recovery.

- **`expandCidImagesToDataURLs` no longer compiles a regex per inline
  attachment.** The approval-preview substitution built `regexp.MustCompile`
  inside the attachment loop and rescanned the whole document for every inline
  image (O(N·M)). It now uses one package-level `cidPattern`, collects the
  attachments into a cid→data-URL map, and rewrites the document in a single
  `ReplaceAllStringFunc` pass — ~5.5x faster on a two-image body, and a
  substituted data URL can no longer be rescanned by a later attachment's
  replacement. Behavior is otherwise unchanged: same case-insensitive scheme
  and id matching, same verbatim passthrough for a cid with no attachment.

- **PRs into `dev` now actually run CI, and the fast lane gained the web
  lane it never had.** `dev-ci.yml` fired only on *push*, and `ci.yml` filters to
  `main`, so a pull request targeting `dev` was gated by nothing but CodeQL —
  `dev` itself was the first thing to ever build a change. Every Dependabot PR
  merged unverified that way, and a `next` 16.3.0 bump landed a TypeScript error
  that only surfaced at the dev→main promotion, where several changes were
  already stacked on it. The lane now also fires on `pull_request` into `dev`.

  Separately, nothing on `dev` ran `web/` **at all** — no lint, no vitest, no
  `next build` — so all three npm Dependabot PRs reached `dev` with no build
  behind them. Added a `Web lint / test / build (fast)` job mirroring `ci.yml`'s
  `web` job command-for-command, and added it to the `Dev gate` aggregate.

  Also fixed the migration DDL lint's base ref on the new trigger:
  `github.event.before` is push-only, so on a PR it would have been empty and the
  script's fallback (merge-base with `origin/main`) would have diffed in every
  migration `dev` had accumulated since `main`, flagging files the PR never
  touched. It now uses `github.event.pull_request.base.sha` on a PR.

  `-race`, govulncheck, Grype, both Playwright suites, and CodeQL stay deferred
  to the promotion's full gate: the split is "does it compile, lint, and pass
  tests" on `dev`, "is it safe to ship" on the promotion. Documented as a
  dev-vs-main table in [`docs/TESTING.md`](docs/TESTING.md).

- **govulncheck: documented why it is deliberately unpinned, and added a daily
  non-blocking scan so advisories are found on a schedule.** The lib/pq incident
  raised the obvious question of whether to pin the gate. Both halves stay
  floating, on purpose:

  - The **database** is fetched from <https://vuln.go.dev> per run. Pinning it
    would blind the gate to anything disclosed after the pin, which is the entire
    point of the check. The cost — the same commit can pass today and fail
    tomorrow — is the gate working, not flaking.
  - The **scanner** stays `@latest`: that is upstream Go's own documented
    invocation, and `golang/govulncheck-action` exposes no version input either.
    A pin here could only be a hand-maintained string (Dependabot does not
    rewrite versions inside `run:`), and the go.mod `tool` directive is worse —
    tool dependencies share the main module's build list, so the scanner would
    dictate the product's `x/net` version. A pin nobody bumps quietly costs
    detection as the reachability analysis improves.

  What was actually missing was discovery latency: the only thing that told this
  project a new advisory applied to it was an unrelated PR turning red. New
  `govulncheck-scheduled.yml` runs the same scan against `main` daily (08:00 UTC,
  non-blocking) and reports SARIF to the Security tab, mirroring the existing
  `grype-scheduled.yml` pattern. It makes nothing greener — the gate still blocks
  a reachable vulnerability — it changes *when* the advisory is found. It also
  sees strictly more: `-format sarif` reports advisories in modules the build
  requires but never calls (SARIF level `note`), which the blocking text-mode
  step ignores entirely.

- Committed the `web/next-env.d.ts` line Next 16.3.0 regenerates, so a local
  `next build` no longer leaves the tree dirty. The file is generated and marked
  "should not be edited"; this just records what the current Next emits.


- **A malformed numeric/bool/duration env value now refuses to boot** (#1119).
  Every typed config knob (~70: cost/token/iteration ceilings, timeouts, DB
  pool sizes, sandbox caps, feature booleans, `FLEET_LOCKDOWN_ONLY`, …) used to
  silently fall back to its default when set to something unparseable — fail-
  OPEN for security knobs: `FLEET_LOCKDOWN_ONLY=enabled` left lockdown off,
  `FLEET_MAX_COST_USD=5O` (letter O) ran with the $50 default. `config.Load`
  now fails startup with one error naming every offending variable, its value,
  and the expected format. **Operators with a typo'd env var will now get a
  startup error instead of a silently-running default — that is the point.**
  An UNSET (or blank) knob still gets its default; only set-but-malformed
  values error. Booleans accept `1/0, true/false, yes/no, on/off`; durations
  are Go syntax (`30s`, `5m` — a bare number now errors instead of being
  ignored). The four hot-reloadable ceilings also enforce their reload ranges
  at boot (`FLEET_MAX_ITERATIONS` 1–10000; `FLEET_MAX_COST_USD`,
  `FLEET_MAX_TOTAL_TOKENS`, `FLEET_TEMPERATURE` ≥ 0), so boot and SIGUSR2
  reload now accept/reject identically — previously a value boot silently
  defaulted was loudly rejected on reload. All three read paths (boot, hot
  reload, `fleet validate-config`) parse through one shared registry
  (`internal/config/knobs.go`); `fleet validate-config` now preflights **all**
  registered knobs instead of a hand-list of three, and a source-scan test
  keeps the registry and the loader from drifting. `FLEET_MAX_COST_USD=0`
  (0 = no cost ceiling) boots and preflights clean; negatives still fail.
  `FLEET_MAX_CONCURRENT_AGENTS` now requires a value ≥ 1 everywhere: `fleet
  serve` feeds it straight into the admission limiter, where 0 floors to a
  box-wide concurrency of ONE (it is NOT "use a default" — only the
  standalone runner pool treats <1 that way), so a set 0 is refused at boot
  and preflight instead of silently strangling the box. Quote handling is unified — every
  helper strips one layer of matching quotes, so the same quoted value
  resolves identically wherever it is read — and the env-file inline-comment
  rule (an unquoted value ends at ` #`; quote values containing it) is now
  documented in `docs/OPERATORS.md`.

- **The strong/escalation tier is back to `openai/gpt-5.6-sol`** (OpenAI: GPT-5.6
  Sol), reverting the one-release move to `x-ai/grok-4.6` (#1040). This is what
  `suggest_advanced_model`, the spreadsheet nudge, and the task fallback resolve
  to. Moved in lockstep across the mirrors the contract in `modelAliases.ts`
  names: `ADVANCED_MODEL` / `ADVANCED_MODEL_LABEL`, the picker's `SEED_MODELS`,
  the Operations Center's `DEFAULT_FALLBACK_MODEL`, `agentcore.DefaultMaxModel`
  (`AdvancedModelSlug` follows it), and `splitLockdownModels`' strong-tier entry.

  **This restores the escalation window: 500,000 → 1,050,000.** The Grok move
  flagged that reduction as its one real regression, on the grounds that this is
  the tier users escalate to for the hardest — and usually largest — problems.
  It costs more at current catalog prices: $2.50/M prompt and $15.00/M
  completion, versus $2.00 and $6.00 (1.25x input, 2.5x output). Modality and
  tool/reasoning support are identical (text+image+file in).

  Two things the swap changes that a slug-for-slug edit would have missed:

  - **The tier is pinned again.** `x-ai/` has no `canonicalUpstream` entry (xAI
    is its only upstream, so a pin buys nothing), but `openai/` carries a soft
    pin — so the escalation path regains per-upstream prompt-cache locality. The
    pin and served-upstream tests that asserted "the strong tier is unpinned"
    now assert it pins to OpenAI, and the unpinned-family case they were
    covering moved onto an explicit `x-ai/grok-4.6` slug so it stays covered.
  - **The static cold-start context table needed a new row.**
    `openai/gpt-5.6-sol` prefix-matches the existing `openai/gpt-5` → 400,000
    entry, and the table returns the FIRST match — so without a longer-prefix
    row ahead of it, a cold boot would compact the escalation target at 38% of
    its real window. Added `openai/gpt-5.6` → 1,050,000 (the whole 5.6 family —
    sol/luna/terra and their `-pro` variants — is 1,050,000). This row did not
    exist the last time `sol` held the tier, so the under-sizing is fixed rather
    than restored. The `x-ai/grok-4.6` → 500,000 row stays: the slug remains
    selectable.

  Also adds `google/gemini-3.7-flash` to the fake-LLM catalog's model list — one
  of the hand-synced tier mirrors that the everyday-default swap (#1154) missed.

- **The recommended everyday model is now `google/gemini-3.7-flash`** (Google:
  Gemini 3.7 Flash), replacing `deepseek/deepseek-v4-flash-0731` in every
  default slot: `agentcore.DefaultCoreModel` (chat + scheduled runs + the
  Operations Center form), `config.DefaultTitleModel` (and the metadata /
  memory / recurring-task / library-prompt models that chain off it), the
  lockdown allow-list default, and the frontend `DEFAULT_MODEL` +
  seeded picker row.

  Routing changes shape with it. DeepSeek needed a *soft* pin plus an
  `fp8AndAbove` floor because 28 OpenRouter endpoints served that family at
  fp4-to-fp8; Google serves this family alone, so `canonicalUpstream` already
  pins it **strictly** (`Only`, no fallbacks) and it needs no floor — there is
  no pool to vary precision across. The DeepSeek pin and floor stay for
  operators who still select those slugs. A new test,
  `TestDefaultCoreModelCannotBeServedAtArbitraryPrecision`, asserts the property
  rather than the lab — whichever family holds the default slot must be either
  strictly pinned or floored — so a future swap cannot quietly drop the
  guarantee.

  The static cold-start context table gains an exact-slug entry at 1,048,576.
  It is deliberately not a `google/gemini-3` family prefix: the Nano Banana
  image variants in that family are 65K–131K, and an over-large window is worse
  than a missing one (a missing entry falls back to the conservative 200K and
  merely compacts early; an over-large one feeds the upstream more than it
  accepts and hard-errors).

  **Cost note:** Gemini 3.7 Flash is priced above the model it replaces
  ($0.375/M prompt and $1.875/M completion, versus $0.14 and $0.28), so
  everyday spend rises unless a deployment overrides the slot. It buys a
  multimodal default (text+image+file+audio+video in, versus text-only) at the
  same 1M-class context window.


### Security

- **Cleared the three open code-scanning alerts on `dev`, two of which were
  taking the CodeQL check red.** All three were true reports of a
  misleading-looking pattern rather than of an exploitable bug, and each is
  fixed by making the code say what it actually does instead of by suppressing
  the query:

  - **`bento_doc.py` looked like it logged live-session credentials in clear
    text (CodeQL `py/clear-text-logging-sensitive-data`, two High alerts).**
    `collab_secrets()` returned the *names* of the credential-bearing fields
    present in a deck's `collab` block — `ownerPriv`, `writerPriv`, `invite` —
    filtered from a module-level constant tuple of those literals. No value was
    ever read out of `collab`, let alone printed; the three warnings tell an
    operator *which fields to go look at*, which is the whole point of the
    warning. But a function called `collab_secrets` whose result is written to
    stderr is indistinguishable, to a name-driven taint analysis, from one that
    prints the keys — and, more to the point, from a future edit that starts to.
    The constant is now `COLLAB_CREDENTIAL_FIELDS`, the accessor is
    `collab_credential_fields()` with a docstring that says names-only and why,
    and the three warning sites share one `collab_field_label()` formatter so
    the names-only rule lives in a single place instead of being re-derived per
    print site. Behavior is unchanged: the warnings carry the same field names,
    prefixed `collab.` for the reader. The alerts were the naming, and the
    naming was genuinely wrong — a list of field names is not a list of secrets.

  - **`NewRecentRuns` sized an allocation from a request parameter (CodeQL
    `go/slice-memory-allocation-with-excessive-size-value`, High).** The
    `?runs=` value on `GET /admin/pipeline-metrics` reached
    `make([]RunPipelineMetrics, 0, limit)` as a capacity hint. The handler did
    clamp to 500 first, so this was not exploitable — but the clamp lived in the
    caller while the O(limit) memory promise lived in the constructor, so
    nothing stopped the next caller from skipping it. The bound now lives with
    the buffer it bounds: `models.MaxRecentRuns` is exported, `NewRecentRuns`
    clamps to `[0, MaxRecentRuns]` itself, the handler clamps against the same
    constant instead of a duplicated `500`, and the slice grows by `append`
    rather than preallocating a requester-chosen capacity. `Add` never exceeds
    `limit`, so the O(limit) bound still holds. Two tests cover it: a huge
    `?runs=` keeps exactly `MaxRecentRuns` with capacity in the same order, and a
    negative limit holds nothing rather than panicking in `make`.

- **Recorded the disposition of GO-2026-5932 (`golang.org/x/crypto/openpgp` is
  unmaintained and unsafe by design) rather than leaving it to be re-triaged.**
  There is nothing to fix and nothing to bump: the advisory has no fixed
  version — the package is deprecated by design, not broken in a release — and
  fleet does not import it. `go mod why golang.org/x/crypto/openpgp` reports
  *"main module does not need package golang.org/x/crypto/openpgp"*; the only
  `x/crypto` packages in the build are `bcrypt` (password hashing in
  `internal/store`, `internal/sched`) and `acme/autocert` (`cmd/fleet/tls.go`).
  `x/crypto` is already at v0.55.0, the latest. The govulncheck gate agrees and
  stays green — it is call-graph aware, and reports the advisory as
  *"1 vulnerability in modules you require, but your code doesn't appear to call
  these"*, which is not a failure. The alert reaches the Security tab from the
  module-level dependency graph, which cannot see reachability. Correct response
  is to dismiss it as not-affected; it will return on any `x/crypto` bump, since
  no bump can clear an advisory with no fix.

### Fixed

- **`publish-sandbox-image.yml`: a manual dispatch would have opened a PR
  pinning `sandbox.image` in fleet's *own* `config/default` bundle.**
  `update_manifest` was declared only for the `workflow_call` trigger, so on a
  `workflow_dispatch` the guard `if: ${{ inputs.update_manifest != false }}`
  compared a *null* against `false` — which is true. An ad-hoc publish of the
  generic bundle, whose entire purpose is to get an image into GHCR, would
  therefore have rewritten `config/default/manifest.yaml` (where `image` must
  stay `"${FLEET_SANDBOX_IMAGE:-}"`, the build-on-box default) and opened a PR
  against this repo. `update_manifest` is now declared for `workflow_dispatch`
  too, defaulting to `false`, and both guards are a plain truth test.

  Three further hardening changes to the same workflow, none behavioral for a
  correctly-invoked caller:

  - `secrets.GITHUB_TOKEN` and `github.actor` are passed to the GHCR-login step
    as `env` instead of being interpolated into the `run:` body. A `${{ }}`
    expression is spliced into the script before the shell sees it, which puts
    the token on a command line and makes any shell metacharacter in a value
    executable; `"$GHCR_TOKEN"` keeps it a value. `github.sha` is likewise
    hoisted to a job-level `SHA`, so no `run:` body interpolates at all.
  - A `concurrency` group keyed on `inputs.image_name` serializes publishes of
    the same image. Two overlapping runs both move the mutable `:latest` tag,
    and the loser of the race decided what it pointed at. Keyed per image name,
    not per repo, so two bundles in one repo still publish concurrently.
  - The header now records why the Actions page shows this workflow red and why
    it has not run since 2026-06-23 — the same answer for both. Its last six
    runs predate #195/#705, when the file still had a `push: branches: [main]`
    trigger and pushed `ghcr.io/elcanotek/sandbox` on every merge; all six died
    at `podman push` with `denied: permission_denied: write_package`, because a
    repo `GITHUB_TOKEN` cannot write an org-level package that is not linked to
    the repo. Converting the file to a reusable per-client publisher removed the
    push trigger, so nothing has fired since and GitHub still displays the last
    recorded conclusion. No fleet gate depends on this workflow — core CI builds
    the sandbox locally (24ce69f) — so the red is a tombstone, not a broken
    pipeline. The note also states the caller-side requirement that the June
    runs violated: publish under the caller repo's own owner, and link the
    package to that repo once.

- **Agent-runtime LOW batch (#1125): six small correctness/accounting fixes in
  `internal/agentcore` + `internal/agent`, one cleanup pass.**

  - *Prompt roster nondeterminism (prompt-cache hazard).* The interactive
    system prompt's MCP tool roster attributed each `mcp_<server>_<tool>` name
    to the FIRST Optional server a map range happened to match, so with
    overlapping server names (real under the `<server>_<account>` variant
    convention, e.g. `jira` / `jira_prod`) a variant's tools appeared in or
    vanished from the prompt per turn at random — silently busting the
    byte-stable cacheable prefix (`docs/PROMPT-CACHE-CONTRACT.md`). The filter
    now resolves the longest matching server name (`internal/agent/prompt.go`),
    the same deterministic variant treatment `mcpAllowlist.toolsFor` applies.
  - *Steering re-injection dedupe was positional.* `steeringStep`'s
    already-present probe checked only the recorded position ±1; a compaction
    or budget-step reduction that shifted history by more than one slot made
    re-application insert the same steered user message twice into the
    provider input. The probe keeps its fast path and falls back to a
    content scan with per-steer claim tracking, so identical-text steers each
    keep exactly one copy (`internal/agentcore/steer.go`).
  - *A non-conforming Policy silently zeroed usage accounting.* `Run` used to
    mint a throwaway orchestration state per round when a Policy exposed no
    `orchestration()`, so such a run proceeded with `Result.Usage` stuck at
    zero and ceilings that never fired. Run now refuses the Policy up front
    (every production Policy conforms), and the redundant per-round
    `policyOrch` lookup collapsed onto the single run-wide `usageOrch` that
    #1118's aux-metering seams already bind.
  - *`parseTaskTrackerSnapshot` parsed less than its comment claimed.* The
    comment promised JSON *or* the human `Summary:` line; only JSON was
    parsed, and the recorded result legitimately stops being one clean JSON
    document (a post-tool hook fragment is appended after it; an oversized
    result becomes a truncation envelope quoting a preview) — those shapes
    returned `Seen=false` and silently disarmed the pending-work finish gate.
    The Summary-line fallback is now implemented, and new tests build their
    inputs from the real `task_tracker` tool so an output-format change breaks
    the test instead of the gate.
  - *Round-cap exhaustion discarded accumulated usage/entries.* Exhausting the
    20 enforcement rounds returned `Result{Label}` with the error, dropping
    the whole paid transcript and token/cost accounting. The RETURNED `Result`
    now carries the accumulated `Usage`/`Entries`/`Rounds` alongside the
    error, matching the `ErrCommittedSideEffects` partial-result contract.
    Honest scope: the scheduled driver (the only mode that can reach this cap)
    still discards the Result on its error paths today, so persisting this
    carry there is a follow-up — the fix makes the accounting *available*, not
    yet *surfaced*.
  - *O(N²·parts) re-estimation in the inner context reducer.*
    `compactOldToolResults`/`evictOldToolInputs` re-ran the full-history token
    estimate inside their per-part loops on every over-target provider step.
    They now keep a running total (the estimate is a per-part linear sum) that
    a test pins equal to a fresh re-estimate; what gets reduced is unchanged.

- **Detached background work outlived the request that started it, and nothing
  waited for it.** `activeTurns` tracked detached *turns* and `DrainTurns` blocked
  on them at shutdown, but the work that is not a turn was tracked by nothing at
  all: the queue-drain re-kick (`time.AfterFunc`, up to 3s out, reaching
  `ClaimNextQueuedInput`), memory-graph extraction (a `go func` that writes
  relations on a detached context), the retained-buffer eviction timer (15 minutes
  by default), and the approval push send. Two of those touch the store, so a
  shutdown could return while a write was in flight, and a finished test could
  leave a writer running against the database the next test was about to truncate.

  `backgroundTracker` (`internal/httpapi/background.go`) now owns all four.
  Pending timers are cancelled rather than waited out — waiting on the 15-minute
  eviction timer would make every shutdown a 15-minute one — while work already
  running is waited for, which is the case where proceeding means closing the
  store underneath a live writer. `StopBackground` runs after `DrainTurns` in
  `cmd/fleet`, in that order because a turn's completion tail can schedule a
  re-kick.

  The DB-backed httpapi fixtures now tear down in the same sequence the server
  uses on SIGTERM — `BeginShutdown`, cancel, drain, stop background, then close
  the store — instead of closing the store first and leaving goroutines writing
  into a dead pool.

  **`BeginShutdown` first is the step that does the real work**, and it is why
  cancelling turns was not sufficient on its own. A turn's completion tail drains
  the input queue and launches the next queued row, so in a test that leaves rows
  queued (the depth-cap test leaves ten) a single cancellation produces a *fresh*
  turn with a live context, and the chain re-arms itself faster than any grace
  period outlasts it. The `shuttingDown` flag is what makes `maybeDrainQueue`
  decline to launch — exactly why production sets it before draining.

  The teardown asserts rather than tolerates: if a turn is still running 20s after
  cancellation the test fails, naming the count, because a silent timeout there
  would hand the next test a live writer again.

  Scope, stated precisely: the leak is proven by turns outliving their tests —
  before `BeginShutdown` was added to the teardown the package hung, with a
  goroutine dump showing `runTurnAsync` parked in a test's gate after that test had
  returned, and the teardown check reporting `ActiveTurns=1`. The `sql: database is
  closed` log spray that first drew attention to this came from a CI run and does
  **not** reproduce in a single local sequential run — baseline and fixed both
  showed zero locally — so the log volume is a CI-timing symptom, not the
  measurement. `internal/httpapi` goes from 15.3s to 18.3s for the added teardown
  discipline. `backgroundTracker`'s cancel/wait/finality/panic behavior and the
  drain-declines-while-draining property are covered by deterministic tests that
  need no database.

- **The test fixture that wipes the database deadlocked in CI, failing PRs that
  had touched nothing near it.** `store.TruncateAllForTest` issued a bare
  `TRUNCATE`, which takes `ACCESS EXCLUSIVE` on each table one at a time in list
  order — `conversations` early, `users` several entries later. Any ordinary
  transaction that writes `users` and then `conversations` holds those two locks
  in the opposite order, so the pair cycles and Postgres kills one of them. The
  observed failure was `TestMockTurn_AgentFieldUnused` dying on
  `truncate: ERROR: deadlock detected (SQLSTATE 40P01)` in `internal/httpapi`.

  The writers were real, and the comment on `newTestStore` explained why nobody
  expected them: it asserted that serial test execution meant no cross-test
  races. Serial execution is not an idle database. `go test` starts the next test
  the moment the previous one returns, and a goroutine that outlived its test —
  a turn driver still persisting events — writes straight through the next
  fixture's wipe. That comment has been corrected.

  The fixture now takes its locks defensively, in a transaction:

  - A `pg_advisory_xact_lock`, acquired before any table lock, serializes
    fixtures against each other. A waiter holds nothing, so two fixtures cannot
    cycle, and ordinary writers never take it so it introduces no new cycle.
  - Every table in the schema is then locked in one statement — not just the
    `TRUNCATE` list, since `CASCADE` reaches further (`messages`, `turn_events`,
    `chat_input_queue`, …) and an unlocked cascade target is where a wait could
    creep back in. Locking the lot also means this step needs no maintenance as
    tables are added.
  - `NOWAIT` on the early attempts is what removes the deadlock rather than
    merely retrying it: a deadlock requires waiting, so a request that refuses to
    wait cannot sit in a cycle. This also moves who pays for contention. Left to
    a bare `TRUNCATE`, Postgres picks a victim and it is frequently the ordinary
    writer — a test's own background goroutine dying of a deadlock it did nothing
    to cause. Now the fixture takes the loss and retries.
  - Later attempts escalate to a queueing lock, because `NOWAIT` and waiting
    starve in opposite directions: `NOWAIT` asks at one instant and loses against
    a steady write stream, while a queued `ACCESS EXCLUSIVE` blocks the row-level
    requests arriving behind it, so the writers drain. `lock_timeout` keeps a
    queued attempt from hanging the suite instead of retrying.

  Measured against a reproducer that runs six writers in the inverted lock order
  while three fixtures truncate: the old code failed 17 times in a 12-second run,
  the new code 0 times across five such runs (~2,000 truncates), and writer
  casualties went from 19 to 13–19 — unchanged, since a write aimed at a database
  being wiped was doomed either way. Under a deliberately unfair variant whose
  writers never stop, failures fell from 281 to 0–7.

  `TestTruncateAllForTestDoesNotDeadlockAContendingWriter` fences it by
  reconstructing the inversion deliberately; it fails on the old implementation
  with the exact CI error and passes on the new one. It is one-sided on purpose —
  if the timing slips, everything succeeds and it passes — so the regression test
  for a flake cannot become the next flake. Retries are capped rather than
  infinite: a writer that never stops still fails the fixture, with an error
  saying a leaked test goroutine is the likely cause, which is worth failing on
  rather than hiding.

- **A bundle could ship a skill name the skill builder would refuse to save.**
  `clientconfig.validSkillName` checked only the charset (lowercase, digits,
  hyphens), so `-`, `--`, `-research` and `research-` all passed, while
  `store.userSkillNameShape` — the regex gating user-authored skills, and
  documented as the same contract — rejects every one of them. The two had
  drifted: the same name was legal in a bundle and illegal in the builder. The
  charset check is now a shape check that begins and ends alphanumeric, matching
  the regex exactly, including its tolerance for an interior `a--b`.

  The contract is now enforced rather than asserted in a comment:
  `TestValidSkillNameAgreesWithUserSkillShape` walks every short string over an
  alphabet of the interesting character classes and fails if the two sides
  disagree. Against the old implementation it reports 11 mismatches. The 64-char
  cap remains the one deliberate asymmetry — the bundle loader reports length
  separately against `maxSkillNameLen`, so an over-long name draws one message
  about its length instead of a second, vaguer one about its charset.

  Scope: this is a bundle-load *warning*, not a gate — the skill is still loaded
  either way — so nothing that worked stops working. What changes is that an
  ill-shaped name now gets flagged at load instead of passing silently until the
  builder rejects the same name.

- **A refused token revocation reported success.** `mcpoauth.RevokeToken` checked
  the transport error and then returned `nil` without ever looking at the status
  code, so an RFC 7009 §2.2.1 refusal — a 400 or a 401 from the authorization
  server — was indistinguishable from a completed revocation. It now reads the
  body and hands a non-2xx to `parseTokenError`, the same helper the token
  endpoint path uses, so the caller gets an `*OAuthError` carrying the code and
  HTTP status and `IsTerminalRefreshError` can classify it.

  Revocation stays best-effort: the sole caller still discards the result and the
  local record is deleted regardless, so no disconnect flow changes behavior.
  Best-effort now describes the caller's posture rather than a function that
  could not tell success from failure. A 200 for a token the server does not
  recognize is still success, per §2.2. Reading the body before closing it also
  lets the connection be reused.

- **Auxiliary model calls no longer bypass the run's cost/token ceilings and
  usage accounting (#1118).** Several model calls made on behalf of a run
  never reached its accounting, so `checkCeilings` and sub-agent budget
  slices under-counted real spend. Now split by where the call fires:

  - **In-loop calls count against the run's ceiling.** The compaction
    summarizer meters through the new
    `agentcore.CompactionSummarizeInput.RecordUsage` (and pre-checks the
    ceiling: at/over budget it degrades to the deterministic truncation
    placeholder instead of buying another call), and the model-invocable
    `suggest_branch_name` / `suggest_commit_message` /
    `suggest_pr_description` tools meter through a context-carried
    `tools.UsageRecorder` that `agentcore.Run` installs — both land in the
    same `orchestrationState`/`Result.Usage` the finalize retries already
    use.
  - **Host-side extras stay off-ceiling but become visible.** The end-of-run
    verifier, the phone-a-friend review, and the scheduled loop's `llm`
    exit-condition verifier record per-call `aux_usage` entries (label,
    model, tokens, cost) in the persisted session log — carried through the
    captain's-log file's redaction and truncation copies too — instead of
    debiting the run; their documented accounting semantics are unchanged,
    the spend just stops vanishing. Error analysis and the chat→recurring-
    task synthesizer, whose run session is unreachable (or nonexistent) at
    call time, emit the same structured `aux model call` host log line as
    their record. Aux metering never overwrites the `LastStep*` per-call
    input-size signals the context meter and compaction trigger read.
    Design note:
    [`docs/AUX-MODEL-CALL-METERING.md`](docs/AUX-MODEL-CALL-METERING.md).

- **Chat's finalize recoveries now see the turn they are recovering, and can
  no longer repeat its side effects (#1117).** Two flaws in the interactive
  driver's finalize paths: (1) the forced-final-summary call replayed
  `TurnConfig.PriorHistory`/`TurnHistory`, but production never populated
  `TurnHistory` — so when a turn ended with tool calls and no prose (the
  exact trigger), the recovery saw prior turns only, neither the current
  question nor the tool results it was summarizing, and fabricated from
  stale context. `agentcore.FinalizeInput.Messages` now carries the
  finishing round's input plus its completed tool transcript (the same
  `carryRoundMessages` carry the enforcement loop uses), both recoveries
  replay that, and the never-wired history fields are gone. (2) The
  leaked-tool-call retry re-drove the round with the full governed roster
  and no record of tool work already done, so the model could re-issue
  already-executed MCP calls; it now honors ADR-0035's side-effect gate via
  the new `FinalizeInput.RoundToolEvents` — any committed tool event this
  round degrades the retry to the tool-less summary path, which can narrate
  the executed work but not repeat it.

- **Task lifecycle recovery/edge paths: recurrence chains no longer die
  silently, crash-loops dead-letter, lease-lost runs are cancelled, and paused
  expiry counts from the pause (#1116).** Four fixes to the paths around the
  (unchanged) claim/lease core:

  - *Recurrence spawn is now idempotent and repairable.* The next occurrence
    of a recurring task spawns after the terminal tx commits, and any failure
    there — a transient DB error, or a crash in the commit→spawn window —
    previously only logged, ending the schedule forever. The spawn now claims a
    per-occurrence settlement flag (`recurrence_spawned`, migration 065) in the
    same transaction that inserts the successor (and carries its task memory),
    and a new always-on scheduler sweep (`ReconcileRecurrences`) re-drives any
    terminal recurring row whose flag is still unclaimed — logged and counted in
    `fleet_sched_recurrences_reconciled_total`. Chains that legitimately end
    (recurrence_until / run budget / unparseable definition) settle the flag
    without spawning, so the sweep never spins on them. The spawn deliberately
    stays OUTSIDE the terminal tx: folding it in would turn a bookkeeping-insert
    failure into a re-run of the whole occurrence's external side effects.
    Restore-safety: a row INSERTED already success/error — `fleet import`
    restoring terminal recurring history verbatim (#713) — lands with the flag
    settled, so a disaster-recovery restore can never be read as a fleet of
    lost spawns and mass-spawn duplicate successors. Dead-lettered rows stay
    unsettled on purpose (quarantine parks the chain without spawning, and the
    sweep never selects them), and `ReplayDeadLetteredTask` re-arms the flag,
    so a replayed quarantined occurrence continues its chain exactly once.
  - *Lease recovery dead-letters past the retry budget.* `RecoverExpiredLeases`
    reset expired leases to pending and incremented `attempt_count`
    unconditionally — the only max-retries check was the in-process failure
    path, which a task that kills the process never reaches, so a crash-looper
    cycled recover→claim→crash forever. Recovery now routes rows with
    `attempt_count >= max_retries` to the dead-letter queue — exact parity with
    the in-process retry gate, so `max_retries=R` bounds a task at R+1 total
    executions and R=0 ("never retry") gets exactly one, with no free extra run
    of external side effects after a crash. The quarantine writes the same
    column shape as the runner's DLQ path, including a derived
    `actual_duration_seconds` (replayable, listed, counted in
    `fleet_dead_letter_queued_total` under reason `lease_recovery`).
  - *A lease-lost run is cancelled.* When a renewal came back
    `ErrTaskLeaseNotHeld` — recovery re-queued the task and a fresh attempt may
    already be running — the zombie run kept executing its EXTERNAL side effects
    (emails, MCP writes, sandbox actions) to natural completion; only its DB
    writes were token-fenced. Two cancellation paths now bound it: the renewal
    verdict cancels the run via its own snapshotted per-claim cancel (safe
    unconditionally — a per-claim context can never touch a re-claimed fresh
    run), and a re-claim by the same pool cancels the stale run it overwrites —
    the majority ordering on a single box, where recovery and re-claim both
    happen inside one renew interval. The cancellation carries a distinct cause
    so the persisted transcript says "lease was lost", not "server shutdown".
    Other renewal errors (DB unreachable) still only log: they don't prove the
    lease is lost, and if the outage outlasts the lease window the next renewal
    gets the definite verdict.
  - *Paused-task expiry is measured from the pause, not the run start.*
    `ExpirePausedTasks` filtered on `started_at < cutoff`, so a run that
    executed 2h before calling `ask` under a 60-minute window was expired on the
    next tick — a zero TTL, and the human never got to answer. A new `paused_at`
    column (migration 064, backfilled from `started_at` for rows already paused)
    is stamped by both pause transitions (`PauseTaskForQuestion`,
    `PauseTaskForWake`) and drives the expiry window, so a question now survives
    its full TTL regardless of how long the run executed first. `paused_at` is
    runtime state like the wake columns: written only by the pause transitions,
    excluded from the insert/upsert, `UpdateTaskTx`, the clone recipe, and the
    export record.

- **`TaskStreamFrame` was missing five fields the task stream actually sends,
  failing the TypeScript build.** `subagentProgressFrame`
  (`internal/runner/task_stream.go`) forwards `success`, `tokens`,
  `duration_ms`, `note`, and `task` on `subagent_progress` frames — emitted
  by `childProgress.started` / `.finished` — but the client-side type
  declared none of them, so a frame literal naming `success` was a type
  error. `npx tsc --noEmit` already
  reported it; `next build` did not type-check the test file that hit it, so
  the error sat latent until Next 16.3.0 widened the build's type-check
  scope and turned it into a red `Web lint / test / build`. The type now
  matches the projection's key list.

- **MCP client transports hardened against hostile or wedged servers
  (#1108).** One pass over `internal/mcp`, three fixes plus three small ones:

  - **HTTP/SSE responses are bounded at the stdio cap (64 MiB).**
    `parseJSONResponse` decoded the body unbounded and `parseSSEResponse`
    capped only a single SSE line (10 MB), not total accumulation — so a
    user-supplied remote server (#443; `remotemcp.probeServer` and the
    per-run overlay both route through `HTTPTransport`) could stream
    gigabytes inside the 2-minute timeout and OOM the credential-owning
    process. Both paths now read through an `io.LimitReader` at
    `stdioResponseCaptureCap`; an oversized response fails just that call
    with an explicit over-cap error and the transport stays usable.
  - **The stdio write path honors its context.** `t.stdin.Write` ran bare
    under the transport mutex, so a request larger than the 64 KiB kernel
    pipe buffer against a subprocess that stopped reading stdin blocked
    forever — and since `callTool` holds `Server.mu` for the whole call,
    one wedged connector cascaded: every call to that server hung,
    `drainAndClose` blocked, and `Reload` sat in its drain holding
    `reloadMu`, permanently disabling hot-reload. The write now uses the
    same goroutine-plus-select pattern as the read and poisons the
    transport on timeout/cancel, so the existing restart path recovers and
    a reload completes with the wedged server drained and retired.
  - **`Client.Close` retires servers, mirroring reload's `drainAndClose`.**
    Close only closed transports, so an in-flight `callTool` whose
    transport died during Close matched `isTransportDeadError` and
    `restartLocked` respawned a credentialed subprocess *after* Close, with
    nothing left to close it (broker mode's process-group SIGKILL contained
    it; in-process mode leaked it). Close now takes each `Server.mu`, sets
    `retired`, and closes whatever transport the racing call left behind —
    a restarted one included.
  - Lows, same pass: `isTransportDeadError` no longer matches bare `"eof"`
    as a substring (it misread messages like "whereof" and restarted
    healthy subprocesses) — it now checks wrapped stdlib identities via
    `errors.Is` (`io.EOF`, `syscall.EPIPE`, `os.ErrClosed`, …) plus precise
    string forms, with EOF matched only as a standalone uppercase word (the
    legacy `"write |1:"` substring is gone too — its real underliers are
    matched structurally and via "broken pipe"/"file already closed");
    `AddHTTPTools` dedupes repeat registrations instead of appending
    duplicate catalog entries; and `HTTPTransport` verifies the JSON-RPC
    response `id` against the request the way stdio does (a foreign-id JSON
    response is rejected, a foreign-id SSE event is skipped).

- **Five defense-in-depth hardenings across the security-critical packages
  (#1124).** Batched LOW findings from the 2026-08-17 audit — none broke an
  invariant; each closes a residual hazard:

  - **The egress proxy no longer tears down a CONNECT tunnel on the first
    half-close.** The splice loop returned after *either* copy direction
    finished, so a client that legally half-closed its write side after
    sending its request had its response truncated by the deferred `Close`s.
    Both directions are now awaited, and a finished direction propagates the
    half-close to its destination via `CloseWrite` (TCP/TLS) so the peer sees
    the FIN while the other direction keeps flowing. The surviving direction
    is bounded by a 60s drain deadline armed when the first finishes —
    without it, a silent peer that ignores the FIN and never responds/closes
    would pin the tunnel's goroutines and FDs for the process lifetime.
  - **A bundle that declares account-suffix base vars where one is an
    underscore-prefix of a sibling is rejected at load.** The
    `<VAR>_<ACCOUNT>` convention is purely lexical, so declaring both `FOO`
    and `FOO_BAR` on one stdio server meant account `bar` silently overlaid
    `FOO` with `FOO_BAR`'s value (a different credential), and `AccountsFor`
    reported phantom accounts (`SLACK_TOKEN_URL` surfaced `url` as an
    "account" of `SLACK_TOKEN`). The rule is scoped per server — the set one
    overlay actually probes — and fails the load with rename instructions.
    Out-of-repo bundles that declare such a pair will now fail to load; the
    shipped `config/default` bundle is unaffected.
  - **`writeEnvLines` fsyncs the temp file before the rename**, so a power
    loss shortly after `fleet mcp account set` can no longer leave an
    empty/truncated 0600 credentials file behind the atomic rename.
  - **Runtime-acquired credentials join the broker's literal redactor at
    acquisition time.** Boot-time `RegisterEnvLiterals` only knows env-file
    secrets; per-user OAuth bearers, rotated refresh tokens, unsealed OAuth
    client secrets, and unsealed api_key secrets used while the broker serves
    were scrubbed by shape patterns alone. `remotemcp.Service` now offers
    every such credential to a
    secret observer, wired in the broker child to the new
    `mcpbroker.RegisterSecretLiteral`; `redact.Redactor.AddLiteral` is now
    safe to call concurrently with `Redact` (RWMutex) and dedupes, so
    per-turn re-registration is free.
  - **Sandbox pool latency hazards off the turn's critical path.**
    `ensureBridge` no longer holds `c.mu` through its 100ms settle sleep;
    `Pool.Take` reaps over-TTL warm containers asynchronously instead of
    paying up to ~10s of podman teardown before handing out a sandbox; and
    `Take`/`TakePersistent`/`coldStart` now thread the caller's context into
    cold-start construction, so a cancelled turn stops paying container
    spin-up (background warming keeps its own `FillCtx`).

- **Bundle `${VAR}` interpolation now sees the `FLEET_ENV_FILE` env file, and
  an unresolvable reference fails the load (#1123).** The manifest used to
  interpolate BEFORE `config.Load` applied the env file, so a
  `${VAR}`/`${VAR:-default}` in `url:`, `command:`/`args:`, `sandbox.image`,
  or `providers[].base_url` silently baked the default (or shipped a literal
  `${VAR}` token nothing ever re-resolved) whenever the value lived only in
  the env file. `clientconfig.Load` now registers every `${...}` reference the
  raw manifest carries with the `.env` allowlist and folds the env file into
  the process env (same admission + process-env-wins precedence as
  `config.Load`, once per process) before interpolating — every bundle-loading
  entrypoint (serve, mcp-broker, `mcp test`, `validate-config`, `eval`,
  `task run`, the admin CLI) goes through this one seam. `EnvVarNames()` now
  inventories references from ALL interpolated fields, not just env/header
  values, and `${VAR:?...}` validates after the env file applies. The MCP
  `env:`/`headers:` maps keep the #706 lazy-resolution contract unchanged, and
  hot-reload precedence is untouched (env-file values stay reloadable; process
  winners still pin). **Compat:** a manifest that today silently ships a
  literal `${VAR}` outside those lazy maps now refuses to load — including a
  bare `${FLEET_WORKSPACE}`/`${FLEET_TASK_ID}` anywhere except an
  `mcp_servers` env value (header maps only preserve the token verbatim, they
  never substitute it), and a `${VAR:-default}` whose default body nests an
  unescaped `${...}` (the interpolator never expands a default body, so the
  inner reference would ship as a literal whenever the outer var is unset).
  The error names the exact manifest field and variable (use
  `${VAR:-default}` for an optional value, `$${...}` for a literal). A
  manifest whose raw bytes fail to parse is now always a load error, even
  when a substitution would repair the syntax. `fleet validate-config`'s
  credentials check reports absent names whose every occurrence carries a
  `${VAR:-default}` as "manifest defaults in effect" instead of missing
  credentials, so a pristine install stays OK rather than warning forever
  about `"${FLEET_SANDBOX_IMAGE:-}"`.

- **Teams are settable from the UI, so projects can actually be shared
  (#1157).** Two bugs made the shipped team/projects feature unreachable on a
  fresh box:

  - The admin Users tab PATCHes `role` and `team_id` together, and
    `PATCH /admin/users/{email}` refused *any* self-PATCH whose role was not
    `admin`. An `ADMIN_EMAILS` bootstrap admin has the default
    `users.role = 'member'`, so every attempt that admin made to set their **own**
    team was rejected with "refusing to demote your own account" — the first
    operator of a box could not create a team at all. The guard now fires only on
    an actual demotion (self, new role ≠ admin, current DB role = admin, and not
    in `ADMIN_EMAILS` — the env grant survives any column write), and the Users
    tab sends only the fields the admin changed.
  - With no team, "Share with my team" 400s, and the message said "ask an
    admin" — pointing back at the broken path.

  Team membership is now split by what the write grants (ADR-0047):
  **creating a team and leaving one are self-serve** — `PUT /me/team`
  (`{"team_id": "platform"}`, `""` leaves), plus `GET /me` for
  `{email, role, team_id, admin}` — while **joining a team that already has
  members** (or that owns a team-shared project) is refused with `409` and stays
  an admin grant, because a shared `team_id` is what exposes team-shared projects
  and team-visible conversations. The check holds a per-name advisory lock so two
  concurrent creates of the same name cannot silently merge, and reserves names
  held by team-shared projects so orphaned shared memory is not claimable.

  New UI: **Settings → Team** (see your team, create one, leave it, and what a
  team unlocks); the Projects modal offers an inline "create team" where the
  unusable share checkbox used to be; the project-home settings dialog names the
  team it shares with, or points at Settings → Team when there is none.

- **Admin pipeline-metrics and first-run log archival no longer load
  every payload into memory (#1122).** `GET /admin/pipeline-metrics`
  called `GetAllLogs`, which decompressed every stored session — and
  the route comment claimed retention bounded the table, but
  `FLEET_RUN_LOG_RETENTION_DAYS <= 0` (the default) disables pruning.
  The scan is now keyset-paginated; the handler keeps a running
  aggregate and only the most recent `?runs=` summaries. `ArchiveOldLogs`
  pages candidates the same way so enabling archival on a large table
  no longer materializes every live payload at once.

- **Expired approvals are no longer claimable between the deadline and
  the next sweep tick (#1109).** `ClaimApproval` checked only
  `status = 'pending'`, so a still-pending card past `expires_at` could
  be approved and executed until `SweepExpiredApprovals` ran. The claim
  UPDATE now requires `expires_at` to be NULL, 0, or in the future.
  The sweep uses a dedicated `ClaimExpiredApproval` so notification and
  audit still land; default-deny is authoritative at click time.

- **`fleet sched task set-model` no longer silently NULLs every matched
  task's `fallback_model` (#1120).** The CLI passed the
  `--fallback-model` flag's default `""` straight into
  `UpdateTasksModelBatch`, which writes `fallback_model = NULL`
  unconditionally. `set-model --model x` (no fallback flag) now leaves
  existing fallbacks in place; explicit `--fallback-model=""` still
  clears them. `--dry-run` prints the fallback change (or
  `(unchanged)`). Fleet-wide writes require a TTY confirmation or
  `--no-confirm`, matching `fleet restore`. `POST /tasks/model` uses
  the same omit-vs-clear contract (`fallback_model` is now a pointer).

- **Malformed JSON on `DELETE /conversations` no longer wipes every
  unpinned conversation (#1110).** The handler swallowed every decode
  error so a bare (empty-body) DELETE could keep its legacy
  delete-all-unpinned behavior. A client that intended a targeted
  `{conversation_ids: [...]}` bulk delete but sent truncated or
  invalid JSON therefore fell through to the wipe and returned 200.
  Empty body (`io.EOF`) is still the legacy path; any other decode
  error is now 400 with zero deletions. DELETE bodies are also
  subject to the same 1 MiB JSON cap as POST/PUT/PATCH (previously
  exempt, so `DELETE /conversations` and `DELETE /push/unsubscribe`
  could stream an unbounded body into `json.Decode`).

### Security

- **Dropped `github.com/lib/pq` from the build, unblocking the govulncheck
  gate.** Seven advisories landed against lib/pq — GO-2026-6166 (GSS
  authentication completes without mutual proof), GO-2026-6168 (unbounded SCRAM
  iteration count → CPU DoS), GO-2026-6169 (wrong `.pgpass` credential
  disclosed via `hostaddr`), GO-2026-6170/6171 (panics on malformed backend
  frames and RowDescription/DataRow messages), and GO-2026-6172/6173 (memory
  exhaustion before frame-length validation). All seven are `Fixed in: N/A`:
  lib/pq is unmaintained, so no version bump could clear them, and every one was
  call-graph-reachable — `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
  failed, taking `CI` on `main` red with it.

  fleet already opened every connection with pgx (`jackc/pgx/v5/stdlib`), so
  lib/pq survived only in two vestigial roles, both now gone:

  - `pq.Array` in `internal/store`, as a bind wrapper and a scan destination.
    Binding needed no wrapper at all — the pgx stdlib driver implements
    `driver.NamedValueChecker`, so a plain `[]string` is encoded as a Postgres
    array. Scanning is the asymmetric half: `database/sql` receives a `text[]`
    as pgx's array *literal* and cannot convert it to `*[]string`, so the new
    `textArray` scanner delegates to pgx's own array codec rather than
    hand-rolling a literal parser — labels are user-supplied and may contain
    separators, quotes, backslashes, braces, or the bare word `NULL`. Absent-value
    semantics are unchanged: a nil slice still binds as SQL NULL and an empty
    slice as `{}`.
  - golang-migrate's `database/postgres` driver, which imports lib/pq
    transitively. `internal/sched/db` now selects that driver's pgx/v5 fork. The
    two agree on everything fleet depends on — the same `schema_migrations` table
    (`version bigint primary key, dirty boolean`) and the same
    `GenerateAdvisoryLockId`/`pg_advisory_lock` key, so the migration lock still
    mutually excludes across a mixed-version rollout — and it puts the whole
    binary on the one Postgres driver it was already using.

  `go list -deps ./...` now reports zero packages reaching lib/pq (it remains in
  `go.mod` as an unbuilt `// indirect` requirement of the module graph), and
  govulncheck reports no called vulnerabilities. New tests in
  `internal/store/labels_test.go` pin the `text[]` round trip against exactly the
  label values that would break a naive parser.


- **HTTP API hardening (#1112).** Webhook transport errors no longer
  leak the full URL (path + query secrets) into logs or the admin Test
  response. `GET /conversations?scope=team` no longer includes each
  conversation's public `share_token`. `POST /attachments` shares the
  `/chat` rate-limit window. Stream DB-fallback turn lookup is scoped
  by `conversation_id` in the query itself.

- **Web no longer persists the orchestrator bearer token in
  `localStorage` (#1115).** The moc username/password form was already
  gone from the UI, but `orchestratorAuth` still stored the token (and
  attached it as `Authorization`) so any XSS could exfiltrate it. The
  browser now authenticates orchestrator API calls via the same
  httpOnly cookie session as chat. Leftover `orchestratorToken` /
  `userToken` keys are purged on first load after upgrade.

- **Merged-skills materialization no longer uses a predictable path
  under world-writable `/tmp` (#1121).** `materializeMergedSkills`
  wrote to `os.TempDir()/fleet-skills/<hash>` with `MkdirAll 0755` and
  no ownership check, so on a shared box another local user could
  pre-create the tree and plant skill content that fleet would inject
  into agent prompts and bind-mount into the sandbox. The merged tree
  now lives under `$FLEET_DATA_DIR/skills-merged` (user cache /
  uid-scoped temp as fallbacks), and every reuse refuses a path that
  is a symlink, not a directory, not owned by the fleet uid, or
  group/world-writable. An untrusted pre-existing path is a loud
  error and falls back to the bundle's own skills dir — never adopted.

- **IP allowlist no longer trusts the leftmost `X-Forwarded-For` entry
  behind a trusted proxy (#1111).** When the TCP peer was in
  `FLEET_TRUSTED_PROXIES`, `clientIP` took the leftmost XFF value — the
  one an external client controls. A request of
  `X-Forwarded-For: <spoofed-allowlisted>` forwarded by Caddy as
  `<spoofed>, <real>` was therefore filtered on the spoof, defeating the
  operator's network control (including for the pre-auth `/webhooks/` and
  `/auth/verify` surfaces). The chain is now walked from the right,
  skipping hops that are themselves trusted proxies, matching the
  orchestrator's existing `ClientIPFromXFF` convention; an all-trusted
  chain falls back to the TCP peer. The filter is still defense-in-depth
  in front of shared-token auth, so a bypass alone grants no data access.

- **Workspace href rewrite no longer lets `..` segments escape the
  workspace-local prefix (#1113).** `resolveScopedWorkspaceHref` decoded
  then re-encoded each path segment with `encodeURIComponent`, which
  leaves `.` / `..` untouched and had no filter — a prompt-injected
  `[x](../../auth/elcano-login)` rewrote to
  `/api/conversations/<id>/workspace/../../auth/elcano-login`, which the
  browser normalizes into an authenticated same-origin GET at
  `/api/auth/elcano-login`. Any decoded (or double-encoded) `.` / `..`
  segment now bails to the raw href, matching the existing absolute-URL
  fallback. State-changing routes stay POST-only with CSRF, so this was
  a same-origin GET primitive, not a write.

### Fixed

- **A scheduled task that hits its cost/token ceiling is no longer recorded as
  SUCCESS.** (#1105) The scheduled driver returned a nil error for a
  budget-stopped (or cancelled) run, so the worker pool's terminal
  classification fell through to success: the task row read `succeeded`, the
  success notification fired, and an email-triggered run replied "here is your
  result" to the external sender — while none of the finish gates (CanFinish,
  the end-of-run verifier, phone-a-friend) had run. The driver now surfaces a
  budget stop as `ErrCostCeilingExceeded` (the existing `cost_ceiling` failure
  class — terminal failure + failure notification, non-retryable by default)
  and an unattributed cancel as the new `agentcore.ErrRunCancelled` (operator
  stop / ask-pause / self-wake / shutdown attribution still take precedence).
  Partial transcripts persist exactly as before, and a sub-agent child stopping
  at its deliberately sliced ceiling still returns its partial answer to the
  parent instead of erroring.

- Task import `conflict=replace` (HTTP and CLI, #1104) is no longer an unlocked
  read-modify-write. Both paths now go through one storage seam
  (`storage.ReplaceTaskDefinition`) that re-locks the row, re-checks it is still
  pending/scheduled inside the transaction, applies the single shared
  definition overlay (`models.OverlayTaskDefinition` — pinned by a completeness
  test against every `TaskExportRecord` field), and recomputes the dispatch
  state with `models.DeriveDispatchState`. This closes three failure modes: a
  replace racing the scheduler/claimer could rewind a claimed run to
  `scheduled` with its lease nulled (double execution) or flip a completed row
  back to non-terminal, erasing its result; each path hand-rolled its own
  overlay and silently dropped definition fields (`title`, `carry_context`,
  `allow_event_triggers`, the SLA trio on both; plus `sandbox_limits`,
  `output_schema`, recurrence end conditions, `run_if`, and
  `serialization_key` on the CLI); and a schedule-less record replacing a
  scheduled one-shot left the row `scheduled` with a NULL `scheduled_for`,
  which the scheduler can never promote. A replace of a leased, running, or
  terminal task is now refused per-record with a clear error instead of being
  silently applied.

- A critical-tool commitment is no longer stranded when the successful call
  carried corrected arguments. `markPendingCriticalDone` discharged a pending
  action only on an exact `(toolName, argsHash)` match, so a call that was
  blocked pre-audit, rejected on its first retry for bad tool arguments, and
  then **fixed** could never discharge — the winning call hashed differently
  precisely because fixing it is what made it win. `CanFinish` kept answering
  *"Execute pending action(s): [mcp_sendgrid_send_email]"* to a run that had
  already sent the email (SendGrid `202`, message id recorded in the session).
  The run then re-sent, hit the duplicate-send guard, and re-rendered its HTML
  body 110 bytes larger so the payload would no longer be identical — defeating
  a guard whose entire job was to stop exactly that. It spent roughly 25 of its
  27 minutes there and finished `success` having published nothing.
  An exact args hit still wins when the retry really is the same call; otherwise
  the oldest pending entry for that tool is discharged. One entry per success,
  so two distinct pending calls to the same tool still need two successes, and a
  success for one tool never discharges another's.

### Fixed

- **Attachment validation stats the vetted path, not the raw client value.**
  `validateAttachments` already confined chat-attachment paths to the uploads
  root (`filepath.Rel` + `filepath.IsLocal`), but then passed the original
  client-derived absolute path to `os.Stat`, which CodeQL's path-injection
  query kept flagging. The path handed to `os.Stat` is now rebuilt from the
  trusted root plus the vetted relative remainder (`filepath.Join(root,
  rel)`) — byte-identical to the old value whenever the guard passes, so no
  behavior change, but the sink now provably consumes only sanitized data.

- **Sandbox image: patched the open Grype/code-scanning CVEs in pypdf,
  soupsieve, pip, and pygments.** Fedora's RPMs for these pure-Python packages
  lag upstream security releases (pypdf 4.2.0 vs upstream 6.x carried ~30
  GHSAs, soupsieve 2.8.3 two High ones, plus Medium/Low findings in pip and
  pygments). The default-bundle Containerfile now overlays the current PyPI
  releases into `/usr/local` and removes the vulnerable RPM copies, with a
  build-time check that the overlay actually won. The native-extension stack
  (numpy, lxml, pyarrow, …) stays on Fedora RPMs; the overlay is unpinned so
  every rebuild keeps picking up upstream fixes, same policy as the base
  image.

- **`download_url`, `generate_image`, and `xlsx_workbook` no longer touch the
  host filesystem.** #784 moved `view`/`write`/`edit_file` into the sandbox
  FileOp seam but left these three as documented host-staging leftovers:
  `download_url` wrote fetched bytes with host `os` calls, `generate_image`
  read reference images and wrote provider output host-side, and `xlsx` did a
  host zip read/rewrite. All three are now bound to the per-turn sandbox and
  move every file byte through `Sandbox.RunFileOp` (reads enforce size caps
  against true file size; writes are atomic and create parents in-sandbox;
  download_url's collision probe is a sandboxed 1-byte read instead of host
  `os.Stat`). Their network halves stay host-side by design — the ADR-0036
  brokered-fetch class (SSRF guard + `fleet-download://` handle resolution for
  `download_url`, `OPENROUTER_API_KEY` for `generate_image`) — so no
  credential enters the sandbox. A nil sandbox fails closed: there is no
  host-execution fallback, and the schema-only `DefaultTools` bindings error
  if ever invoked. The reserved, unused `xlsx.Values` field is deleted.
  ADR-0036's host-exception classes are narrowed accordingly. (#1083)

- **A typed task key (`fleet_task_…`) could read every task in the fleet.**
  The type is sold as scoped — one key per automation — but every task-read
  surface authorized on `view_tasks` alone, so any such key (and any non-admin
  user) could enumerate and read every task's row, structured output, error
  analysis, export bundle, and upcoming-runs projection (prompts included).
  Task-row visibility now mirrors the run-log model (#980): a principal sees
  its own tasks (`created_by` / `created_by_key_id`) unless it holds a
  fleet-wide grant — the bootstrap `ADMIN_API_KEY`, `PermissionAdmin` carriers
  (typed admin keys, admin-role users), or the explicit `view_all_logs`
  auditor permission. The list filter runs in SQL so pagination totals stay
  honest, and a caller-supplied `created_by` can only narrow further, never
  widen. (#1082)

- **Typed admin keys (`fleet_admin_…`) were rejected on the only routes that
  need them** — you could mint an `admin`-type key carrying `PermissionAdmin`,
  but `AdminAuthMiddleware` (`/keys`, `/users`, `/metrics`, `/admin/*`) only
  accepted the bootstrap `ADMIN_API_KEY`, 401ing a valid typed admin key. The
  middleware now also accepts a hash-verified typed admin key. The gate stays
  type-based and does not widen: a valid key of any other class
  (`task`/`readonly`/`webhook`/legacy `sk-`, even with an admin role) is a
  definitive 403 on those routes, the bootstrap key keeps working, and
  unknown/absent/revoked keys stay 401. (#1081)

- **`FLEET_TEMPERATURE` did not move scheduled-task sampling** — the scheduled
  runner had its own temperature field read from the exact `CUTLASS_TEMPERATURE`
  env var only (and hot-reloaded by that exact name only), so an operator
  following the documented `FLEET_` prefix convention changed interactive
  sampling while scheduled runs kept sampling at the leftover value. The
  separate scheduled-only knob is gone: interactive and scheduled runs now share
  the one `Temperature` field, resolved through the standard `FLEET_` → `CHAT_`
  → `CUTLASS_` alias chain at boot and on reload, so `CUTLASS_TEMPERATURE` keeps
  working as the last-resort alias and existing deploys do not jump temperature.
  (#1079)

- **`FLEET_LOCKDOWN_ONLY` and `FLEET_LOCKDOWN_ALLOWED_MODELS` were silent
  no-ops** — the lockdown knobs were the last bare `CHAT_`-only reads, so an
  operator following the documented `FLEET_` prefix convention got an unsealed
  instance while believing lockdown was on. Both knobs now resolve through the
  standard `FLEET_` → `CHAT_` → `CUTLASS_` alias chain and the `FLEET_`
  spellings are on the env-file allowlist; the `CHAT_` spellings keep working,
  so existing deployments stay sealed. (#1080)

- **The project home's chat list and Sources panel, and the rail's
  move-to-project flow, were dead in real deployments — their Next.js proxy
  routes were never created.** The Go handlers (`GET
  /projects/{id}/conversations`, `GET /projects/{id}/files`, `POST
  /conversations/{id}/project`) shipped fully implemented and tested, and the
  UI called them, but the three `/api/*` proxy routes in between didn't exist,
  so every call 404ed: the Sources panel always rendered its empty state, chat
  previews silently fell back to titles-only, and dragging a chat into a
  project failed with an error blaming the server version. All three proxies
  now exist (thin `chatServerPassthrough` wrappers; the re-file route is
  CSRF-gated like every other mutating proxy) with route tests that import the
  real route modules — the gap survived because the mocked e2e suite stubs
  these paths at the network layer, so nothing exercised the routes
  themselves.

- **The durable turn ledger grew without bound — its retention sweep existed
  but was never wired.** `store.SweepTurnEvents` shipped fully written and
  tested, with a comment claiming it ran "after every successful turn", yet
  nothing called it: every SSE frame a turn ever streamed (`turn_events`),
  plus its `turns` row and `turn_journal` records, lived as long as the
  conversation did. The sweep now actually runs after every turn (real and
  mock paths share one `sweepRetention` helper, alongside the conversation
  and input-queue sweeps), pruning turns terminal for longer than
  `FLEET_TURN_EVENT_RETENTION_DAYS` (default 14; `0` keeps the ledger
  forever). Running turns are never swept, so crash recovery is unaffected,
  and canonical history in `messages` is untouched — the ledger is a
  reattach/replay layer. Also removed the superseded
  `store.MarkRunningTurnsErrored` startup path, which
  `RecoverStrandedTurns` (#798) had replaced.

- **OAuth-connected remote MCP servers could wedge permanently on token
  refresh.** Only `invalid_grant` was treated as terminal; every other
  token-endpoint error was classified transient, so a connection whose *client
  registration* the authorization server had dropped (`invalid_client`) or that
  was barred from the refresh grant (`unauthorized_client`) failed identically on
  every run, forever, with no path to recovery — the connection stayed "connected"
  and the UI never offered a reconnect. These are now terminal
  (`mcpoauth.IsTerminalRefreshError`): the connection is marked `needs_reauth`,
  and reconnecting re-runs dynamic client registration through the normal connect
  flow. Network failures and 5xx stay transient, so a blip still costs nobody a
  manual reconnect.
- **A narrowed OAuth scope grant wedged refresh the same way.** fleet stores the
  scopes it *requested* at connect time and sent them on every refresh, but an
  authorization server may grant a narrower set — and RFC 6749 §6 forbids
  refreshing a wider scope than was granted. The result was a permanent
  `invalid_scope`. `FlowConfig.Refresh` now retries once without the `scope`
  parameter, whose omission §6 defines as "identical to the scope originally
  granted" (mirroring the existing `invalid_target` → retry-without-`resource`
  fallback). The retry fires at most once and only when scopes are configured.
- **A needs-reauth connection claimed the authorization had "expired" whatever
  the cause.** `store.RefreshResult` carries an optional `ReauthDetail` that the
  connections UI renders, so a dropped client registration now says so. The
  authorization server's own `error_description` is deliberately never echoed —
  it is attacker-influenced free text from a user-supplied server.

### Removed

- **`GET /search?type=tasks` no longer answers 200 with a lying empty set.**
  The `tasks` type was a stub from a follow-up that never landed (task-log FTS
  was advertised alongside #308's conversation search but never indexed), and
  `all` was silently an alias for conversations. Search is conversations-only:
  `type=conversations` (or no `type`) works as before, and any other value is
  now an honest 400 instead of a successful-looking search with no hits. The
  web UI never sent `type`, so nothing user-facing changes. (#1076)

- **The reserved `BudgetScopeProject` budget scope is gone.** The constant
  existed only "for later": create always rejected `scope=project` (tasks have
  no project dimension, so a project budget could be recorded but never
  enforced), yet the matcher arm and a leftover-row reporting path kept
  implying a feature that cannot work. The constant, the `BudgetsFor` /
  `groupByForScope` project arms, and the unused `BudgetPrincipals.Project`
  field are deleted; unknown scopes (including `project`) stay rejected on
  create, and leftover `project` rows still list but get no special reporting
  path. Project budgets wait until tasks actually have a project dimension.
  (#1078)

- **The leftover moc `analyzing` task status is retired.** Fleet never wrote
  it — the constant survived #124 only so leftover moc-imported rows still
  decoded, which meant claim/lease/recovery/reporting SQL, the admin CLI, and
  the web status filter all had to special-case a status the current worker
  loop never produces. A one-shot sched migration (063) rewrites any leftover
  `analyzing` rows to `running` (recovery re-queues them on lease expiry
  exactly as before) and rebuilds the serialization-key partial index without
  the retired status; the status constant and every special case are
  deleted. Workers still cannot report unknown/legacy statuses. (#1077)

- **The `cutlass` deprecation shim (`cmd/cutlass`) is gone — its one-release
  deprecation window is over.** The shim only printed a DEPRECATED warning and
  forwarded to the same `internal/taskrun` entrypoint; `fleet task run
  <task.yaml>` is (and remains) the documented way to run one task locally.
  Nothing in the Makefile, CI, or deploy scripts built the shim, so no build
  surface changes — the `CUTLASS_*` env-var aliases are a separate
  compatibility mechanism and are untouched.

- **A dead-code sweep of half-wired surface that parsed, persisted, or served
  data nothing ever read.** None of these change behavior — each removed half
  had no consumer:
  - **Config knobs parsed into fields no code consulted**: `REASONING_ENABLED`
    / `REASONING_EFFORT` (reasoning is actually configured through
    `FLEET_DEFAULT_THINKING_BUDGET_TOKENS`; the fields defaulted and sat),
    `CUTLASS_TASK_MAX_ITERATIONS` (the scheduler's per-task iteration default
    reads `FLEET_MAX_ITERATIONS` — a comment claiming the CUTLASS knob was
    honored is fixed), `TavilyAPIKey` (the search tools read `TAVILY_API_KEY`
    from the process env directly — the env var keeps working), and the
    cutlass-era `InputDir`/`InputFiles` pair whose consumer did not survive
    the v2 fold (bundle-declared `CUTLASS_INPUT_DIR` interpolation for MCP
    servers is a different, live mechanism and is untouched).
  - **`turns.recovered_at`**: written on every crash recovery since #041,
    never read by anything — no scan, no API field, no UI. Recovery provenance
    already reaches consumers through the projected history marker and the
    synthetic `turn.error` frame. Migration 050 drops the column;
    `history_committed_at`, its load-bearing sibling, stays.
  - **`GET /admin/provider-health`**: the only registered route with no web
    caller, no CLI verb, no docs, and no OpenAPI entry. The same circuit
    snapshot is already served through `/healthz` and `/admin/health-summary`,
    both of which have consumers.
  - **Exported dead surface**: `store.RecordPanic`/`CountPanics` (the legacy
    #241 shims; `RecordPanicEvent` is the one production writer),
    `store.ListRemoteMCPShares` (superseded by the by-owner variant the API
    uses), and `webpush.SendToUser` (production sends go through
    `NotifyApprovalRequired`/`SendEvent`; the fan-out logic and its tests keep
    running through the unexported seam).
  - **Web orphans**: two orchestrator proxy routes no client ever called
    (`/api/orchestrator/prompts/export`, `/api/orchestrator/mcp-accounts` —
    the Go endpoints remain public API), and the permanently-disabled "Apply"
    placeholder button on diff blocks ("coming in a future release" since
    #180, with no apply backend behind it).

- **Per-API-key spending caps (`max_cost_per_day_usd` /
  `max_cost_per_month_usd`) and the two endpoints that read them**
  (`GET /keys/{id}/spending`, `POST /keys/{id}/reset-spending`).
  **Breaking at the API surface, but not a behavior change: the caps never
  enforced anything.** Nothing in the unified runtime ever called
  `AccumulateCost`, so the counter `CheckBudget` compared against was frozen at
  zero — a key set to `$50/day` was uncapped, and `/keys/{id}/spending`
  reported `$0.00` for every key, forever. (The moc-era task-completion callback
  that fed the counter did not survive the fold into the in-process runner, and
  the package's own tests passed because they called `AccumulateCost`
  themselves.) `POST /keys` now returns **400** naming the replacement rather
  than accepting a cap it would not enforce. That replacement already shipped
  and is strictly stronger: `POST /admin/budgets` with `{"scope":"key",
  "principal_id":"<key_id>","window":"day|week|month","hard_usd":…}` is
  computed from the persisted metering (no second accounting path), adds week
  windows, token bounds, and soft alerts, and counts chat turns as well as
  tasks. Read live spend with `GET /admin/budgets` or
  `GET /admin/usage?group_by=key`. Old key files load unchanged and no
  migration runs. See
  [ADR-0046](docs/adr/0046-remove-per-key-spending-caps.md).
- **Node-name scopes (ADR-0045).** Every principal carried a list of glob
  patterns — `users.scopes` for accounts, `api_keys.allowed_node_patterns` for
  keys — matched against the name of the worker node a task ran on. The node
  registry went away in ADR-0011, which kept the globs "for forward
  compatibility"; with nothing left to match, they degenerated into an
  authorization surface that enforced nothing while looking like it did.
  `taskVisibleToScopes` returned `true` on every path (its own comment said so)
  behind ten call sites that wrote an unreachable `403 Task not within
  allowed scopes`; `TaskFilter.VisibleToScopes` was consumed by a literal
  `_ = filter.VisibleToScopes`; `GetDashboardStatsForUser` hand-rolled
  `WHERE (created_by = $9 OR TRUE)` — the unscoped query with extra steps; and
  `APIKey.CanTargetNode` was unreachable because every `ValidateKey` call site
  passes a nil node name. All of it is gone, along with `principal.scopes()`,
  `taskVisibleToUser`, `storage.MatchGlob` (whose only caller was
  `CanTargetNode`), `TaskFilter.VisibleToUserID`, the `node_access_denied`
  audit action, and the `allowed_node_patterns` parameters of `CreateKey` /
  `CreateTypedKey` / `ValidateKey`. Migration `062_drop_user_scopes` drops the
  column (a reviewed `migration-lint: allow-dangerous` directive).
  **API surface:** `allowed_node_patterns` leaves `POST /keys` and the API-key
  responses, `scopes` leaves the user create/response bodies and
  `docs/openapi.yaml`, and `fleet-admin sched user add --scopes` is removed. An
  old client still sending either field is ignored rather than having its input
  stored and echoed back; nothing it could previously do changes, because the
  patterns never narrowed anything. A legacy-import dump's `sched.users[].scopes`
  array imports and is ignored. **No authorization boundary is weakened** — the
  permission set, the typed-key gates (trigger slugs, budgets, priority
  ceiling), `ownsTask`, and the creator-scoped workspace/run-log gates
  (ADR-0043) are untouched.
- **Dead code swept from the `internal/` tree** — every function that a
  whole-program reachability analysis (`golang.org/x/tools/cmd/deadcode`, run
  with tests and the `fleet_host_executor` tag) found unreachable from any
  entry point *or* any test:
  - `sched/storage.GetStorage`/`InitGlobalStorage` and
    `sched/apikeys.GetManager`/`InitGlobalManager` (plus their package-level
    `globalStorage`/`globalManager` vars) — process-wide singletons superseded
    by the constructor-injected `Storage`/`Manager` the server actually wires.
  - `sched/handlers.checksumCache` in full — the type, the `Handlers` field, its
    constructor, and the `Clear()` call in `CleanupTempFiles`. Nothing ever
    called `Set`, so the map was permanently empty and the periodic "prevent
    memory leaks" clear was clearing nothing.
  - `sched/models.HashTokenIfNeeded` — passed any 64-char hex input through
    **unhashed**, so a session token that happened to look like a digest would
    have been stored and compared raw. Every caller already uses `HashToken`.
  - `observability.CapturePanic` and its private `panicClass` — a byte-identical
    duplicate of the live `safe.PanicClass`, and the last entry point that
    accepted a *raw* recovered value; real recovery boundaries classify first and
    call `CapturePanicClass`/`CapturePanicClassWithTags`.
  - `agent.ApplyMCPOverlay` — a two-line wrapper only its own test called;
    `ApplyMCPOverlayWithBase` is the real entry point and the tests now use it.
  - `internal/agent/toolresult.go` (+ its test) — a verbatim cutlass port for
    scheduled-driver log formatting that was never wired to a driver.
  - `health.CheckNames` and `sched/db.GetMigrationVersion` (superseded by the
    richer `MigrationStatus` report behind `fleet migrate status`).

- **Conversation folders (#258/#279).** Projects (#509) superseded folders when
  they shipped, and the folders UI came out with them — but the whole server
  half stayed: a `folder TEXT` column, `GET /folders`, `POST /folders/rename`,
  a `?folder=` list filter, a `?folder=` bulk-delete filter, and a `folder`
  field on the bulk PATCH. No client had written any of it since, so every row
  carried the `''` default. All of it is gone, along with
  `store.ListFolders`/`RenameFolder`/`FolderCount` and `ListFilter.Folder`;
  `DeleteAllMatching` and `BulkPatch` lose their folder parameter. Migration
  `049_drop_conversation_folder.sql` drops the column and its index (a reviewed
  `allow-dangerous` DDL — the data is uniformly the default). **Labels are
  untouched**: they are the live, multi-assignment sibling the rail still uses,
  so `internal/httpapi/folders.go` becomes `labels.go` and
  `normalizeAndValidateFolderLabels` becomes `normalizeAndValidateLabels`.
  Filing a conversation used to auto-pin it, so previously-filed chats are
  already visible under Pinned; nothing becomes unreachable. Alongside it,
  `store.Get` and `ListFiltered` now share the one `conversationColumns` list
  and a single `scanConversation`, instead of spelling the column order out
  three times.
- **The in-sandbox `browser` tool (#503) and `FLEET_BROWSER_ENABLED`.**
  **Breaking for deployments that set that flag** — the tool is gone and the
  variable is no longer read (it is also out of the `.env` allowlist, so a
  leftover line is ignored rather than silently inert). The Playwright/Chromium
  reference layer is dropped from `config/default/sandbox/Containerfile`.
  It was never installable on the image fleet ships, its real Chromium path was
  never exercised in CI, and its shipped scope was DOM-only with no credentials
  and no login-walled sites. Browser automation now arrives as an MCP connector
  (e.g. Browserbase) under the existing host-side credential broker.
  See [ADR-0044](docs/adr/0044-remove-in-sandbox-browser-tool.md), which
  supersedes ADR-0022; `docs/BROWSER.md` is removed.
- **Operations Center username/password form.** Cookie/OIDC is the operator
  path. `fleet admin add` mints an unusable random password, so that form
  could not admit a real operator. Backend `POST /auth/login` and the bearer
  proxy stay for API clients. A chat-signed-in visitor who is not provisioned
  here sees a dead-end "ask an admin" card.
- **Budget `scope=project` is rejected on create.** A project budget was
  accepted and listed but never enforced (tasks have no project dimension).
  `POST /admin/budgets` now returns 400 for `scope=project`. Leftover rows
  still list. `user` and `key` scopes are unchanged.
- **Dead `GET /concurrency` Playwright stubs.** The moc-heritage concurrency
  card was already gone; the mocked e2e still answered an endpoint fleet never
  served.

### Added

- **Budget create/delete in the Usage panel** and `fleet sched budget
  list|create|delete`. The panel was read-only; CRUD is no longer API-only.
- **Per-task sandbox limits on the task form** (Advanced: memory / CPUs /
  PIDs) and `fleet sched task set-limits`. Create and edit persist
  `sandbox_limits`; `--clear` reverts to the global defaults.
- **Fire event + sleeping-task list.** A `paused_awaiting_wake` task waiting
  on a named event can be woken from the log viewer. A thin Sleeping list on
  the tasks tab surfaces parked work. Status filters include both pause
  states.

### Fixed

- **The SSE reconnect counter recorded nothing in tests.** `httpapi.Server`'s
  `sseReconnects` is wired by `New()`, but the handler-test fixture builds the
  struct literally and omitted it; `inc` is nil-safe, so every `/stream`
  reattach in every handler test tallied into a nil counter silently. The
  fixture now wires it, and the reconnect outcomes (`buffer_expired`,
  `no_content`) are asserted — previously `SSEReconnectCounts` had no reader at
  all, which is why the reachability scan flagged it.
- **The "never format a recovered panic value" regression test guarded the wrong
  function.** It asserted against `observability.panicClass` (the dead
  duplicate, now removed) while `safe.PanicClass` — the classifier every real
  recovery boundary calls before anything reaches logs or Sentry — had no test.
  The assertion moved to `internal/safe` and covers error/string/nil/struct
  values.
- **Sub-agents finish, and you can see what they are doing.** A spawned child
  ran the ROOT run's finish enforcement: it was refused a finish until it read
  `protocols/self-audit.md` and called `confirm_audit(...)`, and the host-side
  end-of-run verifier re-checked its deliverables. Neither is a child's gate.
  Against a real model, a child asked for a haiku spent 85 s and 31k prompt
  tokens flailing at the ritual and returned it narrated into the answer; a
  child that ran out of enforcement rounds mid-ritual returned nothing, which
  the parent surfaced as `[sub-agent produced no final answer]` — the reported
  "it spawns a sub-agent but never does anything with it". A child now runs
  `agentcore.NewDelegatedPolicy`: the same `ScheduledPolicy`, same gate chain,
  same ceilings and critical-tool audit gating, with **only** the self-audit
  ritual skipped at finish (task-tracker items, pending critical actions, and
  undischarged commitments still gate it, and the parent still audits the
  delegated work). The verifier no longer wraps a child either, and every child
  gets a short "you are a sub-agent" prompt section. The same live delegation
  now takes 13 s and comes back clean. See
  [ADR-0007](docs/adr/0007-governed-sub-agents.md) and
  [docs/SUBAGENTS.md](docs/SUBAGENTS.md).
- **A running sub-agent is no longer a black box.** A delegation showed the
  spawn arguments and then nothing at all — often for minutes — so a working
  child was indistinguishable from a stuck one. The child's own run events are
  now relabeled onto the parent's stream as `subagent.progress` (started / tool
  / tool_result / text / thinking / finished) carrying the parent's tool-call
  id, so: the chat spawn chip opens by default with a live activity panel and
  step count, the thinking indicator names what the CHILD is doing, and a
  scheduled run's child card shows its current action. Related: a scheduled
  child's raw events used to leak unattributed into the parent task's live feed
  (they arrive relabeled now), and the spawn result gained `{steps, tools_used}`
  so a reloaded transcript still shows a child's work trail.
- **Email approval card no longer dumps provider JSON after Send.** A
  successful send resolved the card to "Email sent ✓" and then printed the
  provider's raw JSON payload (status code, message id, HTML-lint warnings)
  under it, which read as an error to non-technical users. The card now reuses
  the transcript chip's humanized outcome — a status badge ("Queued for
  delivery" / "Not sent" / "Already sent") with the full payload behind a
  collapsed "Delivery details" disclosure. Non-JSON results (network errors,
  cancel notes) keep the raw view.
- **Themed deployments no longer flash fleet's default palette on refresh.**
  The brand palette's `html:root[data-theme=…]` rules need the `data-theme`
  attribute, but the script that set it ran via `next/script`
  `beforeInteractive`, which the App Router queues through the framework
  bootstrap — after first paint. Every hard refresh on a white-labeled
  deployment (reported on Reklaim) showed fleet's own colors for a beat. The
  bootstrap is now an inline synchronous `<head>` script, which executes
  during parse, before anything paints; `/scripts/theme.js` is gone.
- **Task SLA config is now validated and editable (#274).** `ValidateSLA`
  existed but no create path called it, so a task with a fail threshold at or
  below the warn threshold (or a non-positive expected duration) was accepted
  and misfired alerts at runtime; every create path (create, edit, import,
  estimate) now rejects it with a 400. Editing a task also actually persists
  `expected_duration_minutes` — the edit form sent it, the backend silently
  dropped it — and an edit that omits it clears the SLA. The web edit modal
  echoes API-set `sla_warn_multiplier`/`sla_fail_multiplier` so a UI edit no
  longer resets them to the defaults.

### Changed

- **`analyzing` is no longer a worker-reportable status.** Fleet never wrote
  it (error analysis is a post-terminal annotation). Leftover imported rows
  still decode, recover, and filter; workers can no longer report it. The
  Operations Center status filter now lists `leased` (a real in-flight status)
  instead of the leftover moc `assigned` value, and the live-log viewer
  attaches to `leased`/`running`.
- **Renamed `web/src/app/lib/mocServer.ts` → `orchestratorServer.ts`.** Same
  helpers; the filename was leftover moc vocabulary.

- **Sub-agents are now ON by default, and the parent agent decides whether to
  use them (#1043, amending ADR-0007).** The enablement compose inverts from
  `FLEET_SUBAGENTS_ENABLED || allow_delegation` (both default false) to
  `FLEET_SUBAGENTS_ENABLED && allow_delegation` (both default **true**), and
  migration 061 **backfills existing task rows to `allow_delegation = true`** —
  existing scheduled tasks start seeing the `spawn_subagent` tool and may
  delegate, bounded by the unchanged walls (depth 1, fan-out 5, ≤10% of the
  parent's remaining budget per child, refuse over-cap, spend charged back to
  the parent ceiling). Fan-out is never forced: registering the tool is the
  feature, and a run that never spawns is a successful use of it. Opt out per
  task with the new "Allow sub-agent delegation" toggle (Task form → Advanced,
  on by default) / `allow_delegation: false`, or fleet-wide via Admin →
  Features `subagents_enabled` / `FLEET_SUBAGENTS_ENABLED=false`. New in the
  same change: **interactive chat** registers the tool when the fleet flag is
  on (child spend shows in the chat cost chip); **typed children** —
  `role=explore` (default) is a read-only research child with write-capable
  native tools stripped, `role=worker` keeps the full roster; **per-child
  isolated workdirs** (`<workspace>/subagents/<child-session-id>/`, returned as
  `workdir` in the tool's JSON result); **child cards** on the task page
  (stored + live) and in the chat transcript (id, role, status, spend) instead
  of raw JSON, each with a **Transcript** disclosure backed by new
  child-transcript endpoints (`GET /logs/{task}/subagents/{child}` on the
  orchestrator, `GET /conversations/{id}/subagents/{child}` on chat — existing
  transcript/ownership gates plus a linkage check and strict id validation);
  and a **best-effort MCP write-tool name denylist** for explore children on
  top of the native strip. Design note:
  [docs/SUBAGENTS.md](docs/SUBAGENTS.md).

- The strong/escalation tier is now `x-ai/grok-4.6` (was `openai/gpt-5.6-sol`).
  This is what `suggest_advanced_model`, the spreadsheet nudge, and the task
  fallback resolve to. Same text+image+file modality with tool and reasoning
  support, so escalation keeps every input type it had; roughly 2.5x cheaper on
  input and 5x on output. **Its context window is smaller — 500,000 vs the
  1,050,000 the previous strong tier carried** — which matters because this is
  the tier a user escalates to for the hardest, usually largest, problems.
  Updated alongside `ADVANCED_MODEL` / `ADVANCED_MODEL_LABEL`, the picker seed
  list, the Operations Center's `DEFAULT_FALLBACK_MODEL`, and the lockdown
  allow-list. No `canonicalUpstream` entry: OpenRouter serves it from xAI alone,
  so there is no provider spread to collapse and the prompt cache is already
  single-upstream. The static context table gained
  `x-ai/grok-4.6` -> 500,000 ahead of the generic `x-ai/grok` -> 131,072 row.
- The recommended everyday model is now `deepseek/deepseek-v4-flash-0731`
  (was `z-ai/glm-5.2`); the strong tier is unchanged (`openai/gpt-5.6-sol`).
  Updated in every place the slug is mirrored: the frontend `DEFAULT_MODEL` /
  `DEFAULT_MODEL_LABEL` and the picker seed list, the Operations Center's
  `DEFAULT_PRIMARY_MODEL` for new tasks, `agentcore.DefaultCoreModel`,
  `config.DefaultTitleModel`, and the lockdown allow-list default. Same
  text-only modality as the model it replaces, with tool and reasoning support,
  so no capability is lost; roughly 4.5x cheaper on input and 7x on output.
- **DeepSeek models are now pinned to the first-party DeepSeek upstream**
  (`canonicalUpstream`, non-strict `Order` + fallbacks). OpenRouter serves this
  family from 28 endpoints whose context lengths span 131K-1M and whose
  quantization spans fp4-fp8, so an unpinned route was neither reproducible in
  quality nor safely sized - on top of losing the per-upstream prompt cache,
  which is the same reason `z-ai/` pins to Z.AI.
- The static context-window table resolves `deepseek/deepseek-v4*` to
  1,048,576, ahead of the `deepseek/` -> 128,000 entry that still covers the V3
  line (the table returns its FIRST prefix match, so ordering is what makes it
  longest-first). Cold-start/offline path only - a running fleet reads the live
  OpenRouter catalog - but without it a cold boot would compact a 1M-window
  default at 128K.
- **A serving-precision floor now backs the DeepSeek pin, and the upstream that
  actually served each step is recorded.** Pinning the family fixed cache
  locality but left quality unguarded: a non-strict pin is `Order` +
  `AllowFallbacks=true`, i.e. a *preference*, so every request was one busy
  first-party endpoint away from being served by anything else in a pool that
  spans **fp4-fp8** — and `provider.quantizations` was set nowhere in the repo.
  Since this family is the recommended everyday default, that fallback path is
  the hot path for ordinary chat turns, and an fp4 serving of a flash-tier model
  degrades in a way that reads as the model being broken (token-level
  misspellings, topic drift, runaway output) rather than as a routing event.
  `upstreamPinFor` now attaches an fp8-and-above allow-list
  (`fp8|fp16|bf16|fp32`, excluding `unknown`, which cannot be shown to clear the
  floor) to the DeepSeek pin; DeepSeek's own endpoint is fp8, so the preferred
  route is unchanged. Families whose pool does not mix precisions carry no
  filter and their requests stay byte-identical. Separately, `updateUsage` read
  only `.Usage.Cost` off OpenRouter's provider metadata and discarded
  `.Provider`, so a turn degraded by a fallback route was indistinguishable from
  a bad model; the run now records `LastServedUpstream`, latches a
  `ServedFallback` flag, and logs one line per upstream transition. Diagnosis
  only — it does not change routing or fail a run.

### Security

- **Un-inverted the event-trigger connector boundary: `allow_event_triggers=false`
  now grants zero connector seats instead of all of them** (#979). The opted-out
  spawn left its `credential_allowlist` unset, and an unset allowlist means
  *inherit global* — every seat — not *none*, so the secure default produced the
  most permissive run in the system, the exact inverse of what the code's own
  comment promised. On an email trigger whose policy approves a bare domain, any
  sender in that domain drove an agent holding every bundle connector, credential
  gating disabled. The opted-out run now persists an explicit deny-all allowlist
  (non-nil, empty), and the scheduled runner honors that value by wiring **no**
  connector rather than wiring them all and rejecting each call: an empty MCP
  selection (which otherwise means *the deployment default set*), a dedicated
  empty MCP client instead of the shared process-wide one, no
  `mcp_list_servers`/`mcp_load_servers` loader tools (calling `mcp_load_servers`
  spawns credentialed subprocesses as a side effect), and no per-user hosted
  remote-MCP overlay — the last of which was attached from the task *owner*, so a
  template that looked connector-less in an audit still reached everything that
  human had personally OAuth-connected. Tests now assert the **negative** (a
  `false` spawn reaches zero seats, checked through the same Gate-3 predicate the
  broker enforces); asserting only that the run "inherited nothing" is what let
  the code and its documentation drift apart, since `len(allowlist) == 0` is true
  for both deny-all and inherit-global. The `#177` webhook path is unchanged — it
  is HMAC-authenticated against a per-trigger secret. See
  [`docs/EVENT-TRIGGERS.md`](docs/EVENT-TRIGGERS.md).
- **Run-log transcripts are no longer readable fleet-wide by every `view_logs`
  holder** (#980). `GET /logs/{task_id}`, its per-attempt `/history` siblings,
  and the live `GET /tasks/{task_id}/stream` all authorized on the `view_logs`
  permission alone, with no scoping to the caller's own tasks — so any principal
  holding it (including a `fleet_task_*` key minted for one automation, and every
  `fleet_readonly_*` key) could read the transcript of every task on the box,
  with task ids enumerable through `GET /tasks`. A transcript carries the run's
  verbatim tool traffic — connector query results, whatever PII the agent
  handled, and cost data — so on a multi-client deployment one leaked or
  over-issued key was a cross-tenant read. The scope check these routes did carry
  was a no-op: `taskVisibleToScopes` returns true unconditionally, and unscoped
  principals skipped it entirely. All four routes now share one gate
  (`internal/sched/handlers/log_authz.go`): `view_logs` plus **ownership** of the
  task (creating user or creating API key — the same predicate that already made
  workspace files creator-private), or the new explicit fleet-wide
  **`view_all_logs`** permission, which `PermissionAdmin` implies and no role
  grants. `POST /keys` accepts an explicit `permissions` array on the legacy path
  so an operator can mint a fleet-wide log auditor without handing out an admin
  key; it 400s rather than silently losing to a `role` or `type` in the same
  request. Workspace files stay stricter (admin-or-creator) and are deliberately
  *not* widened by `view_all_logs`. Because ownership is now load-bearing, the
  one create path that dropped it was fixed too: a task scheduled from chat
  (`schedule_task`) is attributed to the approving user, so its own author can
  read its transcript — and, as a side effect, finally gets the completion push
  notification that path never fired. See
  [ADR-0043](docs/adr/0043-per-task-run-log-scoping.md) for the decision, the
  operational breaks (a readonly key reads no transcripts; a non-admin user keeps
  only its own), and the residual it does not close (the task *row* itself,
  prompt and result included, is still fleet-wide under `view_tasks`).

- **The MCP broker now authorizes, not just transports (#167, ADR-0042).** The
  credential-owning `fleet mcp-broker` child used to bind whatever
  `{server, account}` selection it was sent and run whatever tool a call frame
  named: every gate lived parent-side, inside the address space the boundary
  exists to distrust, so a parent-side gating bug was a total bypass. That was
  not hypothetical — activating the production boundary scrubbed the very
  `cfg.MCPServers` the scheduled agent read its Gate-2 allowlist from, and every
  scheduled run silently lost its tool allowlist (#960) with nothing on the
  credential side looking. The child now re-derives the per-server tool
  allowlist and the enabled-server set from **its own** bundle (refreshed inside
  `Reload`, under the lock that publishes new bases) and treats the parent's
  effective sets, which now cross as `ScopeSpec.Policy`, as narrowing only. A
  selection naming a server the child does not have enabled is refused before
  anything spawns; a call outside the scope's bound set is refused at dispatch;
  the scope's advertised catalog is filtered through the same gate that judges
  its calls; and the per-task credential allowlist (#184) is enforced child-side
  through the same `agentcore.GateMCPBrokerWithAllowlist` the in-process loop
  uses, preserving the nil ("inherit global") versus empty ("deny all")
  distinction. Refusals are value-free tool-level results carrying a stable
  `mcp_broker_policy_denied` marker. The unscoped shared client is restricted
  rather than removed: no production agent path reaches it any more, and what
  does is still bounded by the bundle allowlist. `docs/MCP-BROKER-SCOPES.md` no
  longer says "no authorization boundary is added here", because there is one.
- **Recorded the remote-MCP OAuth control-plane boundary as accepted, in
  writing (#167).** Per-user hosted MCP *runs* resolve tokens in the child
  (ADR-0040), but connect / callback / connection CRUD stay parent-side, so the
  parent installs the at-rest cipher and can decrypt any user's stored
  remote-MCP tokens. `SECURITY.md` now states the threat model plainly —
  compromise of the fleet parent process implies compromise of stored
  remote-MCP tokens — instead of leaving the gap implied by omission.
- **Cleared the three high-severity npm advisories and all seven govulncheck
  findings.** `npm audit` in `web/` reported 3 high-severity vulnerabilities and
  the Go CVE gate reported 7 reachable standard-library vulnerabilities; both
  now report clean. On the web side: `undici` 7.28.0 → 7.29.0 (five advisories —
  response desynchronization via the retry interceptor, cross-user disclosure
  and a parse-time crash via degenerate private cache directives, CRLF injection
  via a blob-like body `type`, cross-user disclosure via whitespace around `=`
  in `Cache-Control`, and cookie attribute injection via unsanitized `domain`),
  `nanoid` 3.3.16 → 3.3.18 (`GHSA-2v37-7h3g-55p8`, a custom generator can loop
  forever at size zero — pulled up by bumping its parent `postcss` 8.5.25 →
  8.5.26, since `npm audit fix` alone would not reach a transitive two levels
  down), and `js-yaml` 4.3.0 → 4.3.1 (`GHSA-5p4m-2wfm-xmqj`, quadratic CPU in
  `!!omap` resolution), which arrives on dev by merging main. All three are
  lockfile-only: no `package.json` range moved.
- **Bumped the pinned Go toolchain 1.26.5 → 1.26.6**, which is what actually
  fixes the seven standard-library CVEs govulncheck found reachable from fleet's
  own call graph — `GO-2026-6218` (`net/url`), `GO-2026-6091` (`html/template`),
  `GO-2026-6090` (`crypto/tls`), `GO-2026-6089` and `GO-2026-5026` (`net/http`),
  `GO-2026-6088` (`encoding/xml`), and `GO-2026-5972` (`encoding/asn1`). The pin
  is bumped in every place it is written down so the gate and a local run agree:
  `go.mod`, `web/go.mod`, the `run.go` target in `.golangci.yml`, the four
  workflows that name a `go-version` explicitly (`ci.yml`, `benchmark.yml`,
  `e2e-canary.yml`, `screenshots.yml` — `dev-ci.yml` reads `go.mod`), and the
  two docs that quote the version (`ONBOARDING.md`, `docs/TESTING.md`). No fleet
  source changed; this is a toolchain bump, not a language-version migration.

### Added

- **A server clock on the Operations Center dashboard**, beside the agent-status
  cards. A cron recurrence fires in the *server's* timezone, not the operator's,
  so `0 9 * * *` ran at 9am somewhere the UI never named — and
  `orchestratorApi.config()`, which knew the zone, had no callers at all. The
  clock now reads the orchestrator's own now from `GET /api/config` (which gains
  `server_time`, formatted in the server's location so the offset travels with
  it, plus `default_task_timezone`, the zone a task with no explicit one lands
  in). The browser measures its skew against that once and ticks locally, so a
  wrong laptop clock can't make the readout lie, a long-lived dashboard survives
  a DST change (hourly resync), and a browser whose zone database has never
  heard of the configured zone still shows the right wall time via the embedded
  offset. Hovering names the zone — and names the task-scheduling default too
  when a deployment has set it differently. An orchestrator that reports no
  `server_time` renders no clock rather than local time under a server label.

### Fixed

- **Multi-day chats no longer stay anchored to yesterday's mailbox
  dates (#1026).** The engine already injected a day-granular Runtime Date
  Context in the system prompt; the model still reused the previous turn's
  `date_from`/`date_to`, so a "check again" on August 13 searched only
  August 12 and missed mail that had already arrived. Every run now also
  gets a structured `runtime_today` + 3-day `freshness_window` in the
  message tail (interactive and scheduled), and mailbox/search MCP results
  whose `date_to` (or `on_or_before`) is before today — or that return
  `matches_found=0` on an exact sender/subject query — are annotated so an
  empty exact hit cannot be treated as proof of absence. OpenX-specific
  discovery, coverage-date parsing, and the pre-send ledger stay in the
  client bundle; this is the engine-side date/search gate. See
  [`docs/RUNTIME-DATE.md`](docs/RUNTIME-DATE.md).
- **The chat composer toolbar now fits on one row on a phone.** The model chip
  carried the full vendor-prefixed catalog name (`Z.AI: GLM 5.2`) plus the four
  cost glyphs, and the toolbar row wrapped — the tools button and the context
  ring dropped onto a second line under the chip. The row no longer wraps: the
  icon buttons keep their fixed size and the chip is the single elastic item,
  so it truncates with an ellipsis instead of pushing anything down. Below the
  `sm` breakpoint the chip also drops the vendor prefix (`GLM 5.2` — the full
  name stays the button's accessible name) and hides the cost glyphs, which are
  still shown on every row of the model picker. Desktop and tablet are
  unchanged: full name, capped at 11rem with an ellipsis, cost tier visible.
- **An approved MCP call runs on the account its turn was using, not the
  connector's default seat (#167).** An approval card outlives the turn scope
  that staged it, so execution reached for the unscoped broker at the default
  bundle seat — a multi-account footgun: a turn on a named account could have
  its approved send go out as a different client. Staging now records the public
  `{server, account}` seat (`approvals.mcp_server` / `mcp_account`, migration
  048) and approve/execute opens a fresh short-lived scope carrying that same
  selection; a seat that no longer resolves fails the approval with the seat
  named rather than silently downgrading. The card shows which account a
  named-seat send will run as. Rows staged before the migration carry no seat
  and execute on the shared broker exactly as before.
- **Approval staging looks at the turn's catalog instead of the process-wide
  default-seat one (#167).** `RunTurn` now hands the stager the turn scope's
  broker, catalog, and selection before any tool can stage a card. Previously,
  during a named-account turn, the staged tool identity
  (`mcp_<server>_<account>_<tool>`) did not appear in the stager's catalog at
  all, so email pre-validation ran against the wrong seat and the staged-call
  resolution could not find its own tool.
- **`fleet update` no longer fails on a box whose distro Go lags `go.mod`.**
  The Makefile now exports `GOTOOLCHAIN=auto`, so every build path fetches the
  pinned toolchain instead of demanding it be pre-installed. `go.mod` pins an
  exact patch release and that pin moves on every Go security release (the
  govulncheck gate reports stdlib CVEs against whatever toolchain built the
  code), which distro packages trail by days to weeks — and Fedora's `golang`
  additionally ships `GOTOOLCHAIN=local` in its `go.env`, turning that lag into
  a hard stop: `go: go.mod requires go >= 1.26.6 (running go 1.24.7;
  GOTOOLCHAIN=local)`. An environment variable outranks that `go.env` default,
  which is what makes the build work on a stock Fedora host. `bootstrap.sh`
  already passed `GOTOOLCHAIN=auto` by hand for exactly this reason, so a box
  would **install** cleanly and then fail **every** subsequent `fleet update` —
  setting it in the Makefile covers `update.sh`, `fleet-upgrade.sh`,
  `bootstrap.sh`, and CI at once, since all of them shell out to `make build`.
  `?=` leaves a deliberately-set `GOTOOLCHAIN` (an air-gapped host pinned to
  `local`) alone. Both upgrade scripts also gained a preflight that names the
  one case the fetch cannot cover — a Go older than 1.21, which predates
  `GOTOOLCHAIN` — rather than letting `make build` fail on a raw version
  mismatch. The operator-facing upshot, now documented in `ONBOARDING.md` and
  `docs/TESTING.md`: you need Go 1.21+, not the pinned patch release.
- **`fleet-upgrade.sh` and `update.sh` no longer abort two steps in when
  systemd is not reachable.** Both resolve `INSTALL_DIR` by probing the unit's
  `ExecStart` with `systemctl show … | sed … | head -n1`, and both run under
  `set -euo pipefail` — so the pipeline's non-zero exit killed the script
  outright instead of falling through to the `/opt/fleet` default that exists
  for exactly this case. The `command -v systemctl` guard did not help: the
  failing condition is not a *missing* `systemctl` but an unreachable systemd,
  which is every container carrying the binary without systemd as PID 1 (and
  `head -n1` can SIGPIPE the probe even where systemd is running). Both call
  sites now end in `|| true`, since the probe is best-effort and the fallback
  already covers "no path found". This was visible as
  `TestFleetUpgradeDryRunSmoke` failing on any container-based dev box while
  passing in CI, where the runners do have systemd — the test now passes in
  both.
- **The task prompt field is adjustable, and no longer opens a long prompt into
  a three-row box.** Editing an existing task showed its prompt through a ~78px
  window: auto-grow only ever ran from the textarea's `onChange`, and a prefill
  fires no change event — so the field stayed at `rows={3}` no matter how long
  the prompt was, which made editing one section of a long protocol a scrolling
  exercise. The prefill (and a template or prompt-library insert) is now sized
  like typed text, and the operator can take the height over two ways: **drag**
  the native grip (the field was `resize: none`), or hit the new **Expand**
  toggle beside the Prompt label for a tall editing pane, remembered per
  browser. Auto-grow yields to either — a chosen height is no longer snapped
  away by the next keystroke — and Expand/Collapse doubles as the reset. The
  auto-grow ceiling stays modest so the fields below stay reachable; a
  deliberate drag goes much further.

### Added

- **Tasks have titles** (`docs/TASK-TITLES.md`). The Operations Center
  identified a task by the first ~80 characters of its prompt, so operators were
  writing a title line at the head of the prompt to tell jobs apart — display
  text smuggled into the model's input. A task now carries an optional short
  title, shown in Recent Tasks (leading the row, prompt demoted to a muted
  second line), the Upcoming timeline and week board, the task-detail header,
  and the SLA report; the task search matches it alongside prompt and id. It is
  **not** the existing `name` column, which is the unique import/export identity
  key *and* is cleared on every recurrence occurrence — a title is non-unique
  and is carried onto every occurrence, re-run and clone, so all the runs of one
  job list under the same label. Optional, single-line, ≤120 characters, never
  injected into the agent's prompt; untitled tasks render exactly as before. A
  bundle template may set `task.title`, and the create form otherwise seeds it
  from the template's name. Inserting a **prompt-library** entry seeds an empty
  title from the entry's name too — for a bundle prompt that is the file's own
  `name:` header, the very line operators were reading off the top of the prompt
  to identify the job — without ever overwriting a title already typed.

- **Run now, for a job that has not run yet.** The Operations Center could only
  *Resubmit*, and only a task that had already finished — so a scheduled job had
  no on-demand kick-off at all. A daily task created in the afternoon could not
  be exercised until the next morning, which made the authoring loop for a new
  job a day long. Recent Tasks rows (and phone cards) now carry a **Run now**
  bolt beside the edit pencil, and the task-detail modal offers the same action
  for pending/scheduled tasks. It posts the existing
  `POST /tasks/{id}/rerun`: a fresh one-off copy that starts immediately and
  leaves the source **and its cron untouched** — the confirm dialog says so, so
  nobody has to guess whether "run now" also cancelled tonight's run. In-flight
  tasks are excluded (a second concurrent copy of a running job is a footgun),
  and the wording splits by state: a finished task is *Resubmitted*, one that
  has never run is *Run now*.

### Removed

- The fast.io native tools (`fastio_find`, `fastio_upload_file`) and the
  dedicated Fast.io system-prompt section. They encoded a vendor integration
  **and** one client's `ELCxxxxx` account-code convention — including a
  `regexp.MustCompile("(?i)\\bELC\\d{3,6}\\b")` — inside the client-agnostic
  engine, against this repo's own rule that client content lives in the bundle.
  They also could not express the shape the data actually has: a date-partitioned
  folder tree, where `fastio_find`'s 25-result cap and opaque parent ids silently
  reduce a month of daily partitions to whichever file was touched last.
  Replaced by `fastio_helpers` in the client bundle (`resolve_path`,
  `list_partitions`, `find`, `upload_file`), which adds path resolution and
  partition enumeration and states truncation instead of hiding it.

  Both native tools were **already dead code**: neither `NewFastIOFindTool` nor
  `NewFastIOUploadFileTool` had a single call site, so nothing registered them —
  while the system prompt instructed every agent with fast.io enabled to use
  them. Agents fell back to raw `mcp_fast_io_storage action=search` with none of
  the mitigations the prompt promised. Deleting them changes no runtime
  behaviour; the prompt no longer advertises tools that do not exist. The
  inline-base64 upload guard now defaults to the blob-flow hint alone and lets a
  bundle name its own path-taking upload via `RemediationHints.NativeUploadTool`.
  (#1016)

### Fixed

- Re-running or cloning a **named** task returned a 500. The copy inherited the
  source's `name`, which carries a partial unique index (it is the import/export
  identity key), so the insert collided with the source row still in the table.
  Both copy paths now clear it — the rule `scheduleNextRecurrence` already
  applied when minting the next occurrence of a recurring task.
- Scheduled tasks created from chat carried no model and dead-lettered on their
  first dispatch, before running anything, with `no model configured for
  scheduled task`. `schedule_task`'s `model` is optional and the agent was told
  it "defaults to the orchestrator's configured model" — a promise nothing kept:
  the chat seam copied the empty value through, `ParentModel` inheritance existed
  only task→task, and a deployment with no `*_TASK_MODEL` had nothing behind it.
  A chat-created task now **inherits the model of the conversation it was
  scheduled from**, which also makes the run reproducible instead of drifting
  with a server env var. Two create-time gates (the `POST /tasks` validator and
  the chat seam, which bypasses it) now refuse a task that has neither its own
  model nor a deployment default, so an unrunnable task fails immediately and
  visibly rather than up to a cron period later in the dead-letter queue. The
  dispatcher's error text and a new boot warning name every accepted spelling.
  (#1014)
- `FLEET_TASK_MODEL` and `FLEET_TASK_FALLBACK_MODEL` were silently ignored.
  These were the only two model knobs in `config.Load` read with a bare
  `os.Getenv("CUTLASS_…")`, so they skipped the `FLEET_`/`CHAT_`/`CUTLASS_`
  alias family that `EnvAliases` advertises for them and `TestEnvAliases`
  pins — an operator following the documented convention got no task model at
  all, and every scheduled task on that deployment dead-lettered. Both now
  resolve the whole family in precedence order; existing `CUTLASS_`-spelled
  deployments are unaffected. (#1015)

- Four chat empty-state cards rendered an empty icon box because the
  `core-icons` sprite had no symbol for the name they asked for: `file-text`
  (the `config/default` and example-config "Summarize a document" card, plus
  `DEFAULT_PILLS` — the hardcoded fallback the web renders whenever the
  `/config` fetch fails, so this one hit a bare install and every degraded
  one), `book-open` (example-config), and `globe` + `mail` (a client bundle's
  browse-a-site and mailbox cards, blank beside the `search` and `bar-chart`
  cards that worked). The four Lucide-derived glyphs are now in the sprite.
  An `<svg><use>` pointing at an undefined symbol id renders nothing and
  reports nothing — no console error, no failed request, no build or test
  failure — so two guards now close that silent-failure hole:
  `web/src/app/spriteCoverage.test.ts` checks every icon name this repo
  references (source literals, the `*_ICONS` lookup maps, and the built-in
  bundle's cards) against the sprite, and `TestRealBundleSanity` gained the
  same check for an out-of-repo bundle's cards when run with
  `FLEET_SANITY_BUNDLE_DIR`. Bundles were not changed: a bundle names an icon
  and the engine owes the glyph, so the fix belongs in the sprite.
- **Every Python error was invisible to the agent.** IPython colours its
  tracebacks; `python_bridge.py` passed `content["traceback"]` through verbatim;
  JSON encoding turned each ESC into `\u001b`; and
  `containsEscapedJSONControl` treated any escaped control below `0x20` as
  smuggled binary. `boundModelVisibleToolResponse` therefore suppressed the
  whole result at `shown_bytes: 0`, staged **no** artifact (the staging branch
  is skipped for binary), and told the agent only that "binary previews are
  intentionally unavailable" — so `run_python` failures came back with the
  exception erased. A production session took two blind `KeyError`s and a failed
  `view_file` before abandoning `run_python` for `bash`, where errors arrive as
  plain text. The same trap ate any `bash` output from a CLI that colours
  (`git diff --color`, `pytest --color=yes`, `ls --color=always`). ESC is now
  excluded from the encoded-binary signal — it is the one sub-`0x20` control that
  ordinary text is full of — and the bridge strips ANSI at the source, so the
  escapes no longer burn model tokens either. Genuine signals (escaped/literal
  NUL, `\b`, `\f`, data URIs, base64-named keys, long encoded runs) still
  suppress.
- `checkCommandSafety` rejected any bash command containing `":-"`, `":+"`,
  `":?"`, `"##"` or `"%%"` once a `${` appeared anywhere in it — and the test was
  against the **whole command**, not the inside of the expansion, so one
  unrelated `:-` on the line poisoned every expansion. In production
  `echo "${CUTLASS_INPUT_DIR:-not set}"` was refused, which is the ordinary way
  to read a variable with a default and what every `set -u` script needs. The
  rejection bought no safety: those forms only substitute and trim text, and the
  execution vectors inside an expansion (command substitution, backticks) are
  matched independently in the same loop — `${` falls through with `continue`, so
  `${VAR:-$(id)}` is still caught. `${!VAR}` stays blocked as genuine name-level
  indirection. The guard had no test coverage at all; it does now.
- The generic sandbox image was missing `file` and `which`. Two agent runs lost
  a chained command each to `exit 127` reaching for them (`curl … && file x`,
  `which node && node --check`). Neither was withheld on purpose.
- The interactive critical-tool gate staged one approval card per call with no
  awareness of the cards already waiting, so a turn could park two competing
  writes on the same MCP server. Because a card freezes its tool's arguments
  until the user clicks Approve, the second write is computed against state the
  first is about to change. A production session lost a Pages deploy this way:
  `mcp_pages_patch_page` was staged, the model read "Do NOT retry" as a rule
  about that tool name, rebuilt the same change as a full-file upload, and
  staged `mcp_pages_deploy_page_upload` for the same page carrying the
  pre-patch `expected_version`; the user approved both, the patch landed, and
  the upload was rejected as stale after two CSV ingests, a 14-row
  reconciliation, and 78 KB already transferred.
  `checkCriticalToolApproval` now refuses to stage a *different* critical tool
  on a server that already has a card pending, naming the pending action and
  its approval id. Keyed on `sameToolServer`, so a repeated tool name still
  stages (the batch case — N independent records — has no such coupling) and
  unrelated servers are unaffected; the pre-approval and pre-denial sentinels
  leave no card pending and so reserve nothing. The `APPROVAL_REQUIRED` text
  also now forbids re-routing the same change through a different tool, which
  is the loophole the model actually used.
- `fleet update` installed the new binaries and web build BEFORE the
  sandbox-image gate, so the gate's fail-closed die aborted with new code
  already on disk while the old service kept running — a silent, deferred
  inconsistency (the next restart, crash, or reboot would run code no update
  ever finished deploying) that contradicted the die message's implication
  that nothing had changed. The sandbox step now runs before the
  build/install step, so the abort leaves the box coherent: old binaries on
  disk, old service running. Separately, update.sh resolved the bundle's
  `${FLEET_SANDBOX_IMAGE:-}` image reference against the update shell's
  environment, while the service resolves it from
  `EnvironmentFile=/etc/fleet/fleet.env` (which update.sh deliberately never
  sources): a value set only in the env file forced a pointless multi-GB
  on-box build on every update and could trip the new die spuriously, while a
  value exported only in the operator's shell silently skipped the whole
  sandbox step — absence probe included — recreating the
  boots-clean-breaks-on-first-tool-call failure the gate exists to prevent.
  The reference is now read from the service's env file using doctor.sh's
  `env_get` idiom, matching what the restarted service will actually see.
- The CSRF canonical-origin hardening locked operators out of a box reached
  over an SSH tunnel. `verifyOrigin` compared the Origin host strictly against
  the configured `NEXT_PUBLIC_PUBLIC_ORIGIN` (which bootstrap writes on every
  deploy), and it guards every mutating route including the login form — so a
  browser at `http://localhost:3000` tunneled to a `--domain` box got 403 on
  every POST, presenting as a total auth outage. A loopback Origin
  (`localhost` / `127.0.0.1` / `[::1]`, any port — exact hostnames only, so a
  `localhost.evil.example` lookalike does not qualify) is now also accepted
  when it exactly matches the connection's own Host header: that pair is only
  producible by a browser genuinely connected to the box's loopback (the
  tunnel), never by a victim's browser talking to the real deployment, and
  x-forwarded-host stays ignored so a forwarded-header attack from a remote
  origin still fails. A mismatched local port (a different origin) is still
  rejected, and the server-side 403 log now names the expected vs. actual
  host so the failure is self-diagnosable.
- An async run_if gate's settle write could clobber a concurrent reschedule or
  edit. Both settle paths conditioned only on the task still being `scheduled`
  — a status an edit legitimately keeps — and the bounded async pool stretched
  the dispatch-to-settle window from milliseconds to the gate's full runtime
  (up to 300s). Concretely: a task postponed by an operator while its slow
  gate evaluated would run tonight anyway on the stale pass verdict, and a
  stale decline overwrote the operator's new `scheduled_for` with its retry
  backoff. Every gate settle write (promote to pending, skip record, and the
  recurrence-ended cancel) is now a compare-and-swap that also requires the
  row to still carry the `scheduled_for` the evaluation was dispatched
  against, so any interleaved edit or reschedule wins, the stale verdict is
  discarded (and logged as such, without counting a skip that never
  happened), and the next due tick re-evaluates the task's current
  definition.
- `Scheduler.Stop`'s run_if gate drain could panic the process during graceful
  shutdown. Stop closed the stop channel and immediately waited on the gate
  WaitGroup, but nothing waited for the scheduler's run loop to exit — a tick
  already inside the loop body kept running and could dispatch another gate
  evaluation (`WaitGroup.Add`) concurrently with that wait, which
  sync.WaitGroup forbids. The resulting misuse panic fired in Stop's drain
  goroutine, which has no recover (the per-tick recover covers only the tick),
  so it killed the process mid-shutdown and skipped every remaining deferred
  cleanup in main. The run loop now signals completion on return and Stop
  waits for it to exit before draining the gates — inside the same 5-second
  bound as before — so no new dispatch can ever race the drain.
- The MCP stdio transport read each response line from a server subprocess
  with no ceiling. In broker mode those servers run host-side, so a single
  data-driven oversized response — a connector returning a giant query result
  or a fetched page — was buffered whole in the fleet process's memory before
  any downstream truncation applied: one response could OOM the process and
  take down every user. This is the same risk class already capped on the
  sandbox bridge's response read, and the stdio transport now follows that
  pattern at the same 64 MiB ceiling: a line past the cap is drained to its
  delimiter — the stream stays framed and the healthy subprocess is not
  restarted — and the call fails with an explicit over-cap error rather than
  a silently truncated, plausible-but-wrong result.
- The storage-quota probe's inconclusive path re-probed on every container
  creation with no cap or backoff. Right direction — the earlier fix stopped a
  cancelled first probe from latching "unsupported" for the process lifetime —
  but on a degraded host (podman hanging) every creation then paid the
  serialized 30s probe, added straight to turn-start and scheduled-run
  latency, for as long as the host stayed degraded. Consecutive probe
  *timeouts* (inconclusive with a live caller context: the host being slow,
  not a user cancelling a turn) now cap at three, after which the pool treats
  the driver as unsupported for a 5-minute cooldown before re-probing.
  Containers created inside the window omit only the writable-layer quota —
  the per-file ulimit still applies — and a cancelled turn still never counts
  toward the latch, so a genuinely inconclusive answer still never latches
  permanently.
- The Operations Center rendered its username/password login card whenever the
  initial `/me` probe failed for any reason other than 403 — including the 500
  the orchestrator deliberately answers when its fail-closed session-epoch
  lookup can't reach the chat DB, and thrown network failures. During a chat-DB
  blip every user loading the page was therefore shown a login form instead of
  an error. No session was destroyed — the cookie and the stored bearer
  survived, and a reload after recovery restored the dashboard — but the page
  invited people to type credentials mid-incident: the same conflation of
  "unauthenticated" with "backend down" already fixed for the chat plane. The
  probe now applies that plane's verdict rule (`bootstrapFailure.ts`: only
  401/403 are auth verdicts); a 5xx or a network failure surfaces as a distinct
  "can't reach the server" retry notice and leaves the stored bearer untouched.
- One slow `run_if` gate degraded scheduling for the whole box. The scheduler
  tick ran task promotion, lease recovery, starvation promotion, paused-task
  expiry, and the wake sweep sequentially on one goroutine, and gates were
  evaluated inline during promotion — so a single admin-authored gate pointing
  at a hung dependency (`timeout_seconds` up to 300) delayed ALL scheduling
  and crashed-worker lease recovery by its full runtime, while the 30s ticker
  silently dropped ticks. Gates are now evaluated on a small bounded pool of
  goroutines and settled (promoted or skipped) asynchronously — the tick never
  waits on a gate, a task whose gate is still running is not re-dispatched,
  and one whose slot isn't free simply stays due for a later tick. The cheap
  recovery sweeps also run before promotion now, so a promotion regression
  can never starve lease recovery. Separately, a declined one-shot gate used
  to re-run its host command every 30s tick forever; a decline with no next
  cron tick now backs off exponentially on the already-tracked `skip_count`
  (30s doubling to a 30m cap), so a permanently-false condition costs ~2
  commands an hour instead of 120. Shutdown no longer races those async
  settles: `Scheduler.Stop` now waits up to 5s for in-flight gate evaluations
  to land their promote/skip writes; a gate still running at the bound is
  abandoned to finish on its own (its settle write is conditional, and a task
  left `scheduled` is simply re-evaluated after restart), so a slow gate can
  never extend shutdown by its full runtime.
- Editing a webhook trigger template turned it into a one-shot run. The task
  edit's status recompute derived only from `scheduled_for`, so saving any
  edit to a template (`scheduled`, nil `scheduled_for` — inert by
  construction) flipped it to `pending` and the worker pool executed the
  template itself once, outside any trigger firing. Same root cause and same
  fix as the gated-task edit bypass under Security below: the edit path now
  recomputes dispatch state with the shared `models.DeriveDispatchState`
  rule, which keeps a webhook template parked inert.
- Task export silently dropped `run_if`. The portable definition record had no
  field for the pre-run gate, so a box migration or backup-restore converted
  every gated task into an unconditional one with nothing flagging it — tasks
  whose gate suppressed runs under bad conditions started running
  unconditionally on the new deployment. The gate is now part of the export
  record and survives the round-trip (including `conflict=replace`, which
  overlays it like every other definition field). Because a `run_if` command
  executes on the host as the fleet user, importing a record that carries one
  requires admin permission — the same boundary as authoring one, checked
  up front before any record is written, so import cannot be a path around
  the create/edit admin gate.
- The pre-submission cost forecast (`POST /tasks/estimate`) was dead for every
  cookie-path Operations Center user — "Estimate failed: Unauthorized" — even
  though the sibling comment claimed the endpoint honored the Next-proxy
  header-trust identity. Its auth was a hand-rolled copy of the task-create
  check that predated header trust (#157) and never learned it; it failed
  closed, so nothing was exposed, just broken. The copy is gone: the endpoint
  now shares `authorizeTaskCreator` with `POST /tasks` and `/tasks/batch`, so
  the header-trust semantics and the session-epoch revocation gate apply
  identically to all three creation-shaped endpoints and cannot drift again.
- Every masked MCP-broker failure was undiagnosable. The credential owner
  replaces any operational error with a fixed `mcpbroker: credential-owner …`
  sentence before it crosses back to the parent — correct, because the real error
  can embed connector stderr, resolved URLs, or Authorization headers — but that
  replacement was the *only* thing that happened to it, so the detail existed
  nowhere: not in the parent, not in the broker process, not for the operator.
  A connector answering `Unknown tool: x` and a genuinely revoked credential both
  reached the agent as the same sentence, and the difference was unrecoverable
  without the upstream's own logs. In one observed case an agent retried four
  times and concluded "ops-side credential fix" for what was a version skew
  between a cached `tools/list` and the process serving the call. The broker now
  logs each masked failure host-side with its server and tool, scrubbed through
  `internal/redact` with the connector env registered as literals — so a bare
  high-entropy token quoted back by a connector is replaced by value, not merely
  when its shape matches a vendor pattern. The reply to the parent is byte-for-byte
  unchanged, and credentials-never-in-logs still holds
  (`internal/mcpbroker/redact.go`).
- Five sandbox lifecycle robustness gaps, all host-side and none affecting the
  container's isolation posture:
  - **One cancelled turn could permanently disable the `--storage-opt` disk
    quota.** The pool latched the boot probe's answer with a `sync.Once`, so a
    first probe running under an already-cancelled context cached
    "unsupported" for the process lifetime and every later container ran with
    no writable-layer quota. A probe cut short by its context is now
    inconclusive: that container safely omits the layer quota and the next
    creation re-probes; only a completed determination is memoized.
  - **The run_python bridge response was read into host memory with no
    ceiling.** The bridge caps its own stdout/stderr/result captures, but
    `vars` extraction is not size-bounded, so a cell returning a huge variable
    inflated host RSS without limit — unlike bash output, which
    `BashOutputCaptureCap` bounds. The response line is now capped at the same
    ceiling; overflow drains to the delimiter (the session stays framed and is
    kept), and the call fails with an explicit truncation error instead of a
    bare JSON parse error.
  - **A wedged bridge exec could block every operation on its container
    forever.** `terminateBridgeLocked` runs under the bridge mutex and waited
    on `cmd.Wait()` unboundedly — anything holding the exec client's pipes
    (the hazard `BashWaitDelay` documents) stalled bash, run_python and file
    ops behind it indefinitely. The bridge exec now carries a WaitDelay and
    the post-SIGKILL reap wait is bounded; past the bound the state is cleared
    and the abandoned wait finishes on its own.
  - **Routine teardown of an already-exited container logged a spurious
    error.** The "already gone" suppression in `close()`/`killContainerNow`
    predates podman's actual state error — `can only kill running containers.
    <id> is in state exited: container state improper` (verified verbatim on
    podman 5.8.2) — so the normal exit-vs-`--rm`-removal race logged "kill
    unconfirmed" on every occurrence. The matcher now recognizes the dead
    states (exited/stopped) while a paused container — frozen, not gone —
    still counts as unconfirmed.
  - **Crash-orphaned bridge/seccomp temp files leaked permanently.** Only the
    graceful close path removes the bridge-script and seccomp files written
    into BridgeDir, so every non-graceful exit leaked them forever. Boot now
    sweeps files matching the two CreateTemp patterns, age-bounded (1h) so a
    start still in flight — here or in a sibling instance sharing the
    directory — is never raced, alongside the existing container orphan prune.
- With the Go backend down, the chat client read the resulting 502 as an
  expired session and redirected to `/login`, which sent it straight back to
  `/chat` — an endless bounce instead of an error. Unauthenticated (401/403)
  and unreachable (502/503/504, network failure) are now distinguished, and the
  latter renders a retry state naming the real problem.

- A login attempt refused by the `/auth/verify` rate limiter surfaced as "the
  chat server isn't reachable" rather than the throttle message the backend
  actually returned.
- An env-file override of a `${VAR:-default}` MCP env/header key was silently
  ignored on the standalone paths (`fleet serve`, `task run`, `mcp test`,
  `validate-config`): the bundle manifest interpolates BEFORE `config.Load`
  applies the `.env` file, so the literal default was baked in at load — and
  the override's var name did not even survive the `.env` allowlist, because
  `EnvVarNames` scanned the post-interpolation values where the token no
  longer existed (the fleet#706 residual). Connector env/header values (and
  inline `http_tools` headers) now keep their raw `${...}` text through the
  load and resolve against the live process env at catalog-build time, after
  the `.env` file is applied, so the env-file value wins over the default.
  The same single-pass resolution repairs the `$${` escape for those fields,
  which the load pass used to consume so the spawn pass expanded — and
  blanked — the author's escaped literal. `${VAR:?...}` still validates at
  load, against the pre-`.env` process env.

- `${FLEET_WORKSPACE:-...}` / `${FLEET_WORKSPACE:?...}` — and any other
  colon-suffixed spelling of a reserved runtime token — bypassed the
  reserved-token guard, which matched only the bare form: the token was
  resolved from the process env (an exported `FLEET_WORKSPACE` could hijack
  it) or its default was baked into the manifest, silently defeating the
  launch-time workspace substitution. Non-bare reserved-token spellings now
  fail the bundle load with an error naming the contract
  (docs/MCP-BUNDLE-ENV.md), and the reserved names are no longer registered
  into the `.env` allowlist as if they were ordinary env vars.
- `sudo fleet update` rebuilt the sandbox image into root's podman store, which
  the `User=fleet` unit can never read — root's rootful store is a separate
  namespace from the service user's rootless one, which is why `bootstrap.sh`
  builds through `runuser`. Every rebuild an operator triggered through
  `fleet update`, or by following doctor's own "build it: sudo fleet update"
  hint, was therefore a no-op for the running service. Root-run builds now
  land in the service user's store, and the fix sits in
  `scripts/build-sandbox-image.sh` so every root caller inherits it.

- The sandbox-image rebuild gate compared only the Containerfile hash, so a
  bundle that renames `sandbox.tag` with byte-identical build instructions
  skipped the build. fleet does not verify the image at boot, so the box came
  up healthy and then failed every sandboxed tool call against a tag that was
  never built. The gate now also tracks the resolved tag and rebuilds when the
  expected image is absent from the service user's store.

- `fleet update` restarted the service even when the sandbox build it had just
  attempted failed and the resolved image existed nowhere in the service
  user's store — a `sandbox.tag` rename plus one transient build failure left
  the box reporting healthy while every sandboxed tool call failed, until a
  human noticed. The update now refuses to restart in that state, spelling out
  the recovery commands; a failed build only warns past when the
  currently-resolved tag still exists (Containerfile changed under the same
  tag — the previous image is stale but serviceable).

- The update's sandbox-store probe ran rootless podman as the service user
  without pre-creating `/run/<user>` — tmpfs, present only while the unit's
  `RuntimeDirectory=` keeps it alive — so during a stopped-unit maintenance
  window `podman image exists` failed environmentally and the gate read that
  as "image missing", burning a spurious multi-GB rebuild. The probe now
  pre-creates the runtime dir the way the build path already did, and only
  podman's positive "not found" (exit 1) counts as absent; any other podman
  failure leaves the image as-is and says so.

- The sandbox rebuild gate keyed on the manifest's `sandbox.tag` even when the
  bundle resolves `sandbox.image` to a prebuilt ref the service pulls directly
  (image wins over tag), so the first update after a client switches to
  registry-published images performed one pointless multi-GB on-box build that
  nothing reads. `update.sh` now mirrors `bootstrap.sh`'s
  `resolve_sandbox_image` skip — one rule, same interpolation.

- The update's unit-adoption loop covered only `fleet.service` and
  `fleet-web.service`, so a fix to the shipped backup units reached
  provisioned boxes only via doctor, never via the update path operators
  actually run on release. The loop now also adopts `fleet-backup.service`
  and `fleet-backup.timer` when they are installed on the box — without any
  restart, matching doctor's rule: the daemon-reload alone re-arms a rewritten
  timer's schedule, and restarting the oneshot would run a backup immediately.

- The production MCP-broker boundary refused to boot any bundle that wires the
  documented cutlass-family connector contract: the connector/parent env
  overlap guard treated the whole `CUTLASS_` prefix as parent-owned, but fleet
  itself designates `CUTLASS_RUN_WORKDIR` / `CUTLASS_MOC_TASK_ID` /
  `CUTLASS_REPORT_DIR` / `CUTLASS_INPUT_DIR` as bundle-to-connector wire keys
  (`internal/agentcore/mcp_workspace.go`), and operator passthroughs such as
  `CUTLASS_ALLOWED_DIRS` / `CUTLASS_USER_AGENT` are never resolved by the
  parent. Startup failed with "connector environment overlaps parent-owned
  configuration" on exactly the manifest shape the contract tells bundles to
  write. The guard still fails closed on overlap with parent runtime settings,
  provider keys, webhook signing secrets, and the `CUTLASS_*` names the parent
  itself resolves — those are now enumerated explicitly instead of claimed by
  prefix.

- The `--storage-opt size` support probe assumed `/usr/bin/true` exists in the
  sandbox image — a **client-bundle artifact** free to change its base, where a
  busybox-based bundle has `/bin/true`. Such a bundle failed the probe on every
  host, so the writable-**layer** disk quota was silently dropped with only a log
  line. Scope of that loss, stated precisely: the per-file `--ulimit fsize` cap
  still applied, total *workspace* bytes are unbounded either way (a bind mount
  is outside any storage-driver quota), and under `--read-only` plus size-bounded
  tmpfs the writable layer is nearly unwritable anyway — so this restores
  defense-in-depth, not a cap that was holding back live disk exhaustion. Podman
  validates `--storage-opt` at
  container-create time and only then execs the command — verified on an ext4
  host, where a nonexistent entrypoint still reports the *quota* error — so a
  failure to exec now counts as quota-accepted. The classification is
  deliberately narrow in the fail-closed direction: misreading a real quota
  rejection as an exec failure would pass `--storage-opt` to every container and
  break every start, which is worse than losing the layer quota.

- **`FLEET_DEFAULT_NETWORK_MODE=allowlisted` was entirely non-functional on a
  stock modern host.** Allowlisted egress emits
  `--network=slirp4netns:allow_host_loopback=true` to reach the host-bound
  proxy, but Podman ≥ 5.0 defaults to `pasta` and a stock modern box (verified on
  Fedora 44) ships
  pasta *without* `slirp4netns` — so every container start failed. It failed
  closed (a container that will not start cannot leak), but late and
  repeatedly: boot succeeded, the proxy bound, the log announced "egress
  filtered to […]", and then every interactive turn, scheduled task, and
  approved-bash call errored. `PreflightAllowlistedNetwork` now asks Podman at
  boot whether it has the helper (`podman info`, image-independent — a container
  probe would abort boot on any bundle whose rootfs lacked the probe command,
  since the netns is configured before the exec) and fails boot closed if it
  does not, naming the helper and the fix; `fleet validate-config` runs the same
  check; `scripts/bootstrap.sh`
  installs `slirp4netns` so a bootstrapped box supports the mode. Teaching
  `networkArgs` to use pasta's `--map-host-loopback` instead is deliberately
  deferred — pasta exposes the host at a different gateway address, so it also
  changes the proxy URL and `NO_PROXY`, and that belongs in its own reviewed PR
  with rootless-host verification (recorded in ADR-0012).

- **`validate-config` failed the persona knob that only degrades, ignored the one
  that is fatal, and `FLEET_PERSONA` did nothing.** The manifest check resolved
  `PERSONA` against the bundle ROOT and made a miss a blocking failure, so any
  bundle not shipping `personas/assistant.yaml` — the loader's built-in fallback
  — got a red `✗ manifest: default persona personas/assistant.yaml missing`
  unless `PERSONA` happened to be exported in the shell running the check. Our
  own production bundle, which ships `personas/victoria.yaml`, failed that way;
  a preflight that reports a bundle as broken when it boots fine teaches
  operators to ignore the whole report. Meanwhile `PERSONA_DEFAULT` — the
  interactive default, and the one a miss actually breaks — was never checked at
  all. Both are now resolved the way their readers resolve them (by basename
  inside the bundle's `personas/` dir, so `victoria`, `victoria.yaml` and
  `personas/victoria.yaml` all find the file), each at the severity its runtime
  earns: a missing `FLEET_PERSONA_DEFAULT` is a blocking `✗`, because
  `agent.Manager` returns an error when it cannot read the persona and the turn
  fails, and every new conversation starts on that name; a missing
  `FLEET_PERSONA` is a non-blocking `⚠`, because the scheduled driver ignores the
  read error and only loses the persona's expertise block. Either way the report
  names the knob to set and lists the personas the bundle *does* offer, in the
  shape that knob takes. Separately, both knobs were read with a bare
  `os.Getenv`, outside the prefix alias machinery: an operator following the
  documented `FLEET_` convention got silence and the built-in default, while the
  broker already treated the names as parent-owned. `FLEET_PERSONA` /
  `FLEET_PERSONA_DEFAULT` (and their `CHAT_`/`CUTLASS_` spellings) are now read
  first, with the unprefixed names kept as the back-compat fallback and the
  canonical spellings added to the env-file allowlist so they survive a
  `FLEET_ENV_FILE` load. The two shapes are now written down where personas are
  documented: `FLEET_PERSONA_DEFAULT` takes a persona *name*, `FLEET_PERSONA` a
  bundle-relative *path*.

- Empty MCP tool-call arguments crossed the broker wire as nil
  (`args,omitempty` drops empty maps) and were then marshaled onward as
  `"arguments": null`. Strict MCP servers reject null arguments with `-32602
  Invalid params` — pages.elcanotek.com refused every no-arg tool
  (`mcp_pages_list_templates` and friends) — and because the credential owner
  masks operational errors, the refusal reached the agent as `mcpbroker:
  credential-owner call failed` and read like a credential problem. A nil
  arguments map is now normalized to an empty object at both the broker
  boundary and in `callTool`, pinned by regression tests at each layer. (#958)

- `bootstrap.sh`'s no-`--domain` hint told only an operator proxying from
  ANOTHER host to set `NEXT_PUBLIC_PUBLIC_ORIGIN`. A same-host TLS proxy
  operator following it verbatim shipped a web tier that redirects every login
  to `http://localhost:3000` and mints session cookies without `Secure` — the
  configured origin drives both decisions (`web/src/app/lib/auth.ts`), same
  host or not. Diagnosis was non-obvious because Next inlines `NEXT_PUBLIC_*`
  at build time: editing the env file changes nothing until web/ is rebuilt.
  The hint now tells EVERY custom-proxy front to set the origin in
  `fleet-web.env` and rebuild + restart (`scripts/update.sh` does both),
  keeping `FLEET_WEB_HOST=0.0.0.0` as the extra step only for a proxy on a
  different host.

- A hand-quoted value in `fleet.env` (`FLEET_BACKUP_DIR="/mnt/x"` — legal in a
  systemd `EnvironmentFile=`, which strips the quotes) made every later
  bootstrap re-run die with the misleading `FLEET_BACKUP_DIR must be an
  absolute path (got '"/mnt/x"')`: the re-run read the value back verbatim,
  quotes included. Re-running bootstrap is the documented idempotent upgrade
  path, so a legal env file blocked provisioning until someone spotted the
  quotes. The backup read-backs now go through the same quote-stripping
  `env_get` that `doctor.sh` already uses.

- `validate-config`'s persona inventory accepted `.yml` files that no persona
  roster can load — `agent.ListPersonas`, the task-create catalog and the
  interactive loader are all `.yaml`-only (the loader force-appends the
  suffix) — so a `.yml`-only bundle looked persona-equipped and the report
  offered a remediation that loops: setting the knob to the suggested name
  still resolves to a `.yaml` file that does not exist, while chat's roster is
  empty. The inventory is now `.yaml`-only to match the readers, and such a
  bundle reports as shipping no personas.

- Run as root on a provisioned box (the unit installed), the live-sandbox
  harnesses (`scripts/e2e-boot-server.sh`, `scripts/run_workflow_live.sh`)
  built the sandbox image through `build-sandbox-image.sh`, whose root path
  delegates the build to the fleet unit's `User=` — correct for `sudo fleet
  update`, wrong here, because the harnesses probe and run fleet as the
  INVOKING user, whose image store is a separate namespace. Every root run
  burned a multi-minute rebuild into a store it never reads and then failed
  anyway — an invitation to "fix" it by weakening the delegation that protects
  the real deployment. Both harnesses now force a local build by pointing
  `FLEET_SERVICE_NAME` at a unit that does not exist, with a comment saying
  why the delegation must stay.

### Documentation

- Correct the sandbox documentation claims that outran the code — across the
  ADRs, `README.md`, `ONBOARDING.md`, `DEPLOYMENT.md`, the default bundle, and
  the Go comments that state the same guarantees — each verified against current
  `dev` rather than assumed. The load-bearing ones:
  `docs/SANDBOX-RUNTIMES.md` opened by listing **MCP** among the tool calls that
  run inside the sandbox — the exact opposite of the host-side credential
  brokering invariant (ADR-0003/ADR-0040); ADR-0002's Enforcement section
  credited `sandbox_hardened_test.go` with pinning the hardening flags, which it
  never did (and which no CI lane even runs — it is opt-in behind
  `FLEET_SANDBOX_HARDENED_TEST`), while the test that now genuinely pins them is
  `podman_args_test.go`; and ADR-0012/ADR-0031/FEATURE-NOTES claimed
  `allowlisted` is "strictly more restrictive than open", which is false in the
  host-loopback dimension — `allowlisted` requests
  `allow_host_loopback=true` and exempts the `10.0.2.2` gateway from `NO_PROXY`,
  so it can reach loopback-bound services that `open` cannot. That is now
  threat-modelled rather than denied. Also: the KVM-VM boundary is per
  container, not "per tool call"; `FLEET_PYTHON_REPL_MAX` is a soft cap checked
  only at create time; `--network=none` does have a loopback device; and the
  empty-allowlist boot warning named only scheduled tasks.

- Two more claims that outran the code, each verified against the current
  behaviour: `docs/MCP-BROKER-SCOPES.md` still said manifest interpolation
  replaces `${VAR}` with its value in the parsed bundle for connector values —
  connector env/header values now keep their raw text through the load and
  resolve lazily at catalog-build/spawn time, after the `.env` file is applied
  (the paragraph now also states why the name inventory exists at all: the
  names must survive independent of when values resolve) — and ADR-0041
  counted "three callers outside the auth middleware" in the sched epoch test,
  when `headerTrustUser` has three callers *total*: the middleware plus the
  two routes outside it (task create, upload).

### Security

- Two origin checks still derived their trusted host from client-suppliable
  `x-forwarded-*` headers after their siblings moved to the configured
  canonical origin: the OIDC `redirect_uri` a login sends to the IdP
  (`lib/oidc.ts` `buildRedirectUri`) and the host a mutating request's `Origin`
  header is compared against (`lib/csrf.ts` `verifyOrigin`). Behind the shipped
  Caddy config those headers are proxy-set and correct; behind a proxy that
  forwards a client's own values — the deployment shape the bootstrap no-domain
  path leaves to the operator's proxy — the SSO start route could hand the IdP
  an attacker-picked callback host (contained by an exact-match redirect-URI
  registration at the IdP, which is why this is defense-in-depth rather than an
  open hole), and the CSRF comparison could be aimed at an attacker-chosen
  expected host. Both now prefer `NEXT_PUBLIC_PUBLIC_ORIGIN` through the same
  `lib/auth.ts` helper the redirect and Secure-cookie decisions already use
  (now exported), keeping the header derivation only as the fallback when no
  origin is configured, so plain-http local dev is unchanged. **A deployment
  whose `NEXT_PUBLIC_PUBLIC_ORIGIN` differs from the host users actually hit —
  in practice a no-domain box fronted by an operator proxy without the
  `NEXT_PUBLIC_PUBLIC_ORIGIN` line bootstrap already instructs for that shape —
  will emit a different `redirect_uri` (needs re-registration at the IdP unless
  `FLEET_OIDC_REDIRECT_URI` pins it; the pin still outranks everything) and
  will refuse mutating requests until the origin is corrected.**
- A `run_if` pre-run gate was only enforced on one of the paths that could
  dispatch the task it guards. The gate — an admin-authored host-side shell
  condition, which is why authoring one requires admin permission — is
  evaluated solely at the scheduler's scheduled→pending promotion, but a task
  can reach dispatch without ever passing through it: `POST /tasks/{id}/rerun`
  copies the source's gate and mints an immediate *pending* run (so any
  principal with mere `create_task` permission could execute the gated work
  with the condition unchecked), an immediate create with a gate did the same,
  a webhook/email trigger spawn dropped the template's gate outright, and
  `PUT /tasks/{id}` re-derived the edited task's status from `scheduled_for`
  alone (so that same `create_task` principal could echo a gated scheduled
  task unchanged, omit `scheduled_for`, and flip it to `pending` with the
  gate intact and never evaluated). The contract is now enforced
  structurally: `models.NewTask` and the edit recompute in
  `storage.UpdateEditableTask` derive the dispatch state through one shared
  rule (`models.DeriveDispatchState`) that parks every gated cron task on the
  scheduler path (`scheduled`, with a nil `scheduled_for` defaulted to now),
  so whatever minted or last edited it — create, batch, rerun/clone, trigger
  spawn, import, recurrence, edit — the gate is evaluated before every
  dispatch, at the cost of up to one 30-second tick of latency for an
  "immediate" gated run. Trigger spawns now inherit the template's gate, and
  since a *pending* task is already past its evaluation point, the edit path
  refuses to attach or change (but still allows removing) a gate on one
  rather than silently re-parking an imminent dispatch. The full contract
  lives on `models.RunIf`.
- A password reset did not end the account's existing sessions, so the standard
  response to a compromised account did not evict the attacker. The web session
  cookie is a stateless HMAC over `{email, exp}` that the Next.js tier verifies
  by itself, and the only server-side gate — the user-list check in
  `membershipMiddleware` — admits any request whose email still exists. Nothing
  in the token or the `users` table recorded *when* the session was issued, so a
  stolen cookie stayed valid for its full remaining lifetime, up to 14 days,
  no matter how many times the password was changed underneath it. The only
  working levers were deleting the account outright or rotating
  `APP_SESSION_SECRET`, which logs out everybody. Sessions now carry a per-user
  **session epoch** derived from the stored bcrypt hash, forwarded alongside
  `X-User-Email`, and compared against the account's live value by **both**
  backends the one cookie authenticates: chat, inside the user lookup
  `membershipMiddleware` already performs, and the Operations Center, whose
  header-trust path resolves the chat-plane epoch through an injected lookup seam
  (the two planes keep separate databases, ADR-0005). A mismatch is a 401
  `session_revoked` that also drops the stale cookie, so a reset takes effect on
  the next request to either view. Deriving the epoch
  rather than storing a counter means every password write moves it, including
  `fleet chat user passwd`, `fleet admin add` on an existing address and the
  legacy importer, and a reset to the *same* password still rotates it because
  bcrypt re-salts. Magic-link (`elcano_auth`) sessions are unaffected and stay
  revocable at the auth service, which mints that cookie, and an Operations
  Center bearer login is its own credential; the design lives in
  [`docs/SESSION-EPOCH.md`](docs/SESSION-EPOCH.md) with the invariant in
  [ADR-0041](docs/adr/0041-mandatory-session-epoch-claim.md), and the revocation
  levers, these carve-outs and their blast radius in
  [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md), including `APP_SESSION_SECRET`
  rotation as the deliberate break-glass global logout.
  **At deploy time every logged-in user must sign in once more:** cookies minted
  before this change carry no epoch claim, and the web tier refuses them rather
  than grandfathering a token that no backend check could evaluate.
- The `run_if` host-side gate — a scheduled task's shell command, run by the
  scheduler as the fleet user — was authored by the wrong set of people in both
  directions. `taskCreator.isAdmin` was set for **any** typed API key carrying
  `create_task`, so a scoped CI key could attach an arbitrary `sh -c` gate; and
  it was never set on the web/header-trust or cookie paths, so a user whose role
  really is admin was refused. Both create and edit now gate on one definition —
  possession of `models.PermissionAdmin` — which admits admin keys and role-admin
  users and refuses scoped keys.
- A bundle's per-server `tool_allowlist` (Gate-2) was silently ignored in two
  situations, so agents were offered every tool of every selected connector
  regardless of what the operator allowlisted. First, activating the production
  MCP-broker boundary scrubs `cfg.MCPServers` after the broker child boots, and
  the scheduled agent derived its gates from exactly that field — every
  scheduled and sub-agent run therefore ran ungated. Second, the gate looked up
  the allowlist by the *registered* server name, which for a named-account seat
  is `<server>_<account>`, so selecting a seat dropped the filter for that
  server. The allowlist now travels with the broker's public inventory, and
  registered names resolve back to their manifest entry the way the credential
  and persona filters already did.
- Replacing the blanket `CUTLASS_` parent-owned prefix with a hand-written list
  of names left real gaps: `CUTLASS_SERVER_TOKEN` (a supported legacy spelling
  of the shared server token, whose `FLEET_`/`CHAT_` spellings *were* listed)
  and six knobs the parent resolves lazily after broker boot —
  `CUTLASS_OPENROUTER_BASE_URL`, `CUTLASS_MODEL_CACHE_TTL_MINUTES`,
  `CUTLASS_CONTEXT_PRESSURE_WARN_THRESHOLD`,
  `CUTLASS_CONTEXT_COMPACTION_THRESHOLD`, `CUTLASS_SCHEDULED_AUTO_COMPACT` and
  `CUTLASS_MAX_ITERATIONS` — could be claimed by a bundle connector, which then
  unset them in the parent and dropped them from the reload surface. The legacy
  spellings are now derived from the `FLEET_`-spelled names and the configured
  legacy prefixes rather than hand-listed, so the set cannot drift again. A
  bundle that claims one of these names now fails broker boot validation
  instead of silently mutating parent runtime behavior; the cutlass-family
  connector wire contract stays claimable as before.
- `fleet-web` bound `0.0.0.0:3000` while the Caddyfile, `bootstrap.sh` and the
  deployment docs all described it as loopback-only. On a host without
  firewalld nothing stopped a client reaching the web tier directly, bypassing
  Caddy — which matters because the tier trusts `x-forwarded-proto` (a direct
  plain-HTTP login minted a session cookie without `Secure`) and
  `x-forwarded-host` (login/logout/OIDC redirects). The unit now binds
  loopback, and both helpers prefer the configured canonical origin
  (`NEXT_PUBLIC_PUBLIC_ORIGIN`) over client-supplied forwarding headers.

- Stop the boot-time orphan sweep from being starved by PID reuse, and from
  eating this process's own warm containers. Two independent bugs in the same
  sweep:
  - The `fleet.instance` label records `<pid>@<start>` and `prune.go` claimed the
    start time "disambiguates pid reuse" — but nothing read it. Once a crashed
    run's pid was recycled by any unrelated live process, its orphaned container
    was skipped forever, so the leak the sweep exists to reclaim became
    permanent. The labeled start time is now compared against the live process's
    actual `/proc` start time, failing safe in both unknown directions (an
    unparseable label or an unreadable `/proc` means "assume alive" — leaking a
    container is recoverable, force-removing a live sibling's sandbox mid-turn is
    not).
  - The sweep force-removed any `created`/`exited` container **regardless of
    label**, and it runs *after* the agent manager is built — which starts the
    warm pool filling from a goroutine. So this process's own warm containers,
    caught mid-creation, were removable by their own boot. Containers carrying
    this process's label are now skipped in every state, and the call-site
    comment no longer claims the sweep runs "before building the pool".
- Bound the sandbox telemetry poller so it can never block teardown. `podman
  stats` had no `WaitDelay`, so `Output()` could wait on pipes that context
  cancellation does not close (the hazard `BashWaitDelay` already exists for),
  and `close()` waited on the poller with a bare receive — making sandbox
  teardown, the turn's sandbox release, and the pool slot hostage to a telemetry
  goroutine. The poll is now `WaitDelay`-bounded at 2s (a stats sample that slow
  is discarded anyway) and the teardown wait at 5s, past which the rollup is
  abandoned with a log line rather than stalling.

- Deny `AF_VSOCK` sockets in the sandbox seccomp profile. Under the Kata/libkrun
  microVM runtimes (ADR-0010) `AF_VSOCK` is the guest↔host channel, and podman's
  own default profile denies it while ours allowed it. Closed with a single
  non-overlapping rule (allow every family except `AF_VSOCK`, letting the
  default-deny do the work) rather than by copying podman's five socket rules —
  which was tried and measurably does NOT work, because one of podman's broad
  `SCMP_CMP_NE` allows also matches `AF_VSOCK` and absorbs the narrow deny.
  Verified against the real image: `AF_VSOCK` returns EPERM while `AF_UNIX`,
  `AF_INET`, `AF_INET6`, datagram and `AF_NETLINK` sockets all still open, and
  DNS, HTTPS and `pip install` all work. **Scope, precisely:** this stops the
  ordinary calling convention, not a hostile payload — seccomp compares the full
  64-bit register while the kernel truncates `domain` to `int`, so
  `socket(0x100000028, …)` still succeeds. **Podman's own default has the
  identical bypass**, so this is parity with the platform default rather than a
  fleet-specific weakness, and two obvious hardenings were measured and rejected
  (ANDing two comparisons on one argument fails *open*; a masked-equality deny is
  absorbed by the broad allow). Also still open and documented in `seccomp.go`:
  `AF_NETLINK`+`NETLINK_AUDIT`, which podman denies, and `socketcall(2)` on the
  32-bit architectures the profile still lists.
- Deny `vmsplice` in the sandbox seccomp profile. Podman's own default profile
  denies it; ours allowed it, which made fleet's custom profile **weaker than
  shipping no profile at all** in that one dimension — the opposite of what it
  exists to do. `vmsplice` lets a process splice pages of its own address space
  into a pipe, a primitive that has featured in kernel-memory-disclosure and
  container-escape exploits. Verified nothing in the sandbox needs it: bash
  pipes, `cp`, 20 MB pipe/copy workloads, `shutil.copyfile`, `os.sendfile`
  zero-copy, pandas and numpy all work with it denied, and the profile test now
  pins the denial.

- Resolve the sandbox OCI runtime through Podman in the fail-closed preflight
  instead of guessing the binary from its name. `podman --runtime=<r> info`
  resolves the name through `containers.conf` and errors on an unregistered
  one, so the preflight now probes the binary Podman will actually exec: a
  `containers.conf` remap can no longer make it validate a *different* binary
  than the one running every tool call, and a runtime installed on `PATH` but
  never registered now fails at boot rather than at every container creation.
  Resolution applies to any non-empty runtime (only Podman's own default is a
  no-op); `kata`/`krun` keep their additional KVM and `+LIBKRUN` gates. This
  also removes a false FAIL in `fleet validate-config`, which reported a valid
  `containers.conf` mapping to an off-`PATH` binary as a missing runtime.
- Always emit the per-file `--ulimit fsize` sandbox disk quota, adding
  `--storage-opt size` on top where the driver supports it, instead of
  choosing one or the other. The either/or left the workspace **uncapped on
  exactly the hosts with the better storage driver**: `--storage-opt` bounds
  the writable layer, which is essentially unwritable under `--read-only`, and
  does not apply to bind mounts — so `dd if=/dev/zero of=big` in the default
  workdir could fill the host disk, the scenario `FLEET_SANDBOX_DISK_GB`
  exists to prevent. The boot log and the `DiskLimitGB` docs now state plainly
  that total workspace bytes remain unbounded. **Operator-visible:** hosts with
  XFS-pquota / btrfs / zfs now enforce the per-file ceiling they previously had
  none of — raise `FLEET_SANDBOX_DISK_GB` if a workload legitimately writes
  single files above the 5 GiB default (a `bash`+`curl` download of a very
  large file is the likeliest case). Hosts on other drivers are unaffected;
  they already had this cap.
- Apply the fleet-wide `FLEET_DEFAULT_NETWORK_MODE` to the approved-bash take.
  An approval executes out-of-band of the turn loop and honored only the
  per-conversation lockdown seal, so on a `lockdown` deployment an approved
  command from a non-lockdown conversation still ran with open egress, and on
  an `allowlisted` one it bypassed the proxy — despite ADR-0012/ADR-0031
  claiming the setting applies fleet-wide. It now mirrors the interactive turn
  take exactly; the per-conversation seal keeps its stricter no-degrade rule.
- Extend the #796 straggler guard to `run_python`: a cancelled or timed-out
  cell now synchronously kills the container and poisons the sandbox — parity
  with bash and the sandboxed file tools — instead of killing only the
  host-side bridge client while the cell kept executing (and, in persistent
  REPL mode, could be lent to later turns). Bridge write/read errors now reset
  the session so the next `run_python` boots a fresh bridge instead of wedging
  for the rest of the turn, and a failed close-time container kill is logged
  instead of silently swallowed.
- Cap host-side `run_if` stderr capture at 8 KiB while continuing to drain the
  command, preventing a noisy admin-authored gate from exhausting Fleet's heap;
  document the admin-only gate in the host-I/O exception inventory.
- Remove the unused arbitrary trailing Podman-argument seam from sandbox
  configuration, so internal callers cannot override mandatory hardening flags;
  correct stale package docs that claimed a production host fallback.
- Replace credential-owner MCP call, discovery, scope-open, scope-close, and
  reload error details with stable value-free broker errors; failed calls also
  discard partial output before it can cross IPC.
- Pin Next.js transitive `postcss` and `sharp` dependencies above their patched
  floors and refresh both vulnerable `brace-expansion` lines in the web lockfile.

### Added

- **EKS deployment guide (`docs/EKS-DEPLOYMENT.md`):** an operator recipe for
  running fleet on Amazon EKS as **one pod on one large, dedicated node** —
  keeping the single-process/single-writer model of
  ADR-0004 intact rather than sharding sandboxes across worker nodes (no such
  seam exists; ADR-0011 removed the worker registry). Covers the rootless
  Podman-in-a-pod requirements that fail *silently* if missed (subuid ranges,
  `newuidmap` file caps vs. privilege escalation, an overlay driver instead of
  vfs, `/dev/fuse` + `/dev/net/tun`, and the writable cgroup subtree without
  which `--memory`/`--cpus` are ignored and every per-sandbox cap becomes
  fiction), Containerfiles for the two images the repo does not ship, RDS
  two-database setup, an xfs+prjquota StorageClass so the writable layer gets a
  hard **total** cap on top of the per-file `ulimit` that applies regardless
  (both helpers installed in the image, since pasta is Podman ≥ 5.0's default for
  normal turns while the allowlisted-egress posture requires slirp4netns
  specifically), pod sizing that accounts
  for sandbox cgroups nesting under the pod limit, exec health probes (kubelet
  dials the pod IP and cannot reach the loopback-only listeners), the ALB idle
  timeout that otherwise severs SSE turns, and a verification checklist. Also
  answers the objections a Kubernetes-native reviewer raises, with the cluster
  integration each one needs: Pod Security Standards / Kyverno-Gatekeeper
  exceptions for the privileged pod, `fsGroup` so uid 1000 can write the EBS
  volume, IRSA or Pod Identity with **no** RBAC (fleet makes zero Kubernetes API
  calls), a NetworkPolicy that does govern agent egress (sandbox traffic NATs
  through the pod's netns), single-AZ pinning and the honest RTO/RPO of a
  single-writer workload, Kustomize/Argo packaging including the
  `volumeClaimTemplates`-immutability and PVC-prune traps, a loopback-preserving
  metrics scrape sidecar, and the defaults that silently break this pod
  (NodeLocal DNSCache vs. `slirp4netns`, namespace `ResourceQuota`, VPA `Auto`,
  runtime-security signatures firing on normal nested-container operation).
  Closes with an appendix carrying the whole manifest set assembled in apply
  order with a placeholder table, so it transcribes in one paste rather than
  nine. Flags the deployment gap the new `fleet-backup.timer` leaves on
  Kubernetes — the units and their doctor coverage are systemd-only, so a cluster
  install silently has no scheduled dumps unless RDS backups or a `CronJob` are
  chosen deliberately (the #966 failure mode), while noting the in-process Doctor
  panel does still work and reports those checks as `skip` rather than inventing
  advisories. No Helm chart or in-tree manifests: ADR-0004's enforcement clause stands,
  and an unmaintained chart would imply support this path does not have. States
  its own scope honestly: hand-verified, **not** exercised by CI, no chart or
  manifest shipped in-tree, and explicit about the weaker outer trust boundary a
  privileged pod implies.

- **Scheduled database backups are part of the deployment now, not a doc the
  operator had to find.** `docs/BACKUP_RESTORE.md` described a
  `fleet-backup.service` + `fleet-backup.timer` pair and told the operator to
  install a daily timer, but `scripts/bootstrap.sh` never installed it and
  `fleet doctor` never looked for it — so a box could report `38 ok, 0
  advisories` while holding no backups at all, which is what a production
  deployment did for five days with live client data (#966). Three changes
  close the gap. The two units now ship in `deploy/`, version-controlled and
  covered by doctor's unit-drift check the way `fleet.service` is — as
  optional-if-absent, since not every deployment installs them.
  `scripts/bootstrap.sh --enable-service` installs and enables the timer by
  default: it creates the backup directory `0700` root-owned (a dump holds
  every conversation, task and user row), writes `FLEET_BACKUP_DIR` and
  `FLEET_BACKUP_RETENTION_DAYS` into `fleet.env` alongside the DSNs, and
  converges rather than duplicating on a re-run — an installed unit is left
  alone, and both settings resolve process env > the value already in the env
  file > the default, so a re-run keeps a backup directory you relocated
  instead of resetting it; `--no-backup-timer` is the opt-out. And both halves
  of doctor — `scripts/doctor.sh` and the in-process `internal/boxdoctor`
  behind Settings → Admin → Doctor — report the timer's state, in agreement. A
  missing timer is an **advisory** and doctor never installs one: an operator
  who backs up at the volume or hypervisor layer is not misconfigured. So is a
  timer that is enabled but not **active** — `is-enabled` reads only the
  install symlink, so a stopped timer fires nothing while its service still
  reports its last `Result` as `success`. A timer whose **last run failed** is
  a **failure**, because the `oneshot` already exits non-zero when a dump fails
  its integrity check, and a box that looks covered but is not is worse than
  one that is visibly bare. Reinstalling a drifted backup unit does not trigger
  doctor's post-fix restart, so repairing one never costs chat downtime. The
  doc now states plainly what the timer buys: a same-host `pg_dump` survives a
  bad migration or an accidental delete, not the loss of this host or volume,
  and it still does not capture attachment/upload files.
- **Restaurant-style model cost indicators ($ … $$$$):** both model pickers —
  the chat composer's listbox (and its collapsed model chip) and the operations
  center task form's primary/fallback pickers — now show a four-glyph price tier
  per model, derived from the OpenRouter catalog's per-token prices blended
  3 prompt : 1 completion (the ratio that matches fleet's transcript-heavy agent
  loops). Hover/screen-reader text gives the blended `$/M tokens`. Models with no
  published pricing (workspace providers, half-typed custom slugs) show no
  indicator rather than a guessed one, and the tier is a comparison aid only —
  spend is still governed solely by the per-run cost ceilings, with no price
  ceiling on model selection. No new network calls: `/api/model-catalog` gained
  `price_prompt` and `/api/model-rankings` gained both prices, additively, from
  the already-cached catalog. Catalog prices are also joined back onto the two
  pinned "recommended" rows, which dedup had been leaving price-less. See
  [`docs/MODEL-COST-INDICATORS.md`](docs/MODEL-COST-INDICATORS.md).

- **Production child-owned remote MCP scopes:** interactive and scheduled run
  drivers now send only user identity and public server-name filters to
  `fleet mcp-broker`; the child owns token lookup/refresh, SSRF-guarded HTTP MCP
  clients, scoped calls, and cleanup. Explicit OAuth/connectors HTTP endpoints
  remain parent-side control-plane operations (ADR-0040).

- **Child-owned remote MCP scopes (#167 prerequisite):** `fleet mcp-broker`
  now opens an encrypted chat-store connection when remote MCP is configured,
  performs per-user connection lookup, token/API-key decryption and refresh,
  SSRF-guarded handshake, tool discovery, calls, and cleanup inside the child,
  and returns only public tools and skipped names. The dedicated DB pool is
  capped at 8 open/2 idle connections, scope-open resolver and per-server
  token/handshake failure values do not cross the pipe or enter logs, and
  disabled operation fails explicitly. Production consumes these scopes through
  the activation above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Remote MCP scope protocol (#167 prerequisite):** broker scope-open requests
  can now carry a user email plus public enabled/shadowed remote-server names,
  with an explicit filter bit preserving interactive “none selected” versus
  scheduled “all connected.” Scope responses expose only public tools and
  skipped-server names. Mixed bundle/remote fields and ambiguous filters fail
  before backend dispatch; the production child consumes this selector as
  described above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Remote MCP broker overlay seam (#167 prerequisite):** interactive and
  scheduled runs can now receive a per-user remote-MCP overlay as an injected
  broker, public tool catalog, routing-name set, and bounded close function.
  The injected opener takes precedence over the existing resolver, while the
  in-process OAuth client remains a compatibility path for tests and embedders.
  Production uses the child-owned remote scope described above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Production bundle MCP process boundary (#167):** `fleet serve` now starts
  the credential-owning broker subprocess before serving and routes interactive
  turns, scheduled tasks, approvals, and MCP reload through it. The parent keeps
  public catalog/account/gating metadata only; after liveness and discovery
  succeed it unsets connector env keys, overwrites/drops resolved MCP and inline
  HTTP definitions, and prevents config hot-reload from rehydrating exact or
  account-suffixed keys. Startup fails on a connector/parent env-name collision
  or broker preflight error, without scrubbing on failure. Per-user run-time MCP
  clients are child-owned by the activation above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Per-run MCP broker scope protocol (#167 prerequisite):** the internal broker
  can open an isolated session from non-secret server/account selections, task
  ID, and workspace path; return its opaque ID and public tool catalog; and route
  the existing `agentcore.MCPBroker` call seam through that scope. Scope calls
  retain request correlation and cancellation, close is idempotent after success
  and retryable after failure, legacy backends fail explicitly, and backend
  panics remain value-free and incident-correlated. This protocol groundwork is
  consumed by the production bundle and remote-MCP boundaries above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Credential-owning MCP scope backend (#167 prerequisite):** `fleet
  mcp-broker` now constructs each requested account/task/workspace scope inside
  the child, serializes close against admitted calls, and reaps all remaining
  scoped subprocesses when its protocol loop exits. The shared spawn-definition
  builder now excludes disabled servers, so a stale task selection cannot launch
  one. A cancelled open also closes a late-created scope instead of losing its
  opaque handle. Production now consumes this backend as described above.

- **Interactive MCP broker seam (#167 prerequisite):** `TurnConfig` can now
  inject an out-of-process broker plus its public catalog into the unchanged
  `agentcore.Run` path. Per-user remote overlays compose with that injected base
  without shadowing bundle servers. The local-client default remains for
  compatibility callers and tests; production injects the subprocess broker.

- **Approval MCP broker seam (#167 prerequisite):** interactive email
  pre-validation and post-approval MCP execution now depend on the common
  broker/catalog seam instead of a concrete credentialed client. Prefixed tool
  routing remains server-qualified (including underscore-bearing names), and a
  broker tool-level error now resolves the approval as failed. Production uses
  the child broker's unscoped default-seat path for long-lived approvals.

- **Connector env inventory (#167 prerequisite):** client bundles now retain a
  names-only inventory of MCP and inline-HTTP-tool environment references from
  the raw manifest, before interpolation can erase already-exported variable
  names. The inventory also expands account-suffixed stdio env keys against a
  supplied environment, enabling the production parent to scrub exactly the
  connector keys after a broker child inherits them. The production boundary
  above now consumes this inventory.

- **Interactive Manager MCP scope seam (#167 prerequisite):** Manager can now
  consume an injected public broker/catalog/account inventory without creating
  a credentialed local MCP client, and can open one isolated broker scope per
  chat turn. Scope selection always includes mandatory bundle servers, includes
  only opted-in optional bundle servers, threads public account names and the
  bound conversation workspace, fails closed on open errors, and closes with a
  cancellation-independent timeout. Production now injects this seam.

- **Scheduled MCP broker seam (#167 prerequisite):** the scheduled `Agent` and
  its governed sub-agents can now call and discover bundle tools through an
  injected broker/catalog, including composition with the existing per-user
  remote overlay. Broker mode suppresses the in-process MCP loader tools instead
  of advertising an unavailable mutation path. Production uses this broker path.

- **Scheduled Runner MCP scopes (#167 prerequisite):** `scheduledrun.Runner`
  can now open and close an isolated broker scope per task, preserving explicit
  server/account choices and mapping an empty selection to all enabled bundle
  servers. Task ID and per-run workspace metadata cross the value-free seam;
  scope failures do not fall back locally, cancellation cannot suppress close,
  and remote-MCP shadowing uses the returned public catalog. Production injects
  this opener for every scheduled run.

- **Credential-owner MCP reload (#167 prerequisite):** the broker protocol can
  now ask the child to re-read its own bundle against its boot credential
  snapshot, apply the minimum server diff, and return only the public change summary and tool
  catalog, provisioned account-seat names, and enabled servers' public gating
  metadata. Resolved connector definitions never cross the pipe; reload retains
  request cancellation and value-free panic containment, existing scopes keep
  their snapshot, and future scope opens serialize onto a coherent old or new
  catalog.

- **Interactive broker reload seam (#167 prerequisite):** Manager can now apply
  a credential-owner reload result to its public catalog, account names,
  allowlists, optional gates, prompt roster, and picker metadata under the same
  serialized reload path as its local client. Broker-mode reload takes its
  server metadata from the child result instead of re-reading connector config
  in the parent, and publishes that entire public view atomically. An injected
  broker without a reload adapter fails explicitly rather than returning a false
  empty success. Production injects the credential-owner adapter.

- **Scheduled public MCP inventory (#167 prerequisite):** broker-scoped task
  runs can expand an empty selection and decide whether to mint a per-run
  workspace from a live names/`uses_workspace` snapshot, without retaining the
  credential-bearing `cfg.MCPServers` map. Updating the provider changes the
  next run while the local compatibility binder keeps its existing config path.

- **Query-parameter API keys for hosted connectors (Browserbase)**: some
  vendors authenticate their hosted MCP server with the key in a URL query
  parameter rather than a header. api_key connectors now support
  `api_key_query`: the key stays sealed at rest and is attached per-request
  by the HTTP transport, so the stored URL, logs, and error strings never
  carry it. The built-in Browserbase directory entry — previously marked
  OAuth, which its endpoint doesn't serve, so every connect failed at
  discovery — now uses `api_key_query: browserbaseApiKey` with a setup hint.
- **Self-wake** (`docs/SELF-WAKE.md`): a scheduled run can suspend itself
  and schedule its own resumption — new `sleep` (park until a deadline) and
  `wake_on_event` (park until `POST /tasks/{id}/wake` fires the matching
  key, or a timeout) tools with the exact ask-pause lifecycle: the run ends,
  sandbox/lease released, task parks in new status `paused_awaiting_wake`,
  and the scheduler's tick re-queues it as a fresh run carrying the agent's
  required note-to-self plus the wake reason. Every wake has a deadline
  (event waits default to 7 days), parks are capped at 100 per task, and
  cost accumulates on the task across cycles.

- **Discuss this run** (`docs/DISCUSS-RUN.md`): a finished scheduled run's
  log modal gains "Discuss in chat" — a one-way BFF bridge (inverse of
  promote-to-task) that reads the transcript through the caller's
  orchestrator credential, creates a chat conversation seeded with a clamped
  digest (new optional `seed` on `POST /conversations` — one user message,
  no turn), and deep-links to it via the new `/chat?c=<id>` boot param. The
  chat server never reads the sched store; ADR-0005's database split stands.

- **Per-attempt run log history** (`docs/RUN-LOG-HISTORY.md`): re-running the
  same task id — a retry, an ask-pause resume, a self-wake cycle — no longer
  destroys the prior attempt's transcript. The row the `logs` upsert would
  clobber is copied into `run_logs` in the same transaction (archived payloads
  travel verbatim), capped at 20 per task, pruned with the task by both
  retention paths. New `GET /logs/{task_id}/history[/{entry_id}]` behind the
  exact `GET /logs/{task_id}` gate, and an attempt picker in the task log
  modal that renders only when history exists.

- **A bundle can now brand the shell, not just tint it** (`docs/BRANDING.md`):
  new `branding.logo` — a bundle-relative image path served from the bundle by
  `/brand/logo` (proxied as `/api/brand/logo`), so the navigation rail's mark is
  a bundle change plus a restart, never a web rebuild or a file copied into
  `web/public`. Previously `NavRail` hardcoded `elcano-mark-primary.svg`, so
  every deployment wore one client's mark beside its own `app_name`; that asset
  is deleted from fleet and the fallback is fleet's own mark. The path is
  resolved at load — lexically local, still inside the bundle after symlink
  resolution, a regular file, a known image extension — so a bad value fails at
  startup instead of rendering a broken image on every page; the route caps it at
  2 MiB and pins delivery with `nosniff` + `default-src 'none'; sandbox` so an
  SVG carrying `<script>` executes nothing. `/client-config` advertises
  `logo_url` only when a file actually backed the field.
  **Seven more themable color tokens** — `text_disabled`, `border_strong`,
  `border_subtle`, `overlay_soft`, `overlay_strong`, `rail_hover`, `rail_active`
  — because `globals.css` hand-tints those from fleet's own primary hue, so a
  bundle overriding `primary` alone kept fleet-purple emphasis borders and rail
  rows beside its palette. The last two close the follow-up `globals.css` records
  inline. Semantic status colors (success/danger/warning) stay fleet's: they
  encode meaning, not brand.

### Fixed

- **Live Playwright no longer trips the password-brute-force limiter it is
  meant to preserve.** The real-stack suite submitted the same test account's
  password before nearly every spec, deterministically exhausting
  `/auth/verify`'s five-attempts-per-email window and turning unrelated tests
  into `/login?e=server` failures. It now creates one real password-authenticated
  worker session and copies that cookie into each isolated test context; the
  dedicated auth specs still exercise valid and invalid password submissions,
  and the production limiter remains unchanged.

- **Root Go gates no longer traverse npm dependencies.** An explicit module
  boundary at `web/` keeps `go build/test/vet ./...` scoped to Fleet-owned Go
  packages even after `npm ci` installs packages containing incidental Go code.

- **`fleet chat` now honors the server's terminal turn outcome.** The TUI and
  one-shot mode surface `turn.error`, model-selection failures, cancellation,
  premature stream EOF, and non-success health checks instead of treating them
  as successful empty or partial answers.

- **Chat migrations no longer deadlock with a one-connection pool.** The
  advisory lock and every migration query/transaction now share one dedicated
  connection, so `CHAT_DB_MAX_CONNS=1` remains a valid operator setting.

- **A recovered MCP-broker backend panic no longer strands its caller until the
  request timeout.** Call, tool-discovery, and account-discovery goroutines now
  complete the matching IPC request with a generic, incident-correlated error
  and keep the broker serving subsequent requests. The recovered panic value is
  classified only and never crosses the broker pipe, so connector material in a
  panic cannot be reflected into the agent-loop process. Runtime/README claims
  distinguish the now-active bundle process boundary from the still-in-process
  per-user remote MCP OAuth path tracked by #167.

- **Sandbox acquisition now fails closed at the pool shutdown boundary.** Every
  take path (warm, cold, persistent, lockdown, and allowlisted) returns
  `ErrClosed` after `Pool.Close` marks the pool closed; a concurrent cold start is
  immediately reaped instead of escaping the shutdown boundary. The pool is
  marked closed before its allowlist proxy is stopped, preventing a racing turn
  from receiving a container configured with a dead proxy. A nil pool now
  returns `ErrContainerUnavailable` instead of panicking, and the existing
  keeper/close stress test now exercises the advertised race rather than
  stopping all takers before close.

- **Chat input queues no longer grow forever or drain tied positions
  nondeterministically** (#835). Completed/cancelled rows are now purged after
  `FLEET_INPUT_QUEUE_RETENTION_DAYS` (30 days by default; `0` disables), at boot
  and after turns; pending/running/injected work is never retention-eligible.
  Enqueue and send-now position allocation now serialize per conversation, a
  partial unique index makes non-terminal positions structurally unique,
  migration 046 deterministically normalizes legacy ties (including rows that
  recovery may re-queue), and drain/list queries carry `created_at, id`
  tie-breakers. Fresh enqueue acknowledgements also return the database-assigned
  position instead of an incorrect zero value.

- **Every deployment's shared links unfurled with Elcano's logo** (#893). The
  `og:image` / `twitter:image` was a checked-in `web/public/share.png` containing
  Elcano's logo and wordmark, fleet's purple gradient, and fleet's marketing
  headline — served from the *client's own domain*, so nothing looked amiss to the
  unfurler. Pasting a link to a white-labeled instance into Slack, iMessage,
  Discord, Teams or LinkedIn showed another company's brand. The alt text was that
  same headline, hardcoded, so it leaked even to scrapers that only read
  `og:image:alt`, and to screen readers.

  New `branding.share_image` (bundle-relative, validated at load through the same
  `resolveBrandImage` containment checks as `logo` — lexically local, re-checked
  after `EvalSymlinks`, regular file, known extension), served by
  `/brand/share-image` and proxied as `/api/brand/share-image`. That proxy is in
  the middleware's public-path set out of necessity rather than convenience:
  unfurl scrapers are anonymous, so an `og:image` behind the session gate renders
  no preview at all. `/brand/meta` advertises `share_image_url` only when a file
  actually backed the field, mirroring `logo_url`, so the web never points a
  scraper at a 404. The cap is 5 MiB rather than the logo's 2 MiB, because the
  asset genuinely is larger — but still capped, since scrapers abandon slow
  fetches.

  `og:image:width` / `height` are now declared **only** for fleet's own card,
  whose size fleet knows. fleet does not decode a bundle's image, and a wrongly
  declared size renders a distorted preview; with the tags absent, scrapers fetch
  and measure. The committed default `share.png` is replaced with a **fleet-only**
  card — no Elcano logo or wordmark — so a bundle-less deployment doesn't ship
  another company's branding either, which also removes client-specific content
  from the engine repo per the `AGENTS.md` boundary doctrine.

- **A white-labeled deployment still introduced itself as "Fleet"** (#891, #892,
  #894, #895) — in its browser tab, its tab icon, its login headline, its PWA
  name and splash, its browser-chrome tint, and every shared-link unfurl. Four
  defects with one root cause: the surfaces that most define a deployment's
  identity all render **without a session**, and `/client-config`, where branding
  is served, is member-gated. So each had quietly settled for a fleet default:
  - `branding.login_title` / `login_tagline` were parsed, defaulted, API-served,
    and typed in the web client — then hardcoded in `login-card.tsx` (#892).
  - `branding.share_title` / `share_description` were read by **zero**
    components; `layout.tsx` built its own OG strings from a build-time env var
    nothing in the deploy path set from the bundle (#894).
  - `<meta name="theme-color">` was a pair of fleet-purple literals, and it
    overrides the PWA manifest's `theme_color` in Chrome — so it silently defeated
    `manifest.ts`'s bundle lookup (#895).
  - `app/favicon.ico` was still emitted alongside the bundle-driven
    `metadata.icons` and, being the only candidate carrying `sizes` and `type`,
    won the tab strip. `metadata.icons` overrides `icon.svg` and `apple-icon.png`
    but cannot suppress `favicon.ico`, which Next special-cases (#891).

  All four now resolve from one place: a new token-gated, identity-less
  `/brand/meta` (same trust class as `/theme.css` and `/brand/logo`, which exist
  for exactly this problem), read server-side through
  `web/src/app/lib/serverBranding.ts` — memoized 60s in-process, 2s fetch
  timeout, and it never throws, so branding can never fail a page render.
  `NEXT_PUBLIC_APP_NAME` is demoted to a backend-unreachable fallback.
  `favicon.ico` is deleted (unbranded deployments still get fleet's mark via
  `/api/brand/logo`'s 307), and the PWA's `any`-purpose icon now points at the
  bundle mark while the maskable one stays fleet's correctly-padded asset.

  Two **build-time traps** were found and closed in the process. `manifest.ts`
  was written to read the palette per request and never did once — its route was
  statically prerendered, so the fetch ran during `next build` in a staging dir
  with the backend down, and every deployment silently served the fallback
  (`theme_color: "#1a0b1e"` on a host whose `/api/theme` correctly served
  `#0A0908`). And a root-layout `generateMetadata` is not sufficient alone:
  `/settings/*`, `/no-access` and `/` were prerendered with `<title>Fleet</title>`
  **baked into the HTML artifact**, so only *some* tabs would have been wrong.
  Both are fixed with `force-dynamic` and asserted in tests, since the failure
  mode is silent.

- **Exports downloaded as a file named `export` with no extension** (#896). Both
  proxy funnels — `proxyToOrchestrator` (all 37 `/api/orchestrator/*` routes) and
  `chatServerPassthrough` — re-emitted only `Content-Type` and dropped every other
  upstream header, so the `Content-Disposition` filename four Go handlers set on
  purpose (dataset CSV, prompts, adoption CSV, project export) never reached the
  browser; a bare `download` attribute then names the file after the URL's last
  path segment. Header forwarding now lives once in `web/src/app/lib/proxyHeaders.ts`,
  generalizing the fix `api/conversations/[id]/export` already had by hand.
  `Content-Length` is deliberately not forwarded — `fetch()` decodes a compressed
  upstream body, so a copied length can truncate the response. Both funnels also
  stream `upstream.body` instead of buffering it, which kept whole CSV exports in
  memory and defeated the streaming writers upstream. The project export's filename
  moved into its Go handler (the only one that had none) so it saves as
  `Q3-Planning-a1b2c3d4.json` rather than `project-<uuid>.json`, and
  `exportFilename` now takes its fallback noun as a parameter instead of
  hardcoding `"chat"`.

- **White text was hardcoded on 16 token-driven fills, so a light-primary bundle
  rendered invisible labels** (#890). `on_primary` landed in #889 but only the two
  `--gradient-action-primary` consumers were converted; every flat fill still said
  `text-white`. On the Reklaim deployment (`primary: #FFDF03`) that measured
  **1.33:1** against WCAG AA's 4.5:1 — on the login page's Sign in button, all four
  tool-approval **Approve** buttons, every primary button in Settings, the user
  avatar, and the sidebar's selection tick. Primary fills now use
  `var(--color-on-primary)` (14.87:1 on that palette). Two further fills were
  failing for **fleet's own** stock palette, not just white-labeled ones — white on
  the dark theme's `--color-accent` was 2.28:1 and on `--color-danger` 2.77:1 — and
  now use `var(--color-surface-1)` (5.9–7.2:1), which was already the codebase's
  idiom at three other sites. The bulk-delete button's countdown state takes the
  muted disabled treatment instead of a high-contrast label on a half-alpha fill.
  Root cause was the rule's wording: `DESIGN.md` banned raw *hex* colors, and every
  offender spelled its color as a Tailwind utility, so nothing caught it. The rule
  now bans color utilities too, and `web/src/app/designTokens.test.ts` pins it —
  including a positive check that a primary fill declares `--color-on-primary`,
  which found a 14th site that grepping for `text-white` had missed.

- **A themed deployment still rendered fleet's colors on its most visible
  surfaces.** `branding.colors` themed the flat tokens, but the gradients did not
  read them: `--gradient-bg` (painted on `<body>`), `--sidebar-surface` (the rail),
  the surface/panel/card/composer gradients, and `--gradient-action-primary` were
  hardcoded fleet-purple, as were light-mode agent-link colors and the light
  usage-bar hue. A bundle could set all 18 tokens and the app still read as
  fleet-purple with a client-colored trim. All of them now derive from the palette
  via `color-mix()`; the percentages were fitted against the previous literals for
  fleet's own palette, so the stock appearance is unchanged (every stop within
  ΔRGB ≤ 6). See `docs/BRANDING.md`.
- **A bundle with a light primary had unreadable buttons.** Every rule painting
  `--color-primary` or `--gradient-action-primary` hardcoded white text, so a
  yellow-primary bundle rendered white-on-yellow at 1.33:1 — including the send
  button and the empty-state CTA. New themable `on_primary` token carries the
  foreground for those surfaces (14.87:1 with near-black on yellow), and light
  mode's action gradient now deepens via `--color-primary-hover` rather than
  mixing the brand color toward black.
- **The tab icon and the installed-app splash color ignored the bundle.**
  `icon.svg`/`apple-icon.png` are build-time file-convention assets a bundle cannot
  reach, so every deployment wore fleet's mark in the tab beside its own name; the
  PWA manifest hardcoded a splash color. The favicon now resolves to
  `branding.logo` via `/api/brand/logo` (which redirects to fleet's own mark when a
  bundle declares none, so the link is never dead), and the manifest reads the
  bundle's dark `--color-bg`.
- **An SVG bundle logo rendered as a broken image on every page.** `next/image`
  skips Next's optimizer only when the `src` path literally ends in `.svg`; a
  bundle mark arrives as `/api/brand/logo`, which has no extension, so it was
  rewritten to `/_next/image?url=…` — and that endpoint rejects `image/svg+xml`
  unless `images.dangerouslyAllowSVG` is set, which `next.config.ts` deliberately
  does not set. Every bundle whose `branding.logo` was an SVG therefore wore a
  broken mark in the rail, including the format `docs/BRANDING.md` uses in its own
  example. The rail's `<Image>` is now `unoptimized` (a 28px mark gains nothing
  from the optimizer anyway), and `NavRail.test.tsx` pins it by asserting the
  rendered `src` is the raw path with no generated `srcset`.
- **Bundle branding never actually reached the login page.** The root layout
  links `/api/theme` as a render-blocking stylesheet on every page, and both
  `theme.go` and the proxy route documented it as public — but the path was
  missing from the middleware's public-path set, so a pre-session request 401'd
  and the login page silently fell back to fleet's built-in palette. That is the
  one page every user sees before anything else, and the surface a white-labeled
  deployment cares most about. `/api/theme` and `/api/brand/logo` are now both
  public (deployment-wide, non-secret, and each degrades quietly to empty
  CSS / a 404), pinned by a regression test.

- **Catalog refresh: `gamma`** (`design-media`, provenance `official`, `auth:
  oauth`) — Gamma's hosted MCP at `https://mcp.gamma.app/mcp`: generate decks /
  docs / webpages from a prompt, template, or multi-page input, browse, read and
  export existing gammas (PPTX/PDF), read comments, and pull per-deck engagement
  analytics. Verified before shipping: `initialize` + `tools/list` answer 15
  tools, and the full OAuth chain resolves — RFC 9728 protected-resource
  metadata names `https://auth.gamma.app`, whose RFC 8414 metadata advertises an
  RFC 7591 `registration_endpoint` and PKCE `S256`, so dynamic client
  registration needs no operator-supplied client (no `client_registration:
  manual`). Gamma serves that metadata at the **origin root** only — the
  path-aware location (`/.well-known/oauth-protected-resource/mcp`) 404s — so
  this entry depends on the origin-root fallback added in #878. Scopes are
  `generate` and `gamma:read`; the MCP server is available on all Gamma plans
  and generations charge credits.

- **Bigger uploads with honest failures, plus admin storage visibility and
  cleanup** (`docs/UPLOADS-AND-STORAGE.md`): the per-file upload cap is now
  configurable (`FLEET_UPLOAD_MAX_BYTES`) and defaults to 1 GiB (was a
  hardcoded 256 MiB on chat attachments / 250 MB on task uploads); the chat
  composer learns the limit from `/server-config` and refuses oversize files
  at pick time with a visible explanation instead of a silent post-upload
  413, warns when a queued upload is large, and explains the disabled Send
  button; oversize errors from the server are human-readable 413s in every
  case (including the previous opaque `400 unexpected EOF`). Settings →
  Admin → Server gains a Storage panel — bytes for attachment uploads, task
  uploads, and workspaces, the largest conversation workspaces with owner/
  pinned context, and a "clean up now" action that deletes old unpinned
  chats (pinned/archived/shared/project chats are never touched) and sweeps
  aged files. The orchestrator's `temp_uploads` cleanup (previously dead
  code) and the attachment TTL sweep now also run on an hourly timer, so an
  idle server reclaims disk without waiting for a chat turn.

- **Recurrence end conditions + horizon-based Upcoming**
  (`docs/RECURRENCE-END.md`): recurring tasks can end on a date
  (`recurrence_until`) or after a total number of runs
  (`recurrence_remaining`), exposed in the task modal as "End repeat"; and
  `GET /tasks/upcoming?until=` projects every occurrence inside the window
  instead of capping recurring tasks at their next 5, so the week board no
  longer implies a schedule "ends" five occurrences out.
- **`fleet doctor` — box-level diagnose AND repair** (patterned on chat's
  `chat doctor`; `docs/DOCTOR.md`): a root pass over every box prerequisite —
  toolchain floors, fleet-critical package currency with broken-dnf-repo
  quarantine, the service user's rootless-podman prerequisites (subuid/subgid,
  dir ownership, containers.conf, stale pause namespaces), systemd unit drift
  vs `deploy/`, env-file shape/permissions, service health + `/healthz` +
  `/readyz`, and a sandbox smoke run **as the `fleet` user** — fixing what it
  can in place. `--check` diagnoses only; `--no-restart`; `--dry-run` prints
  the checklist. `doctor` is no longer a bare alias of `status` (the read-only
  contract lives on as `doctor --check`). Admins get a read-only version in
  **Settings → Admin → Doctor** (`GET /admin/doctor`, `internal/boxdoctor`):
  DB pings, disk headroom, podman prerequisites, sandbox image, unit states —
  each failing check annotated with the on-box repair command, plus an
  explicit "Run deep checks" sandbox smoke.

### Changed

- **Icon-button hover polish across chat + orchestrator**: the sidebar's
  green "New chat" hover treatment (colored pill background + tooltip +
  subtle motion-safe scale) is now applied consistently. Add/create actions
  hover green (sealed-chat lock — now the same size and brighter, collapsed-
  rail new chat, orchestrator New task, queue send-now); destructive actions
  hover red on the `--color-status-error-*` tokens (archived trash, bulk
  delete, queue remove, dataset column remove); section chevron rows and the
  log-modal close buttons gained missing hover feedback; and previously
  unlabeled icon buttons (run feedback thumbs, column remove, week nav,
  jump-to-latest) gained design tooltips.
- **Sidebar polish follow-ups (owner review)**: the sealed-chat lock hovers
  neutral instead of green (green stays reserved for the plain "+" adds), the
  conversation and project kebab buttons show a real design tooltip on hover
  instead of the browser-native title, and "Open project" uses the standard
  open/external glyph instead of a bare arrow. Tooltips now shrink-wrap to
  their text instead of always being 14rem wide.
- **Sealed-chat row badge is muted and right-aligned**: the lock marking a
  sealed conversation moved from a leading accent glyph to a quiet muted
  lock at the row's right edge (the retention-note treatment). The
  new-sealed-chat lock button stays in the Chats header next to "+".
- **Pinned and Temporary read as groups, not chats**: both headers are now
  small-caps eyebrow captions with a leading accent icon (pin / clock) — 
  visually distinct from the chat rows beneath them, but one level below
  the Projects/Chats/Labels section headers, matching their hierarchy.
- **One close button everywhere**: every dialog/drawer dismiss "×" (keyboard
  shortcuts — previously a tiny text glyph —, prompt library, save-prompt,
  projects, memories, mobile nav drawer, task create/log/dataset modals) now
  uses one shared CloseButton: same 2rem box, rounding, glyph, and a slight
  red-tint hover. The orphaned `.modal-close` CSS was removed.

### Added

- **"Sign out" on OAuth connectors**: Settings → Connections now separates
  ending an authorization from deleting the connection. Sign out revokes
  (best effort) and drops the stored tokens but keeps the registration —
  including a manually-registered OAuth client's ID/secret — so Connect
  signs back in without re-entering anything. Remove stays the full delete.

### Fixed

- **Remote-MCP OAuth discovery finds path-aware metadata (Google Workspace
  MCP)**: connectors whose resource URL has a path (e.g.
  `gmailmcp.googleapis.com/mcp/v1`) publish RFC 9728 §3.1 metadata at
  `/.well-known/oauth-protected-resource/<path>` and 404 the origin-root
  form fleet probed, so every Google Workspace connect failed at discovery.
  Discovery now tries the path-inserted location first, then the root.
- **OAuth connect no longer strands the browser on localhost**: after a
  successful remote-MCP authorization behind a reverse proxy, the callback
  route redirected to the internal origin (`localhost:3000`) because it built
  the URL from `request.url`; it now uses the forwarded host like every auth
  route (`getRedirectUrl`).
- **Manual OAuth client forms now show the callback URL**: connector setup
  hints (GitHub, Google Workspace, …) told users to register their OAuth app
  with "the callback URL shown in your Fleet instance's OAuth settings", but
  nothing in the UI displayed it. `/mcp-catalog` now returns
  `oauth_redirect_uri` and the guided add form renders it with a copy button.
- **A selection-less scheduled run no longer replays another task's create
  markers**: `bindTaskMCP` returned the **shared per-deployment** MCP workspace
  dir as the #717 reconciliation workdir for any task without an
  `mcp_selection`. That directory's `creates.jsonl` accumulates the markers of
  every task *and* every chat conversation on the box, keyed only by
  `(ssp, deal_name)` with no run attribution — so an unrelated task's
  half-finished create was injected into this task's prompt as "the prior
  process stopped after submitting these creates", and since an abandoned
  `submitted` marker is only ever cleared by a matching resolution in the same
  file, it was replayed into every future run forever. The selection-less path
  now returns no reconciliation workdir (an unattributable ledger is not a
  resume signal); a task that wants real per-run resume semantics declares an
  `mcp_selection` and gets the dedicated client, whose workdir and
  `${FLEET_TASK_ID}` identify exactly one run. When the catalog references
  `${FLEET_TASK_ID}` the run now logs that its per-task ledgers are inert, so a
  missing selection is visible instead of silent. New
  `agentcore.EnvReferencesTaskID`. Same root confusion — a shared directory
  treated as a run identity — as the SendGrid send-once bug fixed in
  elcano-config PR #48, where it made scheduled tasks report emails as sent
  that were never delivered; `docs/MCP-BUNDLE-ENV.md` now states the rule for
  both sides.
- **A sent email now reads as an outcome, not a JSON dump**: `ToolResultView`
  had purpose-built renderers only for `bash` and `task_tracker`, so a
  `send_email` result (built-in or any bundle's `mcp_*_send_email`) fell through
  to the raw `<pre>`. Because the approval gate resolves the call *after* the
  model's turn has ended, the last thing a user saw after clicking **Send** was
  the provider payload — status code, message id, and a paragraph of HTML-lint
  prose about Outlook table borders. It now renders one outcome line (`Queued
  for delivery` / `Already sent` / `Not sent`) with the message id, any
  formatting notes, and the full payload behind a collapsed "Delivery details"
  disclosure. A rejected send is read off the payload's `error`/non-2xx
  `status_code` rather than the tool-level error flag — the server returns a
  failure as a normal result, so trusting `is_err` alone printed "queued" over
  a send that never happened. The pre-approval `APPROVAL_REQUIRED` placeholder
  now says it is waiting for approval instead of showing the raw sentinel.
- **Env-file writes now survive a read round-trip (#834)**: `SetEnvKey` wrote
  values verbatim while both parsers (creds and the server's `loadEnvFile`)
  strip whitespace, surrounding quotes, and ` #`-style inline comments — so a
  secret containing any of those authenticated when saved and came back
  mangled after the next restart. The writer now wraps such values in one
  layer of quotes (which both parsers strip without touching the interior;
  plain API keys stay byte-verbatim on disk), and refuses values containing
  line breaks instead of physically corrupting the file.
- **`turn.retry` events are now actually emitted (#833)**: journal recovery
  and the web client both consumed the event (recovery drops the abandoned
  pre-retry partial text; the client shows an inline "retrying" badge) but no
  code ever produced it — so a crash after a mid-turn provider retry could
  project a garbled mix of pre- and post-retry text into the recovered
  history, and users never saw retries happen. The run loop now emits it
  (RetryEventPayload shape) from fantasy's inner-retry backoff and from every
  resilience re-drive (stream blip, fallback swap, compaction rollback).
- **Chat submit guard reads now fail closed (#832)**: a store error during
  the `input_id` idempotent-replay lookup fell through to start a fresh
  billed turn for an input that may already have been accepted, and on the
  queue path a `CountPendingInputs` error silently waived the
  unattended-spend depth cap (with a lookup error at the cap masquerading as
  a 429 "queue full"). All three reads now surface the error as a 500 so the
  client can retry the same `input_id` safely.
- **Shared MCP spawns no longer leak the literal `${FLEET_TASK_ID}`
  placeholder to connectors (#831)**: only the scheduled per-run path called
  `ExpandTaskIDEnv`, so boot-time shared spawns, hot-reload diffs,
  `BindMCPSelection`, and `fleet mcp test` probes handed a bundle env value
  mapping to the reserved token through as the raw string
  `"${FLEET_TASK_ID}"`. All four paths now drop token-bearing keys (the
  intended no-task-identity fail-safe); the scheduled path, which
  pre-substitutes the real task ID, is unaffected.
- **MCP HTTP clients no longer forward resolved credential headers across
  cross-origin redirects (#830)**: inline HTTP tools and the HTTP MCP
  transport (including the TLS-hardened variant) used Go's default redirect
  policy, which only strips `Authorization`/`Cookie` — a 30x naming a
  different host received every custom credential header (`X-Api-Key`,
  vendor auth headers) verbatim. A shared `CheckRedirect` policy now follows
  redirects but drops all originally-set headers when a hop leaves the
  original host:port origin, mirroring the guarantee
  `mcpoauth.SafeHTTPClient` already gave user-supplied remote servers.
- **Chat web: a stale busy flag no longer turns a `mode:"queue"` submission
  into an invisible billed turn (#824)**: the composer picks the queue path
  from client-side streaming state, but the server only honors queueing while
  a turn is actually running — if the turn finished in the race window, the
  POST started a direct SSE turn that the client mistook for a queue ack:
  no user bubble, no stream consumer, nothing in the queue until a page
  refresh. The queue branch now classifies the response by `Content-Type`;
  a `text/event-stream` answer cancels the unread body (the turn outlives
  its originating request by design) and hands off to `reattachToConv`,
  which attaches to the running turn's buffer and replays from event 0 —
  the `user.message` echo renders the submission's bubble and tokens stream
  normally.

- **Crash recovery preserves steered user messages (#826)**: an interrupted
  turn's recovery rebuilt assistant text, reasoning, and tool calls from the
  event ledger but never projected steered `user.message` events — the
  mid-turn instruction vanished from canonical history while recovery's
  `history_committed_at` stamp made boot queue-recovery mark the steer's row
  completed ("durably in history"). `buildRecoveredEntries` now projects
  steered user messages in stream order, exactly once per `input_id` (a
  resilience re-drive can re-emit the event; the turn-start non-steered
  `user.message` stays excluded — it is committed separately at turn_seq=1).

- **Injected steers are at-most-once, never re-executed after committed side
  effects (#823)**: an injected steer whose turn ended without a history
  commit was blindly re-queued and re-run as the next turn — but the model
  may already have ACTED on it (its committed tool side effects survive the
  failed turn per #820), so "send that email" could execute twice.
  `MarkInputInjected` now stamps the turn journal's max seq as an injection
  watermark (`injected_seq`, migration 044); turn-end settlement and boot
  recovery re-queue the row only when no tool intent was journaled after the
  watermark (the model provably never dispatched with the steer in context)
  and otherwise cancel it with a logged reason — dropping a resendable
  message instead of duplicating a side effect. Rows injected before the
  migration degrade to the coarse gate (any tool intent blocks the requeue).

- **Input-queue hardening (#785 follow-up)**: a per-conversation pending
  depth cap (20, HTTP 429 above it; idempotent replays still answer 200) so a
  retrying client can't bank unbounded unattended LLM turns; a transient
  launch failure of a drained row now schedules a re-kick instead of leaving
  the 202-acknowledged input stalled until the next submission; the Stop
  scope=all epoch gate is strictly-before, so a fresh submission accepted in
  the Stop's own second is no longer silently cancelled; an ambiguous-commit
  replay (`ErrTurnHistoryCommitted`) now settles the turn's injected steer
  rows instead of leaving them listed until the next reboot; queue
  `message_preview` truncates on a rune boundary; stale stop epochs are
  pruned; and turn-scoped queue lookups gained an index (migration 043).

- **Committed tool side effects survive a mid-round provider failure in
  canonical history**: when ADR-0035 suppresses recovery after a tool ran,
  the run's partial transcript was discarded with the error — the executed
  call existed only in the turn journal, was never projected into `messages`
  (the turn seals non-`running`, so recovery skips it), and the retried turn
  could re-execute the side effect blind. `streamErrorResult` now returns the
  partial transcript alongside `ErrCommittedSideEffects`, and the interactive
  manager commits it terminally before surfacing the failure.
- **Data race on a server's tool list during a mid-call stdio restart**:
  `initialize` reassigns `Server.tools` under `Server.mu` while catalog
  readers (`GetAllTools` and friends) hold only `Client.mu` — a torn read
  under concurrent conversations. The slice now has a dedicated RWMutex
  (readers deliberately do not take `Server.mu`, which is held for the whole
  restart including the network round-trip).

- **A lifecycle hook's reason-only trailer no longer downgrades its block**:
  `parseHookDecision` let any later contract-key line replace the verdict, so
  `{"decision":"block",…}` followed by `{"status":"ok","reason":"scan
  complete"}` executed the tool despite `enforce: true` — the fail-open class
  the parser exists to prevent, one key narrower. A line without an explicit
  `decision` key can no longer replace a seen verdict.
- **Turns that fail with `turn.model_required` are sealed as errors, not
  completed**: the engine deliberately emits it instead of `turn.error` (the
  user can fix it by switching models), but `inferTerminalStatus` didn't know
  it — the failed turn was sealed `completed` with `history_committed_at`
  NULL, breaking the turn journal's "gates terminal success" contract and
  hiding it from `RecoverStrandedTurns`.
- **A stale schema-invalid `output_json` candidate no longer blocks every
  lease-checked status write**: the structured-output gate re-validated the
  row's existing value on all updates (lease renewals, error/interrupt
  transitions included), stranding such tasks `running` until lease expiry
  requeued them — a duplicate side-effect window. Validation now applies only
  to output the current update supplies; the success gate still refuses to
  promote a stale candidate.

- **Env-file keys boot actually reads are now allowlisted**: `loadEnvFile`
  silently dropped non-allowlisted keys, and the list was missing ~16 names
  boot reads via `os.Getenv` — including `FLEET_CHAT_DATABASE_URL` /
  `FLEET_SCHED_DATABASE_URL` (documented in `deploy/fleet.service` as env-file
  keys; boot failed with an empty-DSN error despite a correct file) and
  `ADMIN_API_KEY` (admin API rejected everything while the startup warning
  claimed the key was unset).
- **`fleet mcp test` now reads the env file like a real boot**: it loaded the
  bundle without registering connector env-var names or calling
  `config.Load`, so `.env`-only credentials were invisible — credential-gated
  servers were reported as "enable gate is off" or probed with empty creds,
  the exact failure class the verb diagnoses.
- **MCP hot reload is bounded (60s, like boot)**: a stdio server that starts
  but never answers `initialize` blocked SIGHUP reload forever under the
  reload mutexes — wedging all future reloads and leaving the already-published
  new tool gates running against the old catalog indefinitely.
- **`export KEY=…` lines load from the env file** for parity with
  `internal/creds/envfile.go`, whose docs promise the two parsers agree.
- **Spawn-time `${ VAR }` interpolation trims the name** like the load-time
  pass, so a whitespace-padded reference resolves instead of going blank.
- **Editing a task's `carry_context` toggle now persists**: the column was
  missing from `UpdateTaskTx`'s SET list (the `PUT /tasks/{id}` edit path), so
  the toggle was acknowledged with 200 and silently reverted on the next read —
  every later occurrence of the recurring task ran with the stale setting.
- **ask-pause / progress notification emails now carry the message**: the
  `Event.Message` field (for a paused task, the agent's question — the reason
  the notification exists) was documented as "rendered into the email body"
  but never referenced by the text or HTML builders; operators got a
  status-only email with no question. Rendered escaped in both parts.
- **Notification task names clamp at a rune boundary**: the 60-char display
  label was a byte slice, so a multi-byte prompt could put invalid UTF-8 into
  email subjects and the webhook JSON payload (`truncate.Clamp`, the #595
  class).
- **Context-too-large recovery no longer re-executes committed tool side
  effects.** The forced-compact-and-re-drive (and its fallback-model swap) now
  ride the same ADR-0035 side-effect gate as the blip/retry-exhausted paths:
  once a tool ran in the failing attempt — the typical case, since a large
  tool result is what balloons the next request — the round surfaces
  `ErrCommittedSideEffects` so the whole-task RetryPolicy (the operator's
  explicit opt-in) owns the re-run instead of the loop silently repeating a
  send-email-class call in-run. The no-tool-events compaction retry also rolls
  back the failed attempt's partial output first, so the re-driven round
  replaces — not duplicates — it in the transcript.
- **The interactive leaked-tool-call finalize retry now streams under the
  run's ceilings**: the budget guard (pre-completion cost/token check) and the
  `CHAT_MAX_ITERATIONS` step cap are threaded into the retry via
  `FinalizeInput.GuardStep`/`StopWhen` — previously it could keep buying paid
  completions unboundedly, with tool calls only soft-blocked by policy.
- **`edit_file`/`write_file` no longer abort when the destination's ownership
  can't be preserved**: the sandbox executor's `fchown` on overwrite is now
  best-effort — an unprivileged executor overwriting a foreign-owned (e.g.
  host-seeded) workspace file keeps the executor-owned replacement instead of
  failing the whole edit with "cannot preserve destination ownership".
- **`view_file` with `offset` exactly at the file size returns a clean empty
  read** instead of an "offset is beyond file size" error — `offset += limit`
  paging that lands on the size no longer trips a spurious failure.

### Added

- **`fleet mcp test --deep` runs manifest-declared canary probes**
  (`docs/MCP-TESTING.md`): a catalog server may declare `probe:` — ONE
  read-only tool call (tool/args + optional `contains:` substring) the bundle
  author vetted for side effects — and `--deep` executes it after the
  auth-status checks, proving the upstream returns real data rather than just
  accepting the credentials. Fails the run on a not-advertised tool, a call
  error, an `isError` result, or a `contains:` mismatch; servers without a
  probe are noted, never failed. Load-time validation rejects a blank
  `probe.tool` or one outside the server's `tools:` allowlist. The runner
  only ever calls declared probes — it never auto-discovers tools to call.
- **Adoption view — exec per-user AI-usage audit** (`docs/USAGE-ANALYTICS.md`
  part 3): a new admin-only **Adoption** tab in the Operations Center backed
  by `GET /admin/usage/adoption` — per-user token/spend leaderboard with
  daily sparklines, active-day counts and engagement tiers, previous-period
  trend deltas, daily tokens/active-users trends, the provisioned-seat
  roster with a "not yet active" list, and CSV export. Strictly a read model
  over the existing metering (task_iterations ⋈ tasks + chat turn_metrics
  via two new seams); leaderboard sorts token volume first because tokens
  are the pricing-coverage-independent meter (#289), and the report carries
  the caveat that token volume is an adoption signal, not a performance
  grade. The Operations Center tabs now accept a `?tab=` deep link, and the
  Settings → Admin → Users/Overview pages label their numbers as all-time
  chat-only ("Chat spend") and cross-link to the windowed Usage/Adoption
  views instead of appearing to duplicate them.
- **Input queue + mid-turn steering** (#785, `docs/INPUT-QUEUE.md`): a
  submission during an active turn now QUEUES durably (stable ids, idempotent
  `input_id` replay, FIFO drain as separate turns) instead of implicitly
  cancelling the running turn — explicit Stop is the only cancellation, and
  it now covers queued follow-ups too (`scope=all` default). Optional
  `mode=steer` injects the message at the next safe step boundary inside the
  same governed loop: budget-accounted, cache-friendly append, durable
  queued→injected flip before model visibility, persisted exactly once via
  the #798 terminal commit, with a queued-turn fallback when the turn ends
  first. Queue chips under the composer (remove / send-now) ride the new
  `queue.updated` SSE snapshots. Migration 042.

- **Durable turn journal + commit-gated terminal success** (#798,
  `docs/TURN-JOURNAL.md`, ADR-0039): interactive turns now preserve the causal
  chain user input → tool intent → governed tool outcome → conclusion
  durably, BEFORE terminal success is advertised. The user message commits to
  canonical history before the first provider call; every tool route journals
  its call intent before dispatch (fail closed — no side effect without a
  durable record) and its exact governed model-visible result before the next
  provider step (the 4 KB SSE preview is never the persistence limit);
  `turn.completed`/`turn.cancelled` gate on the transactional history commit,
  so a failed write is a visible turn error instead of a completed answer
  that disappears on reload. Startup recovery projects crashed turns into one
  explicit interrupted turn with provider-valid call/result pairing —
  unmatched calls get a synthesized unknown-outcome error marked for
  reconciliation, so the next model verifies instead of silently repeating a
  possibly side-effectful call. Migration 041.

- **Governed lifecycle hooks** (#788, `docs/HOOKS.md`, ADR-0038): a client
  bundle can declare `hooks:` — commands run at fixed run lifecycle points
  (`user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `turn_end`) **inside
  the per-turn sandbox**. A hook receives a bounded, redacted, credential-free
  JSON payload on stdin and prints a JSON decision (continue / block-with-reason
  / bounded additional-context). Hooks can only observe or NARROW — fleet's
  existing policy/approval/audit gates evaluate after them on the same
  unmodified input, so a hook can never widen authority, add tools, or grant
  network/budget/approval. Enforcing hooks fail closed; advisory hooks fail
  observable; every invocation emits a `hook.decision` audit event. The generic
  bundle ships none (zero-overhead default). Both interactive and scheduled runs
  invoke hooks through the one governed core.

- **Save a chat to the prompt library**: a per-conversation kebab action
  ("Save to prompt library…") distills a conversation into a reusable
  prompt-library draft. `POST /conversations/{id}/suggest-prompt` (chat server)
  renders the user/assistant transcript and calls the host-side synthesizer
  `SuggestLibraryPrompt` (`FLEET_LIBRARY_PROMPT_MODEL`, defaulting to the
  metadata/title model) to write a clean, self-contained prompt plus a short
  name and description. Nothing is persisted server-side: the client opens the
  draft in an editable review dialog, and saving goes through the orchestrator's
  existing `POST /prompts` (private by default, optionally workspace-shared), so
  library permissions apply unchanged.
- **Safer `edit_file`** (#787, `docs/FILE-EDIT-SAFETY.md`): `old_text` must now
  match exactly one location unless `replace_all` is set (an ambiguous match is
  rejected with the count instead of silently editing the first occurrence);
  no-op edits are rejected; an optional `expected_hash` (SHA-256 from
  `view_file`) fails the edit safely if the file changed since it was read; and
  a successful edit returns a bounded unified diff plus old/new content hashes.
  `view_file` and `write_file` now report the content SHA-256 so the model can
  round-trip it as `expected_hash`.
- **Admin server status**: Settings now includes an admin-only **Server** tab
  with a lightweight, auto-refreshing view of CPU/load, memory, root-disk,
  uptime, and aggregate non-loopback network traffic. The read-only endpoint is
  role-gated and deliberately omits process, environment, address, and
  filesystem-detail data.
- **Hybrid prompt library** (`docs/PROMPT-LIBRARY.md`): Chat and Operations
  Center now share a searchable prompt picker that live-loads read-only
  `.yaml`/`.yml`/`.md`/`.txt` entries from the client bundle's Git-trackable
  `prompts/` directory and merges them with UI-authored private or
  workspace-shared prompts. Authors/admins can edit or delete database-backed
  entries, seed a new entry from the current draft, and download a versioned
  JSON backup suitable for ordinary cloud-drive storage. The generic bundle
  includes a neutral example prompt.
- **Resilient live Operations logs**: the task activity viewer now reconnects
  dropped fetch-based SSE streams and forwards `Last-Event-ID`, resuming at the
  next tool/message event instead of silently freezing or replaying the live
  buffer from the start. Tool details are now expandable rather than clipped at
  600 characters. Live cancellation continues to interrupt the governed run
  and records the stopping operator; task creators can now stop their own jobs
  without gaining permission to stop a teammate's job.

- **`fleet mcp test` (docs/MCP-TESTING.md)**: per-server smoke test for the
  bundle's MCP catalog — loads the bundle through the boot loader (same env
  interpolation, enable gates, TLS), spawns/handshakes each server exactly as
  the broker would (`initialize` + `tools/list`), and reports tools or the
  actionable failure per server. `--all` sweeps the catalog, `--json` for CI
  gates; exit 0/1/2. Boots nothing (no DB, no server, no sandbox) — run it
  where the deployment's env lives. `--deep` additionally calls each
  server's advertised auth-status tool (`auth_status` / `*_auth_status`) to
  verify the credentials against the upstream, failing the run on an
  `isError` result.

### Changed

- **File tools (`view_file`/`write_file`/`edit_file`) now execute inside the
  sandbox** (#784, ADR-0036). They previously ran `os.ReadFile`/`os.WriteFile`
  directly in the Fleet host process, contradicting the ADR-0002 "every tool
  call runs in the sandbox" invariant — a configured Kata/libkrun runtime,
  seccomp, dropped caps, cgroups, and disk/PID limits did not apply to them.
  The three tools now dispatch read/write/edit through a new sandbox FileOp
  seam (`Sandbox.RunFileOp`) that runs a one-shot `python3` in the same
  per-turn container as bash/run_python, inheriting the full isolation posture;
  the executor also confines each call to a narrow conversation/worktree root
  with a boot-bound root identity and dirfd-relative no-follow traversal,
  defeating both interior symlink swaps and whole conversation-directory
  exchanges against the shared workspace mount. Atomic replacements preserve
  existing modes and fsync the parent; cancellation synchronously kills and
  retires the container so a helper cannot land a late rename. Host-side path
  validation stays as defense-in-depth input, and there is no host-execution
  fallback (the tools fail closed without a sandbox). Scheduled non-worktree
  turns now resolve relative tools against that same workspace root. The
  existing host-temp truncation spill remains an honestly documented temporary
  exception pending #793's governed artifact lifecycle. ADR-0036 also documents
  the
  remaining host-side control-plane/broker tools (network fetch, brokered
  credentials, governed datastore writes) as explicit, threat-modelled
  exceptions rather than a silent contradiction of the invariant.

- **Bounded model-visible tool output**
  ([docs/TOOL-OUTPUT-BOUNDARY.md](docs/TOOL-OUTPUT-BOUNDARY.md)): every native,
  loader, direct/deferred MCP, and media result now crosses a
  non-disableable 128 KiB hard boundary with valid structured envelopes,
  binary suppression, and dedicated metrics. Governed full-output recovery now
  writes only through the confined sandbox FileOp seam into 16 immutable,
  8-MiB workspace slots for private conversations and isolated worktree tasks;
  shared-root non-worktree tasks remain cap-only because they do not own the
  root. The old host `/tmp` bash/Python/web-fetch spills and 6 MiB
  overflow-file pass are gone. Fantasy's inner PrepareStep now budgets
  accumulated tool results and call inputs against the active model window
  while reserving the system prompt, tool schemas, completion, and provider
  framing.

- **Removed the stale Projects button from the collapsed sidebar**: the ≥sm
  icon strip carried a standalone briefcase button that opened the old
  Projects modal — leftover from before the rail's own Projects section
  (#509) became the entry point. Dropped the button and its `onOpenProjects`
  wiring; creating, opening, and pinning projects from the expanded rail is
  unchanged.


- **Fleet application icons**: browser favicons, iOS/Android install icons, and
  the maskable launcher icon now derive from one checked-in Fleet mark. App
  Router file conventions eliminate duplicate icon metadata, and installed-app
  labels now follow `NEXT_PUBLIC_APP_NAME` instead of the old hardcoded name.
- **Coherent latest-Fedora sandbox policy**: the generic sandbox upgrades from
  `fedora-minimal:latest` and installs current Fedora packages on every rebuild,
  without hand-maintained pip overlays that can conflict with RPM ownership.
  Grype continues to publish every finding, while the merge gate blocks only on
  fixable CRITICAL Fedora RPM findings that the image can act on. Client bundles
  may still pin a base digest, package NEVRAs, or a prebuilt image when they need
  reproducibility.
- **Public-repo hygiene (#721)**: untracked the runtime `fleet.pid` file (now
  gitignored along with `/workspace/`), removed the dead
  `web/scripts/smoke-mcp-reporting.py` (bundle repos own their smoke tests),
  and replaced deployment-specific strings in generic content — private bundle
  paths/repo names, internal hostnames in handler tests, personal emails in
  webhook examples, and vendor credential names in `deploy/fleet.service` —
  with `example.com`-style placeholders. The out-of-repo bundle sanity test is
  now `internal/clientconfig/bundle_sanity_test.go`, opt-in via
  `FLEET_SANITY_BUNDLE_DIR` (skips cleanly when unset) and deployment-neutral.
  A README naming note clarifies that the v1 names (chat/moc/gig/cutlass)
  refer to the internal predecessor stack this project replaces.

### Added

- **Connector-directory onboarding** (`docs/CONNECTOR-ONBOARDING.md`): every
  hosted-directory entry is now either connectable in place or visibly
  documented. Per-user remote MCP gains an **API-key auth mode** (migration
  039: key sealed AES-256-GCM host-side like OAuth tokens, replayed as
  `Authorization: Bearer` or the entry's `api_key_header`; rotation via
  `PUT /remote-mcp-servers/{id}/key`) — the 60 `api_key` catalog entries
  previously had no working connect path. api_key and open adds (and key
  rotations) are **validated with a real MCP handshake** before anything is
  stored: a rejected key or unreachable URL fails the add with an actionable
  error and the guided form keeps the typed values, while a successful add
  confirms with the observed tool count. Directory cards grow **guided add
  forms**: per-`{placeholder}` inputs with a live URL preview for
  tenant-scoped endpoints, a write-only key field for api_key entries, and
  bring-your-own OAuth client fields for vendors without dynamic client
  registration (`client_registration: manual` — Google Workspace, Microsoft
  Work IQ, Slack, HubSpot). New catalog fields `setup_hint` (rendered visibly;
  CI-required for every tenant/api_key entry) and `setup_url` link the
  vendor's actual connect walkthrough; hints were researched and authored for
  all 97 such entries. Catalog refresh: new `google-people` (Contacts) entry
  plus community self-hosted `google-workspace-self-hosted`
  (Docs/Sheets-capable) and `microsoft-365-self-hosted` (no Copilot license
  needed) entries; auth corrections for aws-mcp and the key-in-URL vendors
  (Scrapfly, thirdweb, Smartlead). A curation audit removed 25 low-quality
  listings (docs-search-only servers for niche products, unworkable auth
  schemes, obscure duplicates) and added 15 newly-verified official hosted
  endpoints (Docusign, Adobe for Creativity, WordPress.com, Contentful,
  Sanity, Algolia, Cloudinary, Amazon Ads, DoorDash, ServiceNow, Microsoft
  Dataverse/Dynamics 365, Vimeo, Egnyte, Granola, Jotform) — net 267 → 282.
  A new **Featured shelf** (`featured: true`, capped 8–20 entries) surfaces
  the household names — the Google Workspace trio, GitHub, Notion, Slack,
  Linear, Atlassian, Asana, monday, Airtable, Stripe, PayPal, HubSpot,
  Canva, Figma, Adobe, Zapier, Hugging Face — ahead of the category listing.
  The "remote MCP isn't configured" notice is now admin-aware instead of
  showing env-var instructions to members.

- **v1 → fleet cutover runbook** (`docs/CUTOVER.md`, #718): the ordered
  operational sequence for a box already live on the legacy chat + moc stack —
  backups, stopping **and disabling** the legacy units (and the runner daemon
  on worker boxes), exporting with the correct env sourcing, bootstrapping
  with an explicit DB decision, importing, copying the non-DB state (moc task
  files and the legacy chat attachment dir), port/Caddy coexistence, minting
  fleet API keys for external intake callers, DNS, and post-cutover cleanup.
  Linked from `DEPLOYMENT.md` and `LEGACY-IMPORT.md`; `LEGACY-IMPORT.md`'s
  runbook now disables (not just stops) the legacy units, sources
  `/etc/fleet/fleet.env` on systemd deploys, and covers the chat data dir;
  `BACKUP_RESTORE.md` now names the on-disk state (attachments, task uploads,
  workspaces) that `fleet backup`'s DB dumps do not capture.

- `bootstrap.sh` DB flags (#718): `--chat-db-name`/`--chat-db-user`/
  `--sched-db-name`/`--sched-db-user` (fresh names that dodge a legacy
  collision) and `--adopt-existing-chat-db`/`--adopt-existing-sched-db` (skip
  provisioning for that pair — no CREATE, no ALTER, no password rotation — and
  take the operator-supplied DSN, validated with `SELECT 1`).

- **Per-task wall-clock timeout** (#724): `FLEET_TASK_WALL_TIMEOUT` (default
  4h, `0` disables) bounds a scheduled run's total elapsed time; on expiry the
  run is cancelled and the task fails with a clear, deterministic timeout error
  that never consumes the transient-retry budget. Documented in
  `docs/AGENT-RUNTIME.md` alongside the other enforced ceilings.
- **`fleet sched task list`** (#722): the daily-driver task listing (short id,
  name/prompt excerpt, status, priority, recurrence/scheduled time, model) with
  `--status`, `--limit`, and `--json`; plus `fleet start` alongside
  stop/restart, and top-level help for the shipped-but-unlisted verbs
  (sched trigger/dlq, apikey rotate, task memories).
- **Scoped keys can upload** (#719): `POST /v1/upload` accepts a scoped API key
  with create_task permission — external intake apps no longer need the
  full-access admin key to attach files to a create. Admin key / bearer /
  cookie paths unchanged; under-scoped typed keys get 403.
- **Task serialization** (#709): `TaskCreate` accepts an optional opaque
  `serialization_key`; fleet guarantees at most one task per key is active
  (leased/running) at a time, enforced by an advisory-locked re-check at the
  scheduler's claim gate plus a best-effort queue-visibility filter. A blocked
  task is skipped (stays queued), never failed. Recurring occurrences inherit
  the key. See `docs/TASK-SERIALIZATION.md`.
- The legacy migration bundle carries `serialization_key` on sched tasks
  (#712), and `fleet import` gained `--overwrite` for the restore-from-bundle
  use case.
- Scheduled task inputs support logical `file_names` paired with stored upload
  names. Dedicated MCP runs materialize them in `${FLEET_WORKSPACE}/inputs`,
  and bundles may use `${FLEET_TASK_ID}` for connector-side task attribution.

- **Start-of-run create reconciliation** (#717): a scheduled run whose resolved
  MCP workspace holds unresolved pre-POST markers in the bundle servers'
  cross-run create ledger (`creates.jsonl`) gets the v1-byte-compatible
  reconciliation block appended to its task prompt, so a retried/resumed task
  verifies whether creates landed before issuing any new create. Appended to
  the task (user) portion only — the cached system prefix stays byte-stable.
  See [ADR-0034](docs/adr/0034-audit-gate-commitment-binding.md).

### Changed

- **Ops Center loads without the markdown pipeline**: the /orchestrator
  bundle statically shipped react-markdown (~43 KiB transfer) for the task-log
  modal only. `LogMarkdown` (renderer + workspace img/a rewrites, moved
  verbatim from LogViewer) is now lazy-loaded when a log modal actually opens,
  with a raw-text fallback until the chunk arrives — the same split #757 gave
  chat. Note: the ~13 KiB of legacy polyfills Lighthouse flags in the main
  chunk is Next's own baseline polyfill module, injected regardless of
  browserslist targets — verified by building with modern targets — so there
  is no supported way to drop it and no config was added for it.
- **Faster first paint on mobile (Lighthouse)**: the initial /chat bundle no
  longer ships the syntax highlighter (~75 KiB transfer; now lazy-loaded on
  first tool-chip expand with a plain `<pre>` fallback) or the ReactMarkdown
  pipeline (~43 KiB; lazy-loaded when a transcript message renders, showing
  raw text until the chunk arrives), and cold boot skips the serial
  `/api/session` round-trip by reusing the session the /chat server component
  already resolved. Together: ~40% fewer JS bytes and one less blocking RTT
  before the largest contentful paint.

### Fixed

- **A panic in any tool no longer crashes the whole fleet process**
  ([ADR-0037](docs/adr/0037-agent-tool-panic-containment.md), #795).
  fantasy runs streamed tool calls in unsupervised goroutines with no
  `recover`, so a panic in a tool (native/loader/MCP), a policy gate,
  an output guardrail, or an Observer callback would take the entire
  single-host process down — beyond the reach of the runner/httpapi
  goroutine-local recovers. Every tool fleet registers is now wrapped in an
  outermost panic-containment layer: a panic becomes exactly one in-band tool
  error result (paired to the call so the run continues), is recorded to
  PanicCounts/Sentry/`panic_events` with tool + boundary attribution, and is
  surfaced to the model as a stable incident id with a "possibly executed"
  warning. Raw recovered values and stacks are discarded before telemetry;
  only an opaque incident and value-free class are retained. Before-call,
  execution, and output panics receive exactly one failed logical-tool policy record, including
  deferred MCP. Observer panics are recorded and disabled, then surface as an
  ordinary run error only after Fantasy's tool goroutines settle.
  Tool-supplied Go errors are now flattened under the same boundary, screened
  and bounded before Fantasy sees them, so credential-bearing or panicking
  `Error()` methods cannot leak or break transcript pairing.
  The unused arbitrary `PreGatedTools` bypass was removed because a black-box
  tool that owned policy accounting internally made exactly-once recovery
  impossible to prove. Fleet now owns policy accounting for every externally
  supplied tool route.

- **Cancelled or timed-out bash no longer keeps running inside the sandbox**
  (#796). Cancellation used to kill only the host-side `podman exec` client;
  the in-container process tree survived — durable in persistent-REPL mode,
  where the next turn could share the container with a stopped turn's
  stragglers still mutating files or completing external side effects. On
  cancel/timeout the container is now SIGKILLed synchronously — tearing down
  its PID namespace kills every descendant, including a `setsid` daemon or
  backgrounded grandchild that escaped the process group — and the sandbox is
  poisoned so the persistent pool retires it and the next turn gets a fresh
  container. The tool result distinguishes timeout from cancellation and says
  when the sandbox was reset.
- **Declared structured output now fails closed.** A scheduled task with
  `output_schema` can reach success only when schema-valid `output_json` is
  committed in the same lease-checked transaction. The active OpenRouter model
  uses strict native schema mode; other providers use a forced terminal schema
  tool with two correction attempts and no ordinary tools. Format and
  persistence failures have distinct retry/DLQ classes, and success
  notifications/recurrence happen only after the output is durable.
- **Enforcement rounds no longer restart the task from scratch.** When the
  policy blocked a finish (audit not confirmed, critical actions pending),
  the next round's input carried only the original prompt plus the nudge —
  the blocked round's assistant/tool transcript was dropped, so the model
  re-ran its entire analysis on every nudge (observed as a 31-minute
  scheduled run doing its full pipeline twice). The round transcript is now
  carried forward (reasoning stripped, tool call/result pairing preserved)
  so a nudged round continues the work instead of repeating it.
- **Orchestrator (scheduled) runs no longer offer the interactive
  staging-card tools** (`preview_email`, `schedule_task`,
  `suggest_advanced_model`, `propose_memory`). Only the interactive chat
  orchestration guard gives them behavior; headless, three of them are
  mis-wiring tripwires whose non-nil error is fatal to the run — a scheduled
  agent that finished its report and called `preview_email` to present it
  dead-lettered the entire task. The docs always said these were
  interactive-only; the scheduled roster now actually excludes them.
- **`publish_artifact` works for non-worktree scheduled tasks.** It resolved
  paths only via the worktree-isolation working dir, so every task without
  worktree isolation got "no workspace is configured for this run" on every
  publish attempt. The tool now falls back to the run's effective workspace
  root (the same directory the workspace file browser serves); worktree runs
  keep their narrower scoping.
- **A transient mid-stream provider failure no longer dead-letters a whole
  run once any text has streamed** (ADR-0035). The ADR-0033 commitment guard
  suppressed the in-place stream-blip retry and the fallback chain after any
  semantic event, so a single 504 while the model composed its answer killed
  the task as `non_retryable`. Recovery is now gated on tool side effects:
  a text/reasoning-only attempt rolls its partial output back and re-drives
  (in-place retry, then fallback); an attempt that already executed a tool
  still suppresses in-run recovery but surfaces the transient
  `ErrCommittedSideEffects` sentinel so the task's RetryPolicy — not a
  deterministic-failure dead-letter — decides the re-run.
- Deferred MCP tools now advertise `arguments` as a JSON object instead of a
  byte array, preventing models from repeatedly sending array-wrapped arguments
  that every nested tool rejects. Live scheduled-run activity now keeps the
  task plan in a compact expandable progress panel, groups repeated failures,
  and persists structured tool calls/results so terminal logs retain the real
  cause of an incomplete run.
- **`fleet update` no longer breaks sandbox starts after a bundle pull**: the
  updater runs as root, so files a client-bundle `git pull` created or rewrote
  came out root-owned — and the sandbox bind-mounts bundle dirs with an SELinux
  relabel (`:z`) that rootless podman may only apply to files the service user
  owns. One root-owned `protocols/*.yaml` was enough to dead-letter every task
  with `podman run: exit status 126` / `lsetxattr … operation not permitted`.
  The bundle-pull step now re-applies service-user ownership (mirroring
  `bootstrap.sh`), also healing checkouts a previous root-run pull broke.

- **Cookie users can mutate orchestrator state from the web UI again**:
  launching or editing a task while signed in with the elcano session cookie
  died with `403 Cross-origin request blocked`. The Next.js orchestrator proxy
  forwards the user's identity as `X-User-Email` + `X-Orchestrator-Server-Token`
  without the browser's `Origin` header (it enforces same-origin itself before
  forwarding), but the backend's `CSRFMiddleware` treated the missing Origin as
  cross-site. The shared-token header now joins `X-API-Key`/`Bearer` in the
  CSRF exemption list — browsers can't attach custom headers cross-site, and
  the auth middleware still fail-closed rejects a wrong token value. The
  in-handler auth of the routes registered outside the auth group
  (`POST /tasks`, `/tasks/batch`, `/tasks/estimate`, `/upload`) now honors the
  same header-trust identity via a shared fail-closed helper — previously it
  only accepted admin/API keys, bearer tokens, or the raw cookie, so the
  proxied cookie request would have cleared CSRF only to die with 401.

- **Sandbox Soup Sieve ReDoS (GHSA-836r-79rf-4m37)**: replaced Fedora's
  vulnerable `soupsieve` 2.8.3 RPM payload with upstream 2.8.4 and removed the
  old RPM metadata from the final image so Grype no longer reports the
  vulnerable package.

- **`bootstrap.sh` can no longer hijack a legacy database or truncate a
  foreign Caddyfile** (#718). Local-mode provisioning refuses when a
  role/database matching the configured names already exists on the cluster
  but is not recorded in fleet's env file by a previous run — previously the
  unconditional `ALTER ROLE … PASSWORD` silently rotated the legacy `chat`
  role's password (locking out the still-installed legacy server and any later
  `chat-admin export`) and fleet's migrations then ran on the legacy database.
  The password-converging `ALTER` now provably applies only to roles the
  script itself provisioned. Likewise `--enable-web --domain` refuses to
  overwrite an `/etc/caddy/Caddyfile` it did not write; `--force-caddy`
  overrides with a timestamped backup (`Caddyfile.fleet-backup.<ts>`) and a
  loud merge warning.

- `deploy/fleet.service` gained `Wants=postgresql.service` alongside the
  existing `After=` (#718): `After=` only orders units and never starts
  Postgres, so on a `--postgres=local` box whose cluster unit was not enabled,
  a reboot brought fleet up against a down database (`Restart=always` churn
  until someone started Postgres by hand). `Wants=` (not `Requires=`) is
  ignored when the unit is absent, so external-Postgres deploys are unchanged.
- **Audit-gate commitments bind to the full tool name, record ids, and values
  digest** (#715): a typed `confirm_audit` `critical_actions` entry now
  registers a commitment keyed by the full server-qualified MCP tool name plus
  its optional `deal_id`/`deal_ids` record binding and `values_digest`.
  Suffix-count matching had let one approval authorize — and be silently
  discharged by — a same-suffix tool on a different server, variant, or record;
  batches carried no id/digest binding at all. Typed audits now fail closed:
  zero resolved commitments authorizes nothing — refused outright when an
  entry named a critical suffix without its full server-qualified name, and
  accepted-but-empty (every critical call still blocked) for explicit no-op
  declarations. The bare-suffix path survives only as the legacy fallback for
  untyped audits and can never approve a server-side batch.
  See [ADR-0034](docs/adr/0034-audit-gate-commitment-binding.md).

- **Payload-level MCP failures no longer discharge critical-action
  commitments** (#716): a critical tool result whose payload reports failure
  (`{"success": false}` or a non-empty top-level `"error"`) over a clean
  transport now counts as a FAILED execution — it discharges nothing, counts
  against the retry budget, and keeps finish enforcement armed. This
  generalizes the previous `send_email`-only payload check to every critical
  tool; batch tools returning per-record `results[]` discharge per succeeded
  record, idempotently, and never for record ids the audit did not approve.

### Changed

- **Unknown personas are rejected at task create** (#720): a non-empty
  `persona` not present in the client bundle is now a 400 listing the valid
  names, instead of silently dispatching on the global default persona. Tasks
  whose persona disappears from the bundle after creation still fall back at
  dispatch. `docs/openapi.yaml` TaskCreate now documents the previously-missing
  `name`, `persona`, `credential_allowlist`, `tags`, `timezone`, `description`,
  `max_retries`, and `carry_context` fields, and `/upload`'s security note
  reflects the scoped-key change.
- **`fleet sched apikey create --rate-limit-per-minute`** (#722) names the
  rate-limit unit; `--rate-limit` remains a deprecated alias that warns (the
  v1 tooling's flags were per hour — porting numbers 1:1 set a 60× stricter
  cap). The dev fast lane (`dev-ci.yml`) now runs with a Postgres service so
  DB-gated suites — including the `fleet import` tests — run on every dev push
  (#723).

### Fixed

- **Batch task creation was broken on dev** (#710 regression): the `file_names`
  column was added to the task insert without bumping
  `taskInsertColumnsCount`, so every multi-row `POST /tasks/batch` INSERT
  failed with "INSERT has more target columns than expressions". Caught the
  moment the dev lane gained a Postgres service (#723); a DB-free test now
  pins the column/arg/count triple.
- **Chat usage tests no longer accumulate rows**: migration 038 dropped
  `turn_metrics`' FK to conversations (so usage survives conversation
  deletion), which silently removed it from the test-isolation
  `TRUNCATE ... CASCADE`; it is now truncated explicitly.
- **`fleet import` re-runs no longer revert live task state** (#713): sched
  tasks and run logs whose UUID already exists in fleet are skipped by default
  (previously upserted — a completed one-shot flipped back to pending and ran
  again, and fleet-side run history was replaced). Import validation and
  dry-run parity were tightened alongside (#714): warnings for MCP
  opt-ins/personas missing from the loaded bundle and for inert
  scheduled-without-schedule tasks, the same-database guard now covers
  single-section bundles on the shared `DATABASE_URL` fallback, dry-run
  memory/orphan-log checks match the real run, `--live-only` skips are no
  longer mislabeled "already present", flags may precede the bundle path, and
  a chat memory lookup failure no longer aborts the section mid-run.
- The task batch/tx insert paths bound one more argument than their
  placeholder count after the `file_names` column landed
  (`taskInsertColumnsCount` stayed at 60 for 61 columns); the constant is now
  pinned by a drift test.
- Test-suite portability: the chat store's test truncation now clears
  `turn_metrics` (its conversations FK was dropped by migration 038, so it
  stopped cascading and the usage-analytics tests accumulated rows across
  runs), `projects`, `user_connector_prefs`, and `user_skills`; the path
  traversal test no longer assumes the checkout lives outside `os.TempDir()`;
  and `update.sh` accepts a git-worktree checkout (`.git` pointer file).
- **Remote MCP OAuth callbacks use the deployed Fleet domain**: web-enabled
  bootstrap runs now write the same computed public origin to the backend's
  `FLEET_PUBLIC_BASE_URL` and generate the token-encryption key once; normal
  `fleet update` runs also reconcile existing installs from the deployed web
  origin before restarting. A
  `--domain` deployment therefore registers `https://<domain>/api/oauth/mcp/callback`
  instead of retaining a localhost callback.

- **Open-access hosted MCPs no longer get forced through OAuth discovery**:
  one-click catalog adds now preserve the entry's authentication mode. Servers
  such as AWS Knowledge are stored ready to use without a bearer header, so an
  intentional GET redirect to provider documentation no longer fails setup.
  Redirects remain disabled for remote MCP traffic and OAuth discovery.

- **The bundled MCP directory no longer advertises a nonexistent hosted X API
  server**: `https://api.x.com/mcp` returns 404 and X documents its API-capable
  XMCP as self-hosted. The valid open-access X Docs MCP remains available at
  `https://docs.x.com/mcp`, while operators can still add a separately hosted
  XMCP deployment as a bundle-authored connector.

- **Weekly task schedules support multiple days**: the Create New Task modal's
  simple repeat editor now accepts combinations such as Monday and Wednesday.
  Supported cron weekday lists also hydrate the same selected-day UI when
  switching back from Advanced cron.

- **Deleting chats no longer resets user usage totals**: chat cost and token
  metrics now survive conversation deletion and retention cleanup. Transcript
  content is still deleted; only the existing non-content accounting row is
  retained, with unavailable model/project attribution grouped as unknown.

- **Task scheduling controls fit cleanly on mobile**: the Create New Task modal
  now contains its date and time pickers with consistent labels and padding,
  and its Repeat tab defaults to a plain-language daily/weekday/weekly builder.
  Advanced cron remains available without an overlapping clock icon.

- **Chat labels remain editable with mobile keyboards**: viewport resizes caused
  by opening a software keyboard no longer dismiss the conversation menu while
  its label input has focus. Ordinary viewport resizes still close open menus.

- **Default core tier drops `:nitro` for cache locality**: the default
  `z-ai/glm-5.2:nitro` slug asked OpenRouter for throughput-priority
  routing, spraying requests across the ~26 providers serving GLM-5.2 —
  and prompt caches are per-upstream, so the implicit-cache discount
  (~80% off cached input) almost never hit. The default is now the plain
  `z-ai/glm-5.2`, soft-pinned to the Z.AI upstream via `canonicalUpstream`
  (Order + fallbacks allowed, same policy as the Anthropic/OpenAI/Moonshot
  pins). Verified live: the second pinned call served 3,968 of 4,018 prompt
  tokens from cache; with `:nitro` the same pair read 0. Trade-off: routing
  now prefers Z.AI's own endpoint over whichever provider is momentarily
  fastest; fallbacks still engage if Z.AI is down.

- **Prompt caching works on native LLM providers**: a model served by a
  native provider (#289) reports its provider-local slug (`claude-fable-5`,
  no `anthropic/` prefix), which failed `supportsExplicitBreakpoints`'s
  prefix match — so native-Anthropic runs got **no** `cache_control`
  breakpoints and paid full input price on every turn (~10× the cached rate
  on the strong tier). Bare `claude-`/`gemini-` slugs now match; fantasy's
  native Anthropic provider reads the same per-message marker shape the
  OpenRouter hook does, verified live against OpenRouter (99.8% cache-read
  on the second call).
- **Compaction summaries are cache boundaries now**: the shared loop enables
  `WithCompactionSummaryBreakpoint`, so the summary message a compaction
  pass inserts carries its own breakpoint (4 total — Anthropic's maximum)
  instead of the tail markers having to re-cover it. The option existed and
  `interactive.go` tagged summaries for it, but no production caller ever
  enabled it.
- **1M-context beta header follows the active model**: `anthropic_beta:
  context-1m` was attached whenever the *configured* primary or fallback was
  a long-context Claude, so it also rode along on requests actually served
  by the other (possibly non-Claude) model. It is now gated on the active
  slug, the same rule extended thinking already used.

- **Settings sections no longer jump in width while navigating**: the settings
  shell's scroll container now reserves a stable scrollbar gutter
  (`scrollbar-gutter: stable`). Sections differ in height — General and the
  Admin overview fit the viewport while Connections and Skills scroll — so on
  classic-scrollbar platforms (Windows/Linux) the scrollbar appeared and
  disappeared across navigations, resizing the centered layout and shifting
  the sub-nav and content sideways. Every section now renders at the same
  width, including while Connections/Skills catalogs are still loading.
- **Bootstrap re-runs no longer lock admins out of the Operations Center**:
  `scripts/bootstrap.sh` wrote `ORCHESTRATOR_SERVER_TOKEN=<ADMIN_API_KEY>` into
  `/etc/fleet/fleet-web.env`, but the orchestrator's header-trust path verifies
  `X-Orchestrator-Server-Token` against the chat shared secret
  (`FLEET_SERVER_TOKEN`), fail-closed — so any bootstrap (re-)run 403'd every
  cookie-authenticated Operations Center request until the env file was
  hand-repaired. Bootstrap now mirrors `FLEET_SERVER_TOKEN` into both
  `CHAT_SERVER_TOKEN` and `ORCHESTRATOR_SERVER_TOKEN`, matching the middleware
  and the web tier's documented fallback.
- **Operations Center model picker actually lists the catalog again**: the
  task form's picker fetched the OpenRouter catalog directly from the
  browser, which the app's Content-Security-Policy (`connect-src 'self'`,
  #590) silently blocks — stranding the picker on its two seed models. It now
  loads through the existing session-gated `/api/model-catalog` proxy, so the
  full text-model catalog (plus pricing for ranking) is searchable again.

### Docs

- **PROMPT-CACHE-CONTRACT.md**: records the day-granular Runtime Date
  Context as the sanctioned exception to the no-`time.Now()` rule (one
  cache miss per conversation per UTC-midnight rollover, deliberate), and
  documents the provider-local slug forms + the now-default compaction
  breakpoint.

### Added

- **MCP bundle env contract for the cutlass-family servers**
  (docs/MCP-BUNDLE-ENV.md): the reserved `${FLEET_WORKSPACE}` manifest-env
  token substitutes a fleet-provided writable workdir at MCP subprocess launch
  (per-run for a scheduled task's dedicated client, a stable per-deployment
  dir for shared spawns; token-bearing keys are dropped when no dir is
  offered), so bundles can wire `CUTLASS_RUN_WORKDIR`-style vars without
  hardcoding paths. Account-variant spawns now inject
  `MCP_VARIANT_CLIENT=<account>` (cutlass mcp_loader parity), and the new
  per-server `identity_env` manifest list refuses a partially-suffixed named
  account whose identity-routing vars would silently inherit the default
  seat. Bundle-declared `agent_policy.critical_tools` suffixes are now staged
  through the interactive approval-card UX too (previously scheduled-mode
  audit gating only), honoring session pre-approvals and the #225 per-tool
  timeout chain.
- **Host-side prompt-injection guardrails (#702)**: optional workspace-wide
  `off` / `observe` / `block` screening covers seed user/task messages and tool
  output before it enters model context. Operators bring a local HTTP detector,
  configure it live in Admin Features, and can test it with a synthetic sample;
  block mode fails closed while observe mode reports outages without blocking.
- **Cross-provider LLM failover (#703)**: client bundles can declare an ordered
  `fallback_providers` chain. Retryable failures can promote the same model to
  the next eligible backend before streaming begins; explicit provider pins stay
  isolated, model-level `fallback_model` remains distinct, and Fleet suppresses
  failover after any semantic event to prevent stream splicing or duplicate
  side effects.

- **Workspace-provider models in the chat picker**: models from
  admin-configured LLM providers (Settings → Admin → Model providers) now
  appear in the chat composer's model menu with a "workspace" pill — they
  previously surfaced only in the task form's picker. Labels and context
  windows resolve for the chip and the context ring too.
- **Catch-all providers get a browsable catalog (catwalk)**: an
  Anthropic/OpenAI provider saved with an empty models list (= serves any
  slug) previously contributed nothing to the pickers. The member-level
  `GET /llm-provider-models` read now also returns the enabled provider
  roster (name/type/catch_all — no secrets), and the web tier expands such
  rows into `<provider>/<model>` entries from the
  [catwalk](https://github.com/charmbracelet/catwalk) model database (the
  same no-auth catalog Charm's Crush uses) via a new session-gated
  `/api/catwalk-models` proxy with a 24 h cache. `CATWALK_URL` overrides the
  default `https://catwalk.charm.land` for air-gapped deployments; failures
  degrade to typed slugs. See `docs/LLM-PROVIDERS.md`.

### Changed

- **New Task modal redesign**: the Operations Center create-task form is now
  grouped into titled sections (Task · Schedule · Email · Tools & files ·
  Advanced) with consistent field styling — several controls (the advanced
  toggles, run_if row, tags/persona inputs) previously rendered with no CSS
  at all. Real toggle switches with full-row click targets, a two-column grid
  for schedule/model/limit fields, the custom cron input promoted next to the
  presets (click an active preset again to clear it), a Cancel button, and
  Estimate Cost anchored at the footer's start. Field IDs, validation, and
  submit behavior are unchanged.
- **Chat share UX**: creating (or revisiting) a share link now opens a
  proper dialog — the full URL in a selectable field, an explicit "Copy
  link" button with copied-state feedback, a read-only-access explainer,
  and "Stop sharing" — instead of only flipping a small sidebar icon and
  silently writing the clipboard (which browsers can block with no
  feedback). The shared-row menu item is now "Share link…".
- **schedule_task approval card is editable**: the "Make recurring task"
  proposal (and any agent-staged schedule_task) can be adjusted before
  approving — Edit… exposes the name, cron schedule (recurring tasks),
  and the FULL prompt (the card previously showed a truncated preview
  only); "Approve with changes" sends just the changed fields, and the
  server applies them to the staged args BEFORE the same validation and
  single task-create path the unedited flow uses. Nothing is created
  until approve, as before.

- The **Branch** button appears as soon as a turn finishes, not after a
  reload. The turn stream gains a `history.persisted` event: after the
  server persists the turn's history rows it tells the live stream which
  DB ids the messages became, and the client backfills them — the Branch
  affordance (which gates on a persisted id) enables immediately. Mocked
  turns emit the same event, so Playwright covers the flow.
- Model picker polish: the two pinned rows are named in the same
  "Lab: Model" style as every other row ("Z.AI: GLM 5.2 (nitro)",
  "Anthropic: Claude Fable 5"); Z.AI joins the per-lab "newest model"
  curation (listed first); and a lab's newest entry is excluded
  variant-insensitively when it IS the pinned model (a ":nitro" pin and
  its base catalog slug no longer render as duplicate rows).
- The login MOTD banner is now a rowing galley (oars out) instead of the
  small masted-ship mark.

- **Model lineup + no price ceiling + alias retirement.** The recommended
  everyday model is now `z-ai/glm-5.2:nitro` and the strong tier is
  `anthropic/claude-fable-5`, across chat (picker pins, new-conversation
  default, title/metadata fallbacks, lockdown-mode default allow-list) AND
  the Operations Center (task-create primary/fallback defaults + picker
  seeds). The user-facing "default"/"advanced" ALIASES are retired: the
  picker's two pinned rows now show real model names ("GLM 5.2 Nitro",
  "Claude Fable 5") with the existing "recommended" pill, and the
  escalation flow (`suggest_advanced_model`), the spreadsheet nudge, and
  the model chip all render the actual model name — the strong-tier ROLE
  survives (the agent can still suggest switching up), only the aliasing
  is gone. Both price ceilings on model selection are removed (the chat
  picker's $30/M-output cap and the Operations Center picker's $8/$30 per-M
  caps): any OpenRouter model is pickable, and spend stays governed by the
  per-run cost ceilings (`FLEET_MAX_COST_USD`), not by restricting
  selection. Claude Fable 5 is wired into the extended-thinking family
  gate; the 1M long-context beta is deliberately NOT enabled for it until
  verified upstream (falls back to the standard window).

- Chat keyboard shortcuts no longer collide with core browser shortcuts. The
  bindings that shadowed browser features are gone or moved: ⌘/Ctrl+F is
  released back to find-in-page (⌘K remains the search binding), "new
  conversation" moves from ⌘/Ctrl+N (reserved in Chrome — uninterceptable, it
  opened a browser window) to **⌘/Ctrl+Shift+O**, and "focus the composer"
  moves from ⌘/Ctrl+J (browser downloads) to **Shift+Esc** — both matching
  ChatGPT's chords, the prevailing muscle memory for AI-chat apps. The
  bare-key set (?, j/k, Enter, p, a, r, #, y, e) is unchanged; the "?" help
  overlay reflects the new chords.
- `fleet update` systemd-unit drift check is now functional-diff-based and can
  adopt the shipped unit in one step. It compares `deploy/*.service` against the
  installed unit with comments and blank lines stripped, so a reworded header no
  longer raises a spurious "a unit fix may not be live" warning. On a real
  (functional) change an interactive run prints the diff and offers to adopt the
  shipped unit — installing it, creating the `fleet-web` user + chowning `.next`
  when the pre-#654 non-root migration requires it, and running `daemon-reload`
  before the step-5 restart — so operators no longer hand-run `install`/`useradd`/
  `daemon-reload` themselves. A new `--adopt-units` flag
  (`FLEET_UPDATE_ADOPT_UNITS=1`) adopts unattended; `--yes` alone never adopts a
  unit (it only skips the commit-range confirm), so an automated update can't
  silently clobber an operator's hand-edited unit. Declined/non-interactive runs
  still print the actionable manual hint.
- Web UI/UX pass over four surfaces, matching the unified-shell design
  (fleet-unified reference): rail typography aligned to the design's type ramp
  (brand name 0.95rem/700, group headers 0.8rem, conversation/folder/label
  rows 0.875rem, menu items and account email 0.82rem, filter chips 0.78rem);
  a click-toggled ⓘ explainer next to the Recent group header stating the
  retention default (unpinned chats delete after 14 days of inactivity —
  `CONVERSATION_TTL_DAYS` — pin to keep; dismisses on outside click and
  Escape); a new app-wide `--gradient-bg` page-background token painted once
  on `<body>` (cover, fixed) beneath now-transparent chat/orchestrator shells,
  with `--sidebar-surface` moved to the design's rail gradient; and the chat
  composer toolbar rebuilt to the design — model chip opening a listbox
  popover (search field on top preserves type-to-search + free slug entry,
  with arrow-key navigation, Esc close, and focus return), a toolbar divider,
  icon tool-buttons (persona · attach · tools with a corner count badge ·
  context ring), sectioned tools popover with mini-switch toggles populated
  from the live optional-MCP catalog, and the circular gradient send button.
  Composer popovers now all close on Escape and return focus to their
  trigger. Fixes a latent styling bug where gradient tokens consumed via
  bare arbitrary-value background classes compiled to `background-color`
  and never painted (now emitted as `background-image`).

### Added

- **User management in the web UI.** Settings → Admin "Users & roles" now
  covers the full lifecycle — create accounts (typed or generated password,
  shown once), reassign role/team, reset passwords (generated, shown once),
  and delete — so admins no longer need CLI access to the box. New chat-server
  endpoints `POST /admin/users`, `DELETE /admin/users/{email}`, and
  `PUT /admin/users/{email}/password` (admin-gated like the rest of
  `/admin/*`; every write is audit-logged with the acting admin). Role writes
  carry `fleet admin add`'s two-plane semantics: promoting to admin also
  ensures the Operations Center admin row, demoting or deleting removes it —
  via a new injected `WithOpsAdmins` seam (httpapi stays sched-agnostic), with
  the users list annotating who holds it (`ops_center_admin`, the "ops" badge).
  Self-demote and self-delete are refused so an admin can't lock themselves
  out mid-session. Web proxy routes are Origin-checked (CSRF) like the other
  mutating admin routes; passwords travel one way and never appear in
  responses or logs.
- **One-command admin provisioning + interactive bootstrap credentials.** New
  `fleet admin add <email>` provisions a FULL admin across both user planes in
  one idempotent step — the chat web login (password prompted hidden with
  double-entry, or `--password -`), the chat-admin role, and the Operations
  Center admin row the session-cookie bridge (#458) resolves by email — with
  `fleet admin list` (two-plane view) and `fleet admin rm` (removes from both).
  New `fleet config set-openrouter-key` (hidden prompt or `--key -`; upserts
  `OPENROUTER_API_KEY` into the server env file) and `fleet config
  set-auth-pubkey [<key>|--from <file>]` (enables Elcano SSO: accepts the
  `auth pubkey` output line verbatim, validates standard-base64 → 32-byte
  Ed25519 fail-closed, writes `AUTH_SIGNING_PUBKEY` — plus optional
  `--login-url`/`--cookie-domain` — into the web env file). `scripts/bootstrap.sh`
  is now fully interactive on a terminal: a bare run walks through the
  deployment choices (systemd service? web UI? public TLS domain?) and then the
  credentials — the OpenRouter key (hidden prompt when absent), the Elcano SSO
  key under the web tier (`--auth-pubkey <value|@file>` skips the prompt and is
  validated up-front, before the npm build), and admin registration once the
  service is active (`--admin a@x,b@y` or an interactive prompt, passwords via
  `fleet admin add`). Previously-set `AUTH_*` keys now carry across re-runs
  (the web env rewrite used to silently drop them — SSO turned off on every
  re-bootstrap). Explicit flags always win; non-TTY runs skip every prompt and
  keep the old flag/default behavior, printing the equivalent `fleet admin add`
  / `fleet config` follow-up commands instead.
- One-time legacy migration ingest ([docs/LEGACY-IMPORT.md](docs/LEGACY-IMPORT.md)):
  `fleet import <bundle.json>` consumes the versioned `fleet-migration-bundle`
  JSON envelope produced by the deprecated chat repo (`chat-admin export`: users
  with bcrypt hashes, conversations + full message history with pins and
  timestamps preserved, memories) and the deprecated moc repo (`moc
  -export-fleet`: sched users, scheduled/recurring tasks with next-run
  recomputed in the task's timezone, run logs). All legacy-schema knowledge
  lives in the exporters; fleet only consumes the bundle. Idempotent re-runs
  (`--dry-run`, `--live-only` supported); imported history is FTS-indexed.

- Per-task extended-thinking override (#220): a nullable `thinking_budget_tokens`
  on tasks (sched migration 053) lets a scheduled task set its own Claude
  thinking budget — NULL/omitted inherits the global
  `FLEET_DEFAULT_THINKING_BUDGET_TOKENS` (unchanged behavior), `0` forces
  thinking off, `N` sets its budget (clamped to the provider range at run
  time). Exposed on the create/PATCH task API, the `schedule_task` tool, the
  admin CLI import, and a "Thinking budget" field in the Operations Center
  task-create form; carried through task export/import. Resolves override >
  global, mirroring the interactive per-conversation override.


### Added

- Usage report CSV export ([docs/USAGE-ANALYTICS.md](docs/USAGE-ANALYTICS.md)):
  `GET /admin/usage?format=csv` returns the report as a `text/csv` attachment
  (row per bucket + a TOTAL row), and the Operations Center Usage panel gains a
  **Download CSV** button using the current filters.
- Optional paused-task auto-expire ([docs/ASK-NOTIFY.md](docs/ASK-NOTIFY.md)):
  `FLEET_PAUSED_TASK_EXPIRY_MINUTES` (>0) makes the scheduler fail a task that
  has sat in `paused_awaiting_input` past the window (terminal `error`),
  mirroring the anti-starvation sweep (#230). Default 0 = OFF (wait forever),
  so existing behavior is unchanged.


### Changed

- Skill `allowed-tools` is now parsed and **surfaced** for review — the skills
  library UI and `GET /skills` show each skill's declared tool contract (both
  the YAML-list and comma-string frontmatter forms are accepted). It remains
  **advisory, not enforced**: skills are read on-demand mid-turn with no single
  "active skill" to gate a tool roster against, and a skill can never exceed
  the turn's sandbox/MCP/approval limits, so a self-declared list can't be an
  authorization boundary. The "Honesty in docs" invariant (AGENTS.md) and
  docs/SKILLS.md now spell out why, plus the naming-portability caveat for
  skills imported from Claude Code.


### Fixed

- A bare deployment (no client-config bundle) could never start: two fresh-box
  bugs found by running the full interactive bootstrap end-to-end in a clean
  Fedora systemd container. (1) `deploy/fleet.service` listed
  `/opt/fleet/client` in `ReadWritePaths=` unconditionally — on a box using the
  in-repo `config/default` bundle that path doesn't exist, and systemd fails
  namespace setup outright (`status=226/NAMESPACE`); it is now `-`-prefixed
  (ignore-if-missing). (2) bootstrap wrote the default bundle dir into the env
  file as the RELATIVE `config/default`, which the unit resolves under its
  `WorkingDirectory=/var/lib/fleet` — "client config bundle … no such file or
  directory" at boot; `CLIENT_CONFIG_DIR` is now absolutized (repo-anchored
  default, CWD-resolved `--client-config` paths) before it is persisted.
- Admin Health panel: the MCP catalog pills no longer render optional servers in
  the red **danger** tone, which read as "broken". The panel does not probe MCP
  servers — the flag is "enabled by default for new conversations" — so an
  off-by-default optional server is normal, not an error. On-by-default servers
  are now `success` (green) and optional servers `neutral` (grey), never danger;
  the header is relabeled "MCP catalog" with a tooltip clarifying it isn't a
  health probe, and each pill has an explanatory title.

- `fleet update` now pulls the client-config bundle even when the update changes
  `update.sh` itself. The self-update re-exec ran the new script with
  `--no-pull` to avoid re-fetching the already-fast-forwarded fleet checkout,
  but `--no-pull` also skips the client-bundle pull (step 2, which hadn't run
  yet) — so any release that touched `update.sh` left the bundle stale. The
  re-exec now uses an internal `FLEET_UPDATE_REEXEC` marker that skips only the
  SRC fetch + self-update detection (no loop) and otherwise runs a normal update,
  including the bundle pull and the Containerfile-hash sandbox-rebuild gate. Also
  made the unit-drift adopt hint accurate for `fleet-web.service`: it now shows
  the one-time `useradd fleet-web` + `.next` chown prerequisite (the unit runs as
  a non-root user since the deploy-hardening change) and a `systemctl restart`, so
  following it doesn't leave the web tier failing to start.

- Paused-task expiry no longer kills a recurring task's schedule. The
  `FLEET_PAUSED_TASK_EXPIRY_MINUTES` sweep failed an unattended ask-paused task
  with a bare bulk UPDATE that bypassed the terminal-transition path — so an
  expired occurrence of a recurring task went `error` AND permanently ended the
  schedule (no next occurrence). The sweep now uses `UPDATE ... RETURNING` and
  spawns the next occurrence for each expired recurring row (race-free against a
  concurrent resume, which is status-guarded). Opt-in feature, default off. The
  sweep still does not fire the runner's completion notification (the notifier
  lives in the runner) — documented as a known gap.

- The anti-starvation sweep (`FLEET_STARVATION_*`) measured wait from
  `created_at`, but a recurring occurrence's row is created at the previous
  occurrence's completion — so its `created_at` is ~one period old the moment it
  becomes pending, and enabling the window floor-promoted every recurring/retried
  task instantly, inverting the priority queue. It now keys on
  `GREATEST(created_at, scheduled_for)` (the eligibility time); a genuinely
  starving task is still promoted, a freshly-eligible one is not. Opt-in feature,
  default off; no migration.

- Recurring tasks created by an agent or through the chat approval card no
  longer drift to UTC. `EnqueueTask` (the `create_task` tool, the chat
  `schedule_task` approval, promote-to-task) evaluated the first cron fire in
  the server-clock zone and never persisted a per-task timezone, so
  `FLEET_DEFAULT_TIMEZONE` was ignored and every occurrence after the first
  fired in UTC (a "9am" task became 9am UTC). It now resolves and persists the
  per-task timezone exactly like the HTTP create path, so recurrence stays in
  the task's zone. HTTP-created tasks were already correct.

- `fleet update` / `scripts/fleet-upgrade.sh` now actually install the freshly
  built binaries on the standard `/opt/fleet` topology. Both scripts parsed
  `systemctl show -p ExecStart --value` with `awk '{print $1}'`, which grabs
  the literal `{` of systemd's exec-command struct rather than the binary
  path — `update.sh` then resolved the install dir to the source checkout and
  skipped the copy as "in place" (the restart re-ran the OLD binary while
  reporting success), and `fleet-upgrade.sh` resolved it to the operator's
  cwd, voiding the backup/rollback guarantee. The `path=` field is now
  extracted explicitly. `fleet update` also warns when the installed systemd
  units have drifted from the shipped `deploy/*.service` (bootstrap installs
  units only when absent, so shipped unit fixes never reached existing boxes
  and were previously invisible).

- Removed accidentally committed local agent-run artifacts (`hello.txt`,
  `audit_log.txt`, `data/audit/bash.log`, `data/task-run-*/session.json`) and
  added the server runtime `data/` directory to `.gitignore` so runtime output
  can no longer land in the repository.


### Security

- Host-side file tools (`view_file`/`write_file`/`edit_file`) are now confined to
  the workspace root the sandbox bind-mounts, closing an absolute-path bypass.
  Their allowlist (`AllowedBaseDirs`) fell back to the process working directory,
  which under systemd is the whole StateDirectory (`/var/lib/fleet`) — so an
  absolute path could read or write `data/` attachments/uploads and
  `api_keys.json` that the sandbox never mounts, defeating the "sandbox is
  mandatory" invariant for file I/O. `cmd/fleet` now registers the workspace root
  (`tools.SetWorkspaceRoot`, resolved exactly as the agent manager resolves it)
  as the authoritative base; the process cwd is no longer blessed. `ValidatePath`
  already resolves symlinks and re-checks the real path, so a symlink planted in
  the workspace pointing at `data/` is rejected too. `os.TempDir()` and
  operator-opted `FLEET_ALLOWED_DIRS` remain allowed; unregistered (tests/CLI)
  keeps the legacy cwd allowlist.

- Orchestrator admin auth now fails closed when `ADMIN_API_KEY` is unset.
  `verifyAdminKey` compared `sha256(header)` against `sha256(configured key)`
  in constant time, but with the key unset `sha256("") == sha256("")`, so a
  request sending no `X-API-Key` header authenticated as admin — silently
  opening the entire admin surface (`/keys`, `/users`, `/tasks/cleanup`,
  config/MCP reload, the principal `isAdmin` flag) on any deploy that left the
  key unset. Now returns false on an empty configured key, closing all call
  sites at once, and the process logs a startup warning when it's unset.
  `bootstrap.sh` always sets the key, so bootstrapped deploys are unaffected.

- The orchestrator listener now defaults to `127.0.0.1:8000` (loopback) instead
  of `:8000` (all interfaces), matching the chat listener's loopback default
  and what the deploy docs (Caddyfile, fleet.service, DEPLOYMENT.md) already
  promised. On a host without a firewall, the old default exposed the
  orchestrator/admin API directly to the network. Multi-host topologies that
  relied on the wide bind must now set `FLEET_ORCHESTRATOR_ADDR` explicitly
  (e.g. `0.0.0.0:8000`). The chat listener's last-resort fallback (used only
  when `FLEET_SERVER_ADDR` is set but empty) is loopback now too.

- The public-facing web tier (`deploy/fleet-web.service`) now runs as a
  dedicated unprivileged `fleet-web` system user instead of root; bootstrap
  creates the user and hands it `.next/` (Next's runtime cache — the unit's
  only writable path), and `fleet update` re-chowns it after each deploy.
  Existing installs keep their current unit (bootstrap never overwrites);
  `fleet update` now surfaces the drift with the exact adopt command.

- The TLS edge (`deploy/Caddyfile` and the bootstrap-generated variant) now
  sends `Strict-Transport-Security`, `X-Content-Type-Options: nosniff`, and
  `X-Frame-Options: DENY` on every response, mirroring the values the Go
  backends already set (`securityHeadersMiddleware`) so the whole origin
  carries one header policy.

- Interactive chat now honors the fleet-wide sandbox egress mode
  (`FLEET_DEFAULT_NETWORK_MODE`), closing a gap ADR-0012 deferred: a
  non-lockdown chat turn used to get open network egress even under
  `allowlisted` or `lockdown`. `takeTurnSandbox` now seals (`lockdown`) or
  proxy-filters (`allowlisted`) chat turns exactly as scheduled tasks do; a
  containing mode takes precedence over the persistent Python REPL. `open`
  (the default) is unchanged. ADR-0031.

### Added

- Rampart ML engine for PII redaction
  ([docs/PII-REDACTION.md](docs/PII-REDACTION.md)): the #450 "interface-ready
  follow-on" is now shipped — `pii_redaction_engine` picks `pattern` (the
  built-in regexes) or `rampart`, the 17-entity-type MiniLM ONNX classifier
  (names, addresses, government IDs, bank numbers, …) running behind an
  operator-deployed HTTP service (`pii_rampart_url`;
  [`scripts/rampart-service`](scripts/rampart-service/README.md) is the
  reference implementation over the official npm runtime, ~25 ms/call on
  CPU). Rampart redacts with stable numbered placeholders
  (`[GIVEN_NAME_1]`), the deterministic engine sweeps its output as a second
  pass (strict superset of the pattern floor — a missed formatted phone
  number is still caught), and a service outage falls back to the pattern
  engine, never fail-open. The Features panel gains the engine picker, the
  service URL field, a **Test detection** button
  (`POST /admin/pii-redaction/test`) that runs the live redactor over a
  synthetic sample, and a **one-click Install Rampart service** button
  (`/admin/pii-redaction/install`) — fleet builds the service container (model
  baked in, reference service embedded in the binary), runs it on loopback via
  the rootless podman it already uses for the sandbox, health-checks it, fills
  in the service URL, and re-starts it after a reboot. No bootstrap/update
  changes are needed to use it; operators who prefer their own systemd unit
  can instead run `scripts/rampart-service/install.sh`.
- Admin-managed task notifications
  ([docs/NOTIFICATIONS.md](docs/NOTIFICATIONS.md)): Settings → Admin gains a
  "Notifications" panel — configure the email (SMTP) and signed-webhook
  channels and the status filter at runtime, with a **Send test** button per
  channel (one real delivery of a synthetic event, key-free result). The
  config lives in the chat DB (`notify_settings`, migration 036) with the
  SMTP password and webhook signing secret sealed under the store's secretbox
  cipher and **write-only** semantics; a save hot-swaps the running
  notifier's config (`notify.SetConfig` — one shared pointer serves the
  runner, budget alerts, and email reply-back), so the next task completion
  uses it with no restart. Env vars remain the deployment default; the saved
  config replaces them wholesale and "Use env config" reverts. Email
  reply-back (#511) enablement is now consulted live, so enabling SMTP from
  the panel activates it without a restart. The store secret cipher now
  installs whenever `FLEET_MCP_OAUTH_ENCRYPTION_KEY` is set (previously only
  with the remote-MCP feature's public base URL also configured).
- Admin-managed workspace feature settings
  ([docs/ADMIN-SETTINGS.md](docs/ADMIN-SETTINGS.md)): Settings → Admin gains a
  "Feature settings" panel — the env-flag-only product toggles (PII redaction
  mode, tool disclosure threshold, tool-output ceiling, phone-a-friend,
  sub-agent delegation, memory auto-index, task error analysis, auto-title,
  connector recommendations, context handles) are now visible and editable
  from the web UI. Overrides live in the chat DB (`workspace_settings`,
  migration 035) with precedence **admin override > env var > built-in
  default**; every change is validated against a typed server-side registry,
  audit-logged, applied **live** (next turn/run/tool call — the registry only
  admits restart-free settings), and reversible per-setting via Reset. Env
  vars keep working unchanged as the deployment defaults. The registry holds
  no secrets by construction (SMTP/webhook notification config stays env-only
  pending sealed-secret treatment). ADR:
  [docs/adr/0030-admin-workspace-settings.md](docs/adr/0030-admin-workspace-settings.md).

- Admin-managed LLM providers ([docs/LLM-PROVIDERS.md](docs/LLM-PROVIDERS.md)):
  Settings → Admin gains a "Model providers" panel — add an OpenRouter,
  Anthropic, or OpenAI API key, or any OpenAI-compatible endpoint (Ollama,
  vLLM…), with several providers routing simultaneously. Rows live in the chat
  DB (`llm_providers`, migration 034) with keys sealed under the store's
  secretbox cipher and **write-only** semantics (no read path ever returns a
  key). Edits rebuild + hot-swap the model resolver — no restart — and the
  admin rows overlay the client bundle's `providers:` table (same-name rows
  replace, new names append). Enabled providers' listed models appear in the
  shared model picker as `provider/model` entries with a "Workspace" badge.
  With no rows configured, behavior is byte-identical to before (single
  catch-all OpenRouter from `OPENROUTER_API_KEY`), keeping the fake-LLM seam
  and existing deployments unchanged. Each row has a **Test** button — one
  host-side probe against the provider's real endpoint that verifies the
  key/base URL and cross-checks the listed models against the endpoint's
  catalog (absences warn, never fail); works on disabled rows so admins can
  verify before enabling.
- Skills builder (docs/SKILLS.md phases 2 + 3): Settings → Skills gains a
  "Your skills" section — create, edit, enable/disable, and delete personal
  skills (name + description + markdown instructions). User skills are
  DB-owned (`user_skills`, migration 033), strictly per-user, materialized
  into the conversation workspace before each chat turn
  (`user-skills/<name>/SKILL.md`) and listed in the prompt roster; `/name`
  invocation matches them after bundle/built-in names. Scheduled tasks load
  them too: the runner resolves the task owner and inlines their active
  skills into the run's prompt (24KB budget, loud truncation). The agent can
  draft skills from a run via the new `propose_skill` tool (both modes,
  intercepted at the policy boundary like `propose_note`); proposals stage as
  inert "Proposed by agent" entries the owner approves or deletes on the
  Skills page.
- Always-on bundled connectors now render as visible-but-locked rows on the
  connections page — nothing is invisibly enabled.
- New live-lane Playwright coverage drives the skills builder CRUD, the
  connector directory search, and the availability toggles against the real
  stack (real Postgres, real server, real prefs rows).
- Built-in Agent Skills pack + skills library: fleet embeds five
  generally-useful skills (data-profiler, web-research-brief,
  code-review-checklist, release-notes, executive-report) that every bundle
  inherits by default via a merged on-disk skills dir (bundle skills win name
  collisions; `skills_builtin: false` / `skills_hidden: [...]` manifest knobs
  opt out or trim). New Settings → Skills library page (account menu) with
  search, Workspace/Built-in provenance badges, and a full SKILL.md read view
  (`GET /skills/{name}`). Workspace symlinks now point at the registered
  bundle dirs (`tools.SetSupportingDocDirs`) so merged skills resolve in every
  sandboxed run. See `docs/SKILLS.md`.
- Unified connector enablement: Settings → Connections is now the per-user
  AVAILABILITY layer for every connector. Bundled (sandboxed) connectors get
  an "Enabled for me" toggle and a default credential-account seat picker;
  own and shared remote connections get personal On/Off ("Off for me" on a
  shared connection never affects the owner or other grantees). Chat pickers
  and turns honor the prefs (disabled connectors disappear; the default seat
  rides into the turn as the MCP account) while scheduled tasks deliberately
  keep their own pinned per-task `{server, account}` selection + credential
  allowlist — supervised chat follows your defaults, unsupervised automation
  never drifts with them. New `user_connector_prefs` (migration 032) +
  `GET/PUT/DELETE /connector-prefs`; absence of a pref row means the operator
  default, so nothing changes until a user opts. See `docs/CONNECTOR-PREFS.md`.
- Remote MCP connection sharing: the owner of a connected hosted MCP server
  can share it with named users or with everyone on the box (`remote_mcp_shares`,
  migration 031). Grantees' chats and scheduled tasks mount the server's tools;
  tool calls authenticate with the OWNER's OAuth token host-side (the token
  never leaves the host and the grantee never sees it), shared runs are logged
  with owner attribution, and revoking a grant (or deleting the server) takes
  effect on the next turn. New API: `shares` + `shared_with_me` on
  `GET /remote-mcp-servers`, `POST/DELETE /remote-mcp-servers/{id}/shares[/…]`;
  the connections page gains a Share panel per owned server and a "Shared with
  you" section. See `docs/CONNECTION-SHARING.md`.
- Built-in MCP connector directory: fleet now embeds a curated directory of
  ~290 vendor-hosted MCP servers (`internal/clientconfig/builtin_remote_catalog.yaml`)
  spanning 19 categories, inherited by every client bundle by default — the
  directory is a listing only; nothing connects until a user explicitly adds a
  server and completes the per-user OAuth flow (#443). Catalog entries gain
  `category`, `tags`, `provenance` (`official` | `third_party` | `community`),
  `repo_url`, and `auth` metadata; the built-in file requires all of them
  explicitly (CI-enforced) while bundle entries keep back-compat defaults. New
  manifest knobs: `remote_mcp_catalog_builtin: false` (opt out),
  `remote_mcp_catalog_community: true` (opt into community-hosted entries,
  off by default), and `remote_mcp_catalog_hidden: [...]` (tombstone
  individual entries — also the between-release kill switch for a dead or
  compromised endpoint). Bundle entries override built-ins by name; an
  `mcp_servers` name collision shadows the built-in entry with a loud log
  line instead of failing the load. Settings → Connections becomes a
  searchable, category-grouped connector directory with provenance badges,
  docs/source vet-links, auth hints, and an explicit operator-named consent
  step before adding any non-official endpoint. Every shipped URL was
  verified against the vendor's own documentation. See `docs/MCP-CATALOG.md`.

### Changed

- Unified settings UX: Connections, Skills, the new General settings page
  (concurrency cap + MCP credential accounts, formerly the Operations Center's
  Settings modal — now retired), and Admin all render inside one shared
  settings shell with a section nav, an Operations Center cross-link, and a
  "Back to chat" button. The account menu offers Settings · Connections ·
  Skills · Admin identically on both surfaces.
- Web UI polish — rail, composer, motion, responsive (design-handoff parity):
  the rail collapses to a 4.25rem icon strip (toggle in the brand row,
  preference persisted to `localStorage["rail-collapsed"]`, avatar-only
  account menu, collapse closes menus and exits select mode); at ≤900px it
  auto-collapses and expands as an overlay drawer with a scrim, while the
  <640px hamburger drawer is unchanged. Multi-select is redesigned per the
  handoff: a row kebab's "Select…" is the only way into select mode
  (checkboxes are no longer hover-revealed), with a compact icon bulk bar at
  the rail's foot (move-to-folder / pin / add-label / delete / cancel,
  tooltips, disabled at zero selected, popovers with inline folder/label
  creation, Esc exits). The composer container matches the design's geometry
  (53rem column, `--radius-xl`, `--shadow-md`, a focus-fading keyboard hint
  below the bar) — the toolbar keeps its existing control set — and gains a
  sealed variant (accent border + explainer strip + sandbox placeholder) on
  lockdown conversations. The icon-only "new sealed chat" button explains
  what a sealed chat is on hover and keyboard focus. The
  account-menu theme segment is now System | Light | Dark — System follows
  the OS live and clears the stored preference. All transitions/animations
  ride the shared `--motion-fast`/`--motion-base` tokens, every popover and
  modal enters with one fade+rise, reduced-motion handling is consolidated
  into the single global block, and small controls (kebab, chip removers,
  search clear, modal close) get ~2rem hit areas with no visual change.

### Fixed

- Removed the Settings → General concurrency-cap card: it called
  `GET/PUT /concurrency`, endpoints fleet's orchestrator never served (a
  moc-era leftover), so it could only ever surface an error toast. The cap
  remains `FLEET_MAX_CONCURRENT_AGENTS` in the env file (boot-bound — it
  sizes the admission semaphore and sandbox warm pool at startup); see
  [docs/ADMIN-SETTINGS.md](docs/ADMIN-SETTINGS.md) for which settings are
  live-tunable instead.
- `fleet update` now actually ships web changes: `scripts/update.sh` deploys
  the freshly built Next app into the `fleet-web` unit's WorkingDirectory
  (`/opt/fleet/web`) and restarts `fleet-web` after the backend restart —
  previously the build stayed in the source checkout and the live site kept
  serving the old bundle. The rebuild also re-applies the `NEXT_PUBLIC_*`
  origin/app-name stamps bootstrap baked in (grepped from
  `/etc/fleet/fleet-web.env`; the 0600 file is still never sourced).
- Runner terminal side effects gated on the DB write (#580): the success
  notification and email reply fired even when the terminal DB write failed,
  producing spurious "success" and duplicate emails on retry; all terminal
  side effects now wait for the write to land.
- Stale-lease fencing on runner cleanup (#581): the worker cleanup deleted the
  active-map entry by task ID without a token guard, letting a stale goroutine
  clobber a fresh re-claim; the delete is now token-guarded.
- Resumed task keeps the human's answer (#582): a paused-awaiting-human task
  that failed transiently and retried lost the answer because `ClearPendingQA`
  ran before the run; it now clears only at the terminal transition, so the
  answer survives a retry.
- Paused dataset rows resume instead of failing (#586): pausing a running
  dataset marked in-flight rows permanently `failed` and resume never retried
  them; interrupted rows are now requeued to `pending` with the attempt refunded.
- Rune-safe truncation (#595): byte-boundary truncation emitted invalid UTF-8
  (a dataset row could stick in `running`; prompt/log corruption). A shared
  `internal/truncate.Clamp` cuts on rune boundaries and is used by datasets,
  the prior-run handoff, and evals.
- Sub-agent budget over-grant (#588): a release-before-charge lock ordering let
  concurrent sub-agent spawns over-reserve against the parent budget; the
  reservation and charge now settle atomically under one lock.
- Cached-token accounting (#587): context-pressure and child-budget math
  double-counted cached tokens on OpenRouter, so the token ceiling could fire
  late and cache hit-rate could exceed 100%; all usage accumulators are
  normalized onto one include-cached convention.
- Runaway-compaction cap made reachable (#598): `maxConsecutiveCompactions`
  (and `ErrContextBudgetExhausted`) was dead code; the counter now resets only
  on a round that completes without force-compaction, so a genuine
  compaction storm is refused.
- BranchConversation range check + atomicity (#578, #597): an out-of-range
  `branch_point_message_id` silently copied the entire conversation (now errors),
  and the INSERT + history copy now run in one transaction instead of two
  statements.
- AdminStats excludes soft-deleted conversations (#579), and the conversation
  mutators (`SetModel`, `SetPinned`, title/rename, archive, optional-MCP,
  approval-timeout) now exclude soft-deleted rows (#596), matching the guarded
  `SetShareToken`/`SetThinkingConfig`.
- bridgeStdout data race (#583): the container/host `runPython` reader goroutine
  raced bridge teardown on context cancellation; the reader now snapshots the
  bridge handle under the mutex.
- ConcurrencyLimiter eviction (#594): the per-key map never evicted released
  keys (a slow unbounded leak on a long-lived process); a key is now removed the
  moment its in-flight count hits zero.
- Migration DDL linter (#593): the linter missed the COLUMN-less forms of
  `ADD ... NOT NULL` / `DROP ...` it claims to reject; both spellings are now
  caught, and the script gained its first test harness.
- Orchestrator live SSE parser (#589): the orchestrator parser diverged from the
  hardened chat parser (CRLF broke it, multi-line `data:` corrupted); it now
  reuses `parseSseChunk`, so both consumers share one hardened parser.
- OpenAPI + docs honesty (#591, #592, #599): the OpenAPI spec now matches what
  the handlers emit (`GET /api/me` body, `POST /users` 201), the README no
  longer claims the OpenAPI CI test skips body-schema gating (it doesn't), and
  the critical-action cap comment now describes its real failed-attempt-count
  semantics.
- Tool-output ceiling on direct MCP calls (#576): MCP tools registered directly
  (roster below the #506 tool-disclosure threshold — the common case) applied
  redaction and PII gating but not the #199 output ceiling, so one oversized
  connector result could enter the transcript untruncated and overflow the
  context window. `mcpTool.Run` now caps output exactly like the wrapped
  (deferred `tool_call`) path — identical truncation above and below the
  threshold, error results included — and the `tool_output_limit.go` comment no
  longer overstates `policyGuardedTool.Run` as the universal choke point.
- Scheduler liveness (#566): `ProcessScheduledTasks` no longer live-locks when
  a full batch of due tasks makes no forward progress — e.g. ≥1000 one-shot
  tasks all soft-held by a declining `run_if` gate. The old loop paged with a
  plain `LIMIT` and terminated only on a short batch, so a full batch of
  soft-held rows (which stay scheduled + due by design) was re-fetched
  identically forever, hanging the scheduler goroutine and with it lease
  recovery and starvation promotion for the whole box. The due set is now
  walked with a keyset cursor over the total order `(scheduled_for, id)` — each
  row is handled at most once per tick, a held prefix can't mask due work
  behind it (the `id` tiebreaker makes pages stable even when many rows share
  one `scheduled_for`), per-tick cost is linear, and a defensive 100k-rows valve
  bounds a pathological tick. Soft-hold semantics are unchanged: a held one-shot
  stays scheduled and is re-evaluated next tick.
- Web rate limiter (#561): `(*rateLimiter).wait` no longer double-unlocks its
  mutex when the context is cancelled mid-wait. Cancelling a `web_fetch` /
  `web_search` turn while it was blocked in the minimum-interval (or per-minute)
  wait triggered `fatal error: sync: unlock of unlocked mutex`, which `recover()`
  cannot catch and which crashed the whole `fleet` process — killing every
  user's in-flight session. Both waits are now context-aware and release the
  lock exactly once on every path.
- Recurring tasks (#565): the next occurrence of a recurring task now preserves
  every definition field. The recurrence path built each new occurrence from a
  hand-maintained `TaskCreate` literal that omitted many fields, so occurrence
  #2+ silently reset `allow_network`, `carry_context`, `output_schema`,
  `sandbox_limits`, the delegation / task-creation / event-trigger capability
  bits, `persona`, and the SLA config to their zero values — e.g. a daily
  `allow_network:true` task had its sandbox resealed `--network=none` from the
  second run on and failed silently. The path now delegates to the canonical
  `TaskToCreate` clone (also used by re-run/clone #270), and `TaskToCreate` was
  itself completed to carry `persona` and `carry_context`, which it had been
  dropping — so re-run/clone stops losing them too. A reflection guard
  (`TestTaskToCreateCarriesEveryDefinitionField`) now fails if a new `TaskCreate`
  definition field is added without being carried. Network posture is preserved,
  never widened.
- Structured output extraction (#244 hardening): a final answer carrying
  SEVERAL top-level JSON values (a narrated intermediate plus the restated
  final object — observed in a live run) now validates to the last conforming
  value instead of failing outright; extraction scans complete JSON values
  with a decoder, so braces inside strings can't derail it.
- `fleet chat` no longer races the terminal on its first markdown render:
  glamour's auto-style raw-queried the terminal (OSC 11) mid-session while
  bubbletea owned stdin, so the reply could wedge input or be typed into the
  composer as garbage on real terminals. The TUI now resolves dark/light via
  bubbletea's RequestBackgroundColor handshake and always hands glamour an
  explicit style. Also: typed letters no longer scroll the transcript (the
  viewport's default h/j/k/l keymap was receiving composer keystrokes).
- API keys minted by the CLI (`fleet sched apikey create`) now authenticate
  against an already-running server without a restart: on a lookup miss the
  key manager stats `api_keys.json` and reloads newly-appended keys (existing
  in-memory keys — and their runtime rate/budget state — are never clobbered,
  and a just-revoked key can't be resurrected). Found by exercising the
  documented mint-and-use flow live.

### Added

- Usage analytics (#601 part 1): admin-only `GET /admin/usage?group_by=&from=&to=`
  rolls the already-persisted metering (per-iteration `task_iterations` ⋈
  `tasks`, plus the chat `turn_metrics` session log) up by user, API key,
  project, model, or day/week time bucket over a requested window — a pure
  read model: no new accounting path, no new tables. Rendered in the
  Operations Center as an admin-only "Usage" tab (KPI tiles, single-hue
  bar/column charts coherent in light + dark, full table view). Honest scope
  (#289): native-provider runs accrue $0 unless a pricing override is
  configured, so the endpoint and panel always show token totals alongside
  dollars and say so. Per-principal budgets are part 2 of #601 and are not
  included here. See `docs/USAGE-ANALYTICS.md`.
- Per-principal rolling budgets with alerts (#601 part 2): a budget is
  `{scope: user|key|project, principal, window: day|week|month, soft/hard
  bounds in dollars AND tokens}` (one new sched table, migration 052),
  enforced at task-create by ONE shared gate across every create path —
  `POST /tasks`, `POST /tasks/batch`, and the chat `schedule_task` approval
  seam. Spend is recomputed per check from the part-1 usage read model (no
  second accounting path). At a soft bound exactly one notify alert fires per
  window crossing (persisted marker — restart-safe, concurrency-safe) through
  the existing email/webhook/Web Push pipeline; at a hard bound new task
  creation is refused with 402 + `Retry-After` until the window rolls over.
  Fail-safe composition with #286: effective hard bounds are clamped to the
  LIVE global `FLEET_MAX_COST_USD` / `FLEET_MAX_TOTAL_TOKENS` — budgets only
  narrow, never widen, and no budget configured is byte-for-byte today's
  behavior. Admin CRUD at `GET/POST /admin/budgets` +
  `DELETE /admin/budgets/{id}` (in `docs/openapi.yaml`, parity-tested); the
  Operations Center Usage panel lists configured budgets read-only with live
  window spend. Honest scope: `scope=project` is stored/reported but not yet
  enforced (no create path carries a project); admin-key submissions and
  in-process spawn/rerun paths are not gated. See `docs/USAGE-ANALYTICS.md`.
- `fleet task run <task.yaml>` — the local one-shot harness (run a single task
  to completion through the governed scheduled runtime, no server/DB) is now a
  verb of the unified CLI instead of the separate `cutlass` binary; the logic
  moved to `internal/taskrun` and `cmd/cutlass` remains as a warning shim for
  one deprecation release (the fleet-admin pattern). The example task file is
  now `docs/examples/local-task.yaml`, and the Charmbracelet acknowledgement
  credits the full charm stack behind the TUI (Bubble Tea, Bubbles, Lip Gloss,
  Glamour, vhs, freeze) alongside Fantasy.
- Tool-pipeline metrics endpoint (#543): admin-only
  `GET /admin/pipeline-metrics` derives per-run tool turns, total/distinct
  tool calls, tokens, cost, and wall clock from the session logs fleet already
  persists (no new columns — works retroactively on every retained run), plus
  a fleet-wide aggregate with a tool-turn histogram and the share of runs that
  were long multi-tool pipelines. This is the sensor behind #505's reopen
  criteria: optimization decisions become a threshold check, not a vibe.
- Demo GIFs for every surface (#540): the README now opens with three
  recordings telling one story — plan in chat (a REAL model + sandbox take),
  automate in the Operations Center (real scheduler), ride along in the TUI
  (deterministic mock). Recording + conversion pipelines are scripted
  (`docs/scripts/record-web-demos.mjs`, `generate-web-gifs.sh`,
  `generate-tui-gif.sh` with a verified-take retry loop) and documented in
  `docs/generating-demo-gif.md`.

- `fleet cleanup [--dry-run] [--deep]` — reclaim host-side build/deploy cruft:
  dangling podman image layers (each sandbox rebuild strands ~1.3 GB) and the
  Go build/test caches; `--deep` additionally prunes unused named images,
  stopped containers, and networks. Never touches databases, workspaces, or
  the client bundle. `scripts/update.sh` now prunes dangling layers
  automatically after a successful sandbox rebuild, so routine updates can't
  fill the disk. CLI usage strings now name the unified `fleet` binary
  instead of the deprecated `fleet-admin`.
- Browser push notifications via the Web Push API (#292): opt in per browser
  under Settings → Connections and get a low-detail alert — task complete or
  failed (`FLEET_PUSH_ON_TASK_COMPLETE`), approval needed
  (`FLEET_PUSH_ON_APPROVAL_REQUEST`), or a paused task waiting for an answer —
  even with the fleet tab backgrounded. Web Push rides the existing
  `internal/notify` fan-out as a per-user backend (a new `Event.Audience`
  routes to the task owner's subscriptions). Setup: `fleet generate-vapid-keys`
  → set `FLEET_VAPID_PUBLIC_KEY` / `FLEET_VAPID_PRIVATE_KEY` /
  `FLEET_VAPID_CONTACT` (private key stays host-side); unconfigured
  deployments are byte-for-byte unchanged (`/push/*` answers 501). Payloads
  carry only a title, state, and deep link — never message content, tool args,
  or secrets. See `docs/PUSH-NOTIFICATIONS.md`.
- Temporal knowledge-graph memory (#523, stage 3 of #515): derived
  entity/relation tables over the memories table (chat migration 030) with
  provenance links back to each source memory and NO time columns of their
  own — all temporal semantics derive through the memories join (ADR-0029).
  As-of queries over both bi-temporal axes (`GET /memories/graph` and
  `GET /memories` with `as_of_valid=`/`as_of_learned=`), LLM extraction of
  entities + triples when a memory becomes active (gated by
  `FLEET_MEMORY_GRAPH_ENABLED`, default OFF; model via
  `FLEET_MEMORY_GRAPH_MODEL`; async + best-effort, plus a manual
  `POST /memories/{id}/extract-graph`), and a "Graph" tab in the memory
  manager with "what was true on…" / "what did fleet know on…" time-travel
  inputs. Deterministic auto-conflict rules stay deferred per the issue's
  triage; conflict handling remains human-confirmed supersession.
- `fleet chat` TUI polish + an animated README demo (#540): speaker pills,
  ✓/✗ tool-outcome glyphs, a full-width header bar (model · conversation), a
  rounded composer box that dims while streaming, a right-aligned hint bar, a
  formatted `/help`, and Esc-to-cancel. The README now embeds a deterministic
  demo GIF recorded with charm's vhs against a canned mock server (no model,
  no keys) — `docs/generating-demo-gif.md` documents the pipeline. Also new:
  `docs/BUILDING-ON-FLEET.md` (the HTTP API as an automation substrate) and a
  README "Batteries included" tour.
- Reusable sandbox-image publish workflow (#195):
  `.github/workflows/publish-sandbox-image.yml` (`workflow_call`) lets a client
  config repo build its bundle's sandbox image in CI with the canonical
  `scripts/build-sandbox-image.sh`, push immutable `{git-sha}` + `:latest` tags
  to GHCR, and auto-open a PR pinning `sandbox.image` in its manifest — so
  deploys `podman pull` a prebuilt image instead of rebuilding ~1.3 GB on-box.
  Core CI still builds the sandbox locally (the 24ce69f decoupling stands).
- Trust-labeled MCP connector catalog (#538): a bundle can curate a directory
  of official third-party hosted MCP servers (`remote_mcp_catalog:` in
  `manifest.yaml`, validated at load), served alongside the bundled
  Optional-server catalog by the new `GET /mcp-catalog` and rendered on
  Settings → Connections with explicit "Bundled" vs "Third-party" trust badges
  and one-click add into the per-user remote-MCP OAuth flow (#443). The generic
  bundle ships a starter directory of official vendor endpoints. See
  `docs/MCP-CATALOG.md`.
- Skills, phase 1 of first-class skills (#513): `GET /skills` returns the
  client bundle's Agent Skill roster (name + description per skill), and a chat
  message starting with `/<skill-name>` (exact, case-sensitive match — unknown
  `/tokens` like paths are silently ignored) explicitly invokes that skill by
  appending an instruction block to the persisted user message, so the
  transcript records which skill was loaded. The web composer gains a `/`
  autocomplete popover over the roster (prefix filter, ↑/↓ + Enter/Tab to
  complete, Esc to dismiss) backed by a new `/api/skills` proxy. In-app
  authoring, save-from-run (DB-staged proposals with approval + operator export
  to a bundle), and project scoping are deferred to phases 2/3 — see
  `docs/SKILLS.md`.
- `fleet chat` — a terminal UI for chatting with the fleet agent (Bubble Tea /
  Lipgloss, glamour-rendered Markdown, streaming replies, tool-call + reasoning
  display, `/new` `/retry` `/model` `/reasoning` `/clear` `/quit`, Ctrl+C to
  cancel a turn). It is a thin SSE client of the running server's `POST /chat`,
  so every turn still flows through the one governed run loop, the sandbox, and
  host-side credential brokering — the TUI only renders. `fleet chat --message
  "<text>"` (or `--no-tui`) runs one turn non-interactively to stdout for
  scripts/pipes. Connection identity resolves from `--email`/`--server`/`--token-file`
  → `$FLEET_USER_EMAIL` / `$FLEET_SERVER_TOKEN`; the shared token is never logged.
- Unified the operator CLI into one `fleet` binary (`fleet serve` runs the
  server — bare `fleet` also serves, for back-compat — and `fleet <verb>` is every
  operator command); a new `make install` puts `fleet` on PATH; `fleet update
  --check` is a read-only "commits behind" report; bootstrap installs a login
  MOTD banner. The old `fleet-admin <verb>` still works for one deprecation
  release (it prints a warning and forwards to the same dispatch) and is then
  removed.
- Top-level `VERSION` file as the single source of truth for the release number,
  stamped into the `fleet` and `fleet-admin` binaries at build time via
  `-ldflags -X` (`internal/version`). `fleet version` / `fleet-admin version`
  (also `--version`) print the version plus the VCS revision; the chat health
  summary and the orchestrator `/health` + `/api/config` endpoints report the
  same string. Builds without the ldflag (a bare `go build`) fall back to a
  `dev` sentinel and the VCS revision recovered from the Go build info.
- This `CHANGELOG.md`, in Keep a Changelog format.

### Changed

- Web UI aesthetic unity (#540): one token-driven design system across
  `/chat`, `/orchestrator`, `/admin`, `/settings`, and `/login`. Recurring
  hardcoded status colors (the chip/banner greens, ambers, and reds) became
  semantic theme-aware tokens (`--color-{success,warning,danger}-strong/-soft`,
  defined for dark *and* light with AA-contrast light values), and the
  hand-rolled status pills and notice banners were consolidated into shared
  primitives (`web/src/app/shared/ui/StatusChip.tsx`, `NoticeBanner.tsx`).
  Stray near-miss border radii moved onto the shared `--radius-*` scale, input
  focus states aligned on the accent-border treatment, and a few
  never-defined `var(--color-text)` references were fixed to
  `--color-text-primary`. Raw hex in `.tsx` is now the exception (meta
  theme-color + the approval-card HTML-preview inversion map only) — the
  system, token families, and the "no raw hex; add a semantic token" rule are
  documented in `web/src/app/DESIGN.md`. No redesign: same look, one source
  of truth.

### Security

This release closes a batch of security findings raised against the run loop,
the scheduler, the HTTP surface, and the native tools. Every fix is a strict
tightening — no previously-sealed posture was widened, and each restores or
reinforces a documented invariant.

- Lockdown network seal on approved bash (#562, P0): approved-bash execution
  ran in a network-enabled warm-pool container, silently dropping the
  `--network=none` hard seal for lockdown conversations. Staged bash now takes
  the sealed container whenever the conversation is locked down or
  `CHAT_LOCKDOWN_ONLY=true`, fail-closed, mirroring the turn path.
- Lockdown model allowlist bypass (#568, P1): the per-turn model override on the
  existing-conversation chat path skipped the lockdown allowlist gate the
  create/PATCH paths enforce; overrides are now checked and rejected before the
  turn runs.
- Persona tool-allowlist (Gate-4) bypass (#570, P1): once BM25 tool disclosure
  deferred MCP tools behind bridges, the persona allowlist stopped governing
  them. Persona policy is now applied to each logical MCP tool before the
  disclosure decision, so denied tools never enter the deferred registry.
- Secret scrubber gaps (#569, P1): the redactor missed
  `aws_secret_access_key`-style names and `Authorization: Basic` headers,
  leaking credentials into the model context and logs; both are now scrubbed.
- API-key rotation stale hash (#567, P1): a second rotation left the first
  rotated-out key hash valid indefinitely; the outgoing predecessor is now
  revoked so only the current key and the most recent predecessor (within grace)
  authenticate.
- DeleteUser credential inheritance (#563, P1): deleting a user orphaned their
  `remote_mcp_servers` rows and encrypted OAuth tokens, so re-using the email
  inherited them; deletion now purges them (and owned projects) in-transaction.
- download_url workspace escape (#564, P1): a relative `output_dir` containing
  `..` escaped the workspace to arbitrary host-side writes; traversal segments
  are now rejected before the path is joined.
- Cross-conversation workspace traversal (#575): file tools accepted `..` to
  reach other conversations' workspaces; per-conversation confinement is now
  enforced across the view/edit/write and upload paths.
- Email content_file arbitrary read (#573): outbound email could materialize
  arbitrary host files via absolute paths and `$VAR`/`~` expansion; content is
  now confined to the conversation workspace with no shell expansion.
- SSRF classifier gaps (#574): `web_fetch`/`web_search` did not block CGNAT
  `100.64.0.0/10` (Alibaba/Oracle cloud-metadata) or several reserved ranges.
  The private/reserved-IP classifier is now a single shared `internal/netguard`
  implementation (previously duplicated and drifted) covering CGNAT, the RFC
  test/benchmark/reserved nets, and multicast, applied at dial time.
- output_schema external `$ref` file read (#585): an untrusted `output_schema`
  with a `file://` (or `http(s)://`) `$ref` made the host open arbitrary files
  during schema compile; external ref resolution is now refused.
- Personal memory API scope escape (#577): personal `UpdateMemory`/`DeleteMemory`
  omitted `project_id IS NULL`, letting project-scoped memory be mutated via the
  personal API; the guard is now applied.
- Webhook signature timing oracle (#572): Slack and custom-HMAC-header trigger
  verification returned early on shape mismatches, leaking a slug-enumeration
  timing side channel; verification now performs the same body-HMAC work on
  every path and fails closed.
- Shared-link limiter DoS (#571): `/shared/{token}` created an unbounded rate
  bucket per unvalidated token; the token is now resolved before a bucket is
  created, bounding the map and making the abuse gate effective.
- Config-reload secret rotation (#584): reload silently rotated file-sourced
  webhook/Slack HMAC secrets, bypassing the non-reloadable contract; such
  secrets are now held at their boot value and reported under `Skipped`.
- HTTP-tool JSON body injection (#600): inline HTTP-tool JSON body templates
  interpolated model args raw, allowing JSON injection into the outbound
  request; values substituted into JSON bodies are now JSON-escaped (fail-closed).
- Content-Security-Policy for the public share iframe (#590): responses now
  carry a CSP (strictest on `/shared/*`) so assistant-authored HTML in the
  sandboxed preview iframe can no longer beacon out to attacker hosts.
- Sandbox image CVEs (#620): the sandbox image pins patched `pypdf`, `tornado`,
  `pip`, `idna`, and `pygments` over the lagging Fedora RPMs (removing the RPM
  copies so a single unambiguous version remains), clearing 41 Grype findings
  (verified 41 → 0 with the CI-pinned scanner).
- Static-analysis hardening: enabled CodeQL code scanning, secret scanning +
  push protection, Dependabot security updates, and private vulnerability
  reporting. Addressed the resulting CodeQL findings — path-injection sinks
  routed through recognized confinement barriers, unbounded allocations bounded,
  and a narrowing integer conversion guarded; findings that were provably safe
  (dial-time-guarded SSRF, high-entropy-token SHA-256 with constant-time
  compare, HTTP-boundary-confined image paths) were dismissed with written
  justification.
