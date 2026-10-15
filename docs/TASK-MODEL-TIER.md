# The task model tier

**Status:** shipped 2026-10-15.

## What shipped

fleet has a third model tier next to the two chat tiers. It is the **task
model**: the model an Operations Center task runs on unless the task names
another.

| Tier | Admin setting | Env default | Used for |
|---|---|---|---|
| Default | `default_model` | `FLEET_DEFAULT_MODEL` | New chats (fast, everyday) |
| Advanced | `advanced_model` | `FLEET_ADVANCED_MODEL` | Chat escalation (`suggest_advanced_model`) |
| **Task** | `task_model` | `FLEET_TASK_MODEL` | Operations Center tasks |

**Unset, the task tier follows the advanced tier.** `agentcore.CurrentTaskModel`
returns the task-tier override when one is set, otherwise the *current* advanced
slug. A deployment names its strong model once; changing the advanced model
moves the task default with it, with no second edit. An admin sets **Task model**
under Settings → Admin → Features → Model tiers only to give tasks a different
model from chat's advanced tier. Reset returns the setting to following the
advanced tier.

The task tier feeds every place a task's model is chosen by default:

- **The Operations Center create form** pre-fills it as the Primary model
  (`/client-config` `models.task_model`, `currentTaskModel()` in the web).
- **`schedule_task` from chat** pins it on the task when the agent names no
  model. A chat-scheduled task no longer inherits the conversation's model.
- **An unpinned task** (an API or import create with no `model`) runs on it,
  read at dispatch (`scheduledrun.runWorker`). The cost estimate reads it too.
- **`fleet task run`** seeds it from `FLEET_TASK_MODEL` / `FLEET_ADVANCED_MODEL`,
  because the one-shot harness has no settings service.

## Why

Chat and scheduled tasks need different models. Chat users want speed and pick
models to try; the default tier is a fast model (Claude Haiku 5.5 since
2026-10-15). A scheduled task is a long run with nobody watching it, which is
the job the strong model is for. The task form used to pre-fill the chat
default, so the Haiku swap moved every new task to Haiku as well. A separate
tier lets each surface have the model it needs. This replaces both "always edit
the task model by hand" and any automatic escalation-and-retry scheme: there is
no routing logic, just a different default.

## Behaviour changes

- **The #1014 "no model configured" create-time refusal is gone.** It refused an
  unpinned task on a deployment with no `FLEET_TASK_MODEL`, because the task
  could never run. The task tier is never empty, so that state cannot occur. The
  boot warning about a missing default task model went with it.
- **`FLEET_TASK_MODEL` is now the env default of a live admin setting.** Before,
  the scheduler snapshotted it at boot and an unpinned task failed without it.
  Now unset means "follow the advanced model", and an admin override applies to
  the next task without a restart.
- **Chat-scheduled tasks use the task tier instead of the chat's model** when
  the agent passes no `model` (`scheduledTaskModel`). The `schedule_task` tool
  description says so.

## What did not change

- **Existing tasks keep their pinned model.** A task saved with a model (every
  form-created task, and chat-scheduled tasks) records it at create time, so a
  later tier change does not move it. Moving tasks that were created on Haiku
  is an explicit operator step:
  `fleet sched task set-model --from-model anthropic/claude-haiku-5.5 --model <new-slug> --dry-run`
  (drop `--dry-run` to write)
  (or `POST /tasks/model`).
- **The fallback model** (`fallback_model` / `FLEET_TASK_FALLBACK_MODEL`) is
  unchanged. It is a provider-outage fallback, not an escalation target.
- Chat's default and advanced tiers, the lockdown allow-list (chat only), and
  `suggest_advanced_model` are unchanged.
- **No cost guard.** The strong tier costs more per token (Gemini 4 Argon is
  about 20× Claude Haiku 5.5). The per-run cost and token ceilings still bound
  every run.

## Deferred

- **Automatic escalation on failure** (rerun a failed task on a stronger model,
  remember the upgrade on success). This was considered and set aside in favour
  of the static default: it needs finer failure classes than `terminal`, and a
  side-effect guard so a rerun cannot repeat a write.
- A one-shot migration of tasks already pinned to the chat default. Deployments
  run `fleet sched task set-model` if they want it.
