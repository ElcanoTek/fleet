import { formatFixed } from "@/app/lib/i18n";
import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
// Settings → Admin → Features (fleet-unified settings pass): every workspace
// feature setting the server registers (GET /api/admin/settings), with its
// effective value, where it came from (env default vs admin override), and
// controls to change or reset it. Every registered setting applies LIVE — the
// server registry only admits settings whose consumers re-read per turn/run —
// so there is deliberately no "restart required" state to render.
//
// Presentation (labels, grouping, help copy) lives here; the server owns the
// registry, types, bounds, and validation. A setting the server adds before
// this file learns about it still renders (in "Other", from its raw key) —
// new backend settings never silently vanish from the panel.
//
// This page renders ONLY what the live registry serves plus the live guardrail
// action (detector probe) — no mock-only rows.

import { useEffect, useState } from "react";
import { useRouter } from "@/app/lib/navigation";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { humanizeVarName } from "@/app/shared/lib/taskTemplates";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { ModelPicker } from "@/app/shared/ui/ModelPicker";
import {
  ActNote,
  ActStatus,
  btnClass,
  CodeChip,
  ConnBadge,
  Segmented,
  SETTINGS_INPUT,
  SetSwitch,
} from "../../ui/atoms";
import {
  ConnEmpty,
  ConnGroup,
  ConnGroupHead,
  ConnPanel,
  DirSearch,
  SetSection,
} from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { AdminGateFallback } from "../../AdminGateFallback";
import { responseErrorText } from "@/app/lib/responseError";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { getGatedJson, redirectToLogin } from "@/app/settings/lib/getGated";

type ResolvedSetting = {
  key: string;
  kind: "bool" | "int" | "float" | "enum" | "url" | "model";
  enum?: string[];
  min?: number;
  max?: number;
  min_zero_ok?: boolean;
  env_var: string;
  value: string;
  source: "admin" | "default";
  default: string;
  updated_at?: number;
  updated_by?: string;
  // An override row exists but no longer validates (bounds tightened in a
  // later release): the default serves, and the row can only be cleared.
  stale?: boolean;
  // The stored value failed to apply at boot (e.g. a guardrail mode whose
  // detector URL vanished from the env) — env-derived behavior serves.
  apply_error?: string;
};

type SettingMeta = {
  label: string;
  description: string;
  // Per-enum-option one-liners, shown for the selected option (PII modes).
  optionHelp?: Record<string, string>;
  // Suffix hint for numeric fields ("bytes — 0 uses 64 KiB; hard max 128 KiB").
  unitHint?: string;
  // What an EMPTY server default means, for a setting whose unset env value
  // is a state rather than a gap (task_model: follow the advanced model).
  emptyDefault?: string;
};

const META: Record<string, SettingMeta> = {
  pii_redaction_mode: {
    label: tr("PII redaction"),
    description:
      tr("Off by default. When on, Fleet looks for personal details (emails, US Social Security numbers, card numbers, phone numbers, IP addresses) in what connected tools send back, before the assistant sees it. What people type into chat is never changed. Secret filtering (passwords, keys) stays on whatever this is set to. Both are pattern-based and can miss things."),
    optionHelp: {
      off: tr("Off (default): tool output passes through unchanged. PII is never redacted anywhere."),
      observe: tr("Detect and audit-log findings (kind + count, never the value) without changing the output — a monitoring posture."),
      redact: tr("Replace each detected span with a [PII:kind] marker so the model sees the structure without the value."),
      block: tr("Withhold any tool result containing PII entirely — the strictest posture; the model sees only a blocked notice."),
    },
  },
  guardrail_mode: {
    label: tr("Prompt-injection guardrail"),
    description:
      tr("Screens messages, task instructions and tool results for attempts to trick the assistant into ignoring its instructions. Needs a detector service (the URL below). It adds a layer of protection; the sandbox and approvals still apply."),
    optionHelp: {
      off: tr("Off (default): no detector calls and existing behavior is unchanged."),
      observe: tr("Record verdicts and detector outages without blocking content."),
      block: tr("Withhold flagged content; detector outages fail closed."),
    },
  },
  guardrail_url: {
    label: tr("Guardrail detector URL"),
    description:
      tr("The web address of your detector service. Keep it on this server or a private network."),
  },
  tool_disclosure_threshold: {
    label: tr("Tool disclosure threshold"),
    description:
      tr("When a chat has more tools than this, the assistant searches for the tool it needs instead of seeing them all at once. Lower it to keep requests small in workspaces with many connections."),
    unitHint: "tools",
  },
  max_tool_output_bytes: {
    label: tr("Tool output ceiling"),
    description:
      tr("The largest result a single tool can hand to the assistant. Anything bigger is shortened, with a pointer to the full result where one is available."),
    unitHint: tr("bytes — 0 uses 64 KiB; hard max 128 KiB"),
  },
  max_cost_usd: {
    label: tr("Per-run cost ceiling"),
    description:
      tr("The most one chat reply or one task run can spend before Fleet stops it. Applies to everyone on this workspace from the next run."),
    unitHint: tr("USD — 1 to 100000, cents allowed; 0 = no ceiling"),
  },
  max_total_tokens: {
    label: tr("Per-run token ceiling"),
    description:
      tr("The most text (counted in tokens) one chat reply or one task run can use before Fleet stops it. Repeated text the model provider caches does not count. Applies from the next run."),
    unitHint: tr("tokens — 10000 to 1000000000; default 10000000; 0 = no ceiling"),
  },
  approval_timeout_seconds: {
    label: tr("Approval timeout"),
    description:
      tr("How long an action waiting for approval (sending an email, a risky command, changing a scheduled task) waits before it is declined automatically. The assistant is told the action did not happen. Some chats and tools set their own timeout, which wins. Email previews never expire."),
    unitHint: tr("seconds — 60 to 604800 (7d); default 86400 (24h)"),
  },
  subagents_enabled: {
    label: tr("Sub-agent delegation"),
    description:
      tr("On by default. Lets the assistant hand parts of a big job to helper assistants that follow the same limits and rules. Turn off to stop this everywhere. A single task can also opt out."),
  },
  memory_autoindex_enabled: {
    label: tr("Memory auto-index"),
    description:
      tr("After each chat reply, suggest lasting facts the person can save to their memories with one click. Uses one small model call per reply."),
  },
  error_analysis_enabled: {
    label: tr("Task error analysis"),
    description:
      tr("When a scheduled task fails, have the assistant explain what went wrong on the task's page."),
  },
  auto_title_enabled: {
    label: tr("Auto-title conversations"),
    description: tr("Name each new conversation from its first messages. Uses one small model call."),
  },
  connector_recommendations_enabled: {
    label: tr("Connector recommendations"),
    description:
      tr("Suggest a connection that isn't turned on when a message looks related to it. Uses keyword matching only: no model call, and nothing leaves this server."),
  },
  context_handles_enabled: {
    label: tr("Composer context handles"),
    description: tr("Let people type @ in the message box to point the assistant at a file or an earlier conversation."),
  },
  team_chats_shared_by_default: {
    label: tr("Multiplayer team chats by default"),
    description:
      tr("When on, new chats that team members start outside a personal project are shared with their team automatically, so teammates can open them and reply. Lockdown chats and chats in personal projects stay private. The chat's owner can stop sharing or remove people at any time. Everyone uses their own connected accounts."),
  },
  default_model: {
    label: tr("Default model"),
    description:
      tr("Our smartest frontier model, which every new conversation starts on by default and which appears first in each model picker. Users can switch to a cheaper or free model in the picker when they want to optimize spend. When rollout on save is enabled below, saving also upgrades existing chats and tasks."),
  },
  advanced_model: {
    label: tr("Fallback model"),
    description:
      tr("Our alternative smart frontier model from a different provider family — offered when a chat turn struggles, and used as the fallback for tasks and patrols if their primary model fails. Shown second in each model picker. When rollout on save is enabled below, saving updates task fallbacks and upgrades chats pinned to the previous fallback."),
  },
  task_model: {
    label: tr("Task model"),
    description:
      tr("The model background tasks use unless a task picks its own. Leave it empty to use the default model. When rollout on save is enabled below, saving updates upcoming scheduled and recurring tasks."),
    emptyDefault: tr("Follows the default model"),
  },
  escalation_signals_enabled: {
    label: tr("Offer the fallback model when a turn struggles"),
    description:
      tr("After a reply that clearly struggled (several tool errors in a row, the same step repeated, the chat had to be summarized, or a very long run), offer to switch to the fallback model. Free: no extra model call."),
  },
  escalation_classifier_mode: {
    label: tr("Frustration check"),
    description:
      tr("After each message from the second on, a small model reads the end of the chat to tell whether the person is unhappy with an answer. It runs alongside the reply, so nothing is slowed down, and costs a tiny fraction of a cent. Only recent messages are sent to that model's provider, never files or tool results. Lockdown chats are never checked."),
    optionHelp: {
      off: tr("Off: no check, nothing sent."),
      observe: tr("Check every message and count frustrated ones in Analytics, without showing a card."),
      suggest: tr("Suggest (default): check, count, and offer the fallback model when the user is pushing back."),
    },
  },
  escalation_classifier_model: {
    label: tr("Frustration check model"),
    description:
      tr("The small model used for the frustration check. The default follows its maker's newest release automatically. The list shows every model you can pick, cheapest first."),
  },
  escalation_classifier_threshold: {
    label: tr("Frustration threshold"),
    description:
      tr("How sure the check must be before a message counts as frustration. Higher means fewer, more certain suggestions."),
    unitHint: tr("percent — 50 to 99; default 80"),
  },
  monthly_soft_spend_limit_usd: {
    label: tr("Monthly soft spend limit"),
    description:
      tr("Soft monthly billed spend allowance in USD displayed on the Analytics overview banner (included / $0-cost models count $0 toward this limit). Set 0 for unlimited."),
    unitHint: tr("USD — 1 to 1000000; 0 = unlimited"),
  },
};

const PRIVACY_GROUP = tr("Privacy & data protection");
const MODEL_TIERS_GROUP = tr("Model tiers");
const MODEL_TIER_KEYS = new Set(["default_model", "advanced_model", "task_model"]);

const GROUPS: { title: string; keys: string[] }[] = [
  {
    title: PRIVACY_GROUP,
    keys: ["pii_redaction_mode", "guardrail_mode", "guardrail_url"],
  },
  {
    title: tr("Agent runtime"),
    keys: [
      "tool_disclosure_threshold",
      "max_tool_output_bytes",
      "approval_timeout_seconds",
      "max_cost_usd",
      "max_total_tokens",
      "subagents_enabled",
    ],
  },
  {
    title: MODEL_TIERS_GROUP,
    keys: ["default_model", "advanced_model", "task_model"],
  },
  {
    title: tr("Fallback-model suggestions"),
    keys: [
      "escalation_signals_enabled",
      "escalation_classifier_mode",
      "escalation_classifier_model",
      "escalation_classifier_threshold",
    ],
  },
  {
    title: tr("Workspace features"),
    keys: [
      "team_chats_shared_by_default",
      "memory_autoindex_enabled",
      "error_analysis_enabled",
      "auto_title_enabled",
      "connector_recommendations_enabled",
      "context_handles_enabled",
      "monthly_soft_spend_limit_usd",
    ],
  },
];

// The guardrail and model-rollout action blocks have no setting row of their
// own; these haystacks keep them visible when filtering by related keywords.
const GUARDRAIL_ACTIONS_SEARCH_TEXT = "guardrail test prompt-injection detector probe";
const MODEL_ROLLOUT_SEARCH_TEXT = "model upgrade rollout chats recurring tasks sync notify preview";

type ModelRolloutSummary = {
  dry_run: boolean;
  primary_model?: string;
  task_model?: string;
  fallback_model?: string;
  lockdown_included: boolean;
  chats_matched: number;
  chats_updated: number;
  chats_notified: number;
  tasks_scanned: number;
  tasks_updated: number;
  tasks_primary_updated: number;
  tasks_fallback_updated: number;
  tasks_recurring_updated: number;
  partial_error?: string;
};

type SettingWriteResponse = ResolvedSetting & {
  rollout?: ModelRolloutSummary;
  rollout_error?: string;
};

type RolloutUiStatus =
  | "idle"
  | "running"
  | { state: "ok" | "err"; message: string };

function formatModelRolloutSummary(r: ModelRolloutSummary): string {
  const lockdownExcluded = Boolean(r.primary_model) && r.lockdown_included === false;
  if (r.dry_run) {
    if (r.chats_updated === 0 && r.tasks_updated === 0) {
      return lockdownExcluded
        ? tr("Preview: all non-lockdown chats and scheduled tasks are already on the current tiers (lockdown chats excluded).")
        : tr("Preview: all existing chats and scheduled tasks are already on the current tiers.");
    }
    const parts: string[] = [];
    if (r.primary_model) {
      parts.push(
        r.chats_notified > 0
          ? tr("{0} chats ({1} active chats notified)", [r.chats_updated, r.chats_notified])
          : tr("{0} chats", [r.chats_updated]),
      );
    }
    if (r.task_model !== undefined || r.fallback_model !== undefined) {
      parts.push(
        r.tasks_recurring_updated > 0
          ? tr("{0} scheduled tasks ({1} recurring)", [r.tasks_updated, r.tasks_recurring_updated])
          : tr("{0} scheduled tasks", [r.tasks_updated]),
      );
    }
    if (lockdownExcluded) {
      parts.push(tr("lockdown chats excluded"));
    }
    return tr("Preview: {0} would be updated.", [parts.join(" · ")]);
  }
  if (r.chats_updated === 0 && r.tasks_updated === 0) {
    return lockdownExcluded
      ? tr("All non-lockdown chats and scheduled tasks are already on the current tiers (lockdown chats excluded).")
      : tr("All existing chats and scheduled tasks are already on the current tiers.");
  }
  const parts: string[] = [];
  if (r.primary_model || r.chats_updated > 0) {
    parts.push(
      r.chats_notified > 0
        ? tr("{0} chats updated ({1} notified)", [r.chats_updated, r.chats_notified])
        : tr("{0} chats updated", [r.chats_updated]),
    );
  }
  if (r.task_model !== undefined || r.fallback_model !== undefined || r.tasks_updated > 0) {
    parts.push(
      r.tasks_recurring_updated > 0
        ? tr("{0} scheduled tasks updated ({1} recurring)", [r.tasks_updated, r.tasks_recurring_updated])
        : tr("{0} scheduled tasks updated", [r.tasks_updated]),
    );
  }
  if (lockdownExcluded) {
    parts.push(tr("lockdown chats excluded"));
  }
  return parts.join(" · ");
}

async function fetchSettings(): Promise<ResolvedSetting[] | null> {
  const data = await getGatedJson<{ settings: ResolvedSetting[] }>("/api/admin/settings", {
    label: tr("Settings"),
    statusText: {
      403: tr("You are not on the admin allowlist."),
      501: tr("Workspace settings are not available on this deployment."),
    },
  });
  return data ? (data.settings ?? []) : null;
}

function metaFor(key: string): SettingMeta {
  return META[key] ?? { label: humanizeVarName(key), description: "" };
}

export default function FeaturesAdminPage() {
  // Client-side visibility gate only — every /api/admin call below is
  // independently authorized server-side regardless of what renders here.
  const admin = useIsAdmin();
  const router = useRouter();
  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);
  if (admin !== "admin") return <AdminGateFallback state={admin} />;
  return <FeaturesAdmin />;
}

function FeaturesAdmin() {
  // Load via the shared cancellation-safe hook; edits below patch rows into
  // `edits` (keyed by setting) so a PUT/DELETE response re-renders one row
  // without a refetch, and reload() gives the error state a real Retry.
  const { data: loaded, loading, error, reload } = useCancellableFetch<ResolvedSetting[] | null>(
    fetchSettings,
    [],
  );
  const [edits, setEdits] = useState<Record<string, ResolvedSetting>>({});
  // Per-key UI state: in-flight writes keyed by setting (so one slow save
  // disables only its own row, not the whole panel), numeric drafts,
  // row-level errors.
  const [saving, setSaving] = useState<Record<string, boolean>>({});
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({});
  const [rolloutOnSave, setRolloutOnSave] = useState(true);
  const [notifyChatsOnRollout, setNotifyChatsOnRollout] = useState(true);
  const [rolloutStatus, setRolloutStatus] = useState<RolloutUiStatus>("idle");
  const [q, setQ] = useState("");

  const settings = loaded ? loaded.map((s) => edits[s.key] ?? s) : null;

  // A refetch makes the server list authoritative again: `edits` are the
  // per-row PUT/DELETE echoes layered over the LAST list, and left in place
  // they would mask whatever the new list says (another admin's change, or a
  // Retry after an error). Drop them, then reload.
  const reloadAll = () => {
    setEdits({});
    setDrafts({});
    setRowErrors({});
    return reload();
  };

  const write = async (key: string, path: string, init: RequestInit) => {
    setSaving((prev) => ({ ...prev, [key]: true }));
    setRowErrors((prev) => ({ ...prev, [key]: "" }));
    try {
      const response = await fetch(path, init);
      if (response.status === 401) {
        // Session expired mid-edit: re-authenticate instead of surfacing a raw
        // 401 body as a row error (mirrors fetchSettings).
        redirectToLogin();
        return;
      }
      if (!response.ok) {
        const text = (await response.text()).trim();
        throw new Error(text || tr("Request failed: {0}", [response.status]));
      }
      const updated = (await response.json()) as SettingWriteResponse;
      const { rollout, rollout_error: rolloutError, ...resolved } = updated;
      setEdits((prev) => ({ ...prev, [resolved.key]: resolved }));
      setDrafts((prev) => {
        const next = { ...prev };
        delete next[resolved.key];
        return next;
      });
      if (rolloutError) {
        const detail =
          rollout && (rollout.chats_updated > 0 || rollout.tasks_updated > 0)
            ? `${formatModelRolloutSummary(rollout)} — ${rolloutError}`
            : rolloutError;
        setRolloutStatus({ state: "err", message: detail });
      } else if (rollout) {
        setRolloutStatus({ state: "ok", message: formatModelRolloutSummary(rollout) });
      }
    } catch (err) {
      setRowErrors((prev) => ({
        ...prev,
        [key]: errorMessage(err, tr("Failed to save.")),
      }));
    } finally {
      setSaving((prev) => {
        const next = { ...prev };
        delete next[key];
        return next;
      });
    }
  };

  const save = (key: string, value: string) => {
    const withRollout = MODEL_TIER_KEYS.has(key) && rolloutOnSave;
    return write(key, `/api/admin/settings/${encodeURIComponent(key)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(
        withRollout
          ? { value, rollout: true, notify_chats: notifyChatsOnRollout }
          : { value },
      ),
    });
  };

  const reset = (key: string) => {
    const withRollout = MODEL_TIER_KEYS.has(key) && rolloutOnSave;
    const qs = withRollout
      ? `?rollout=true&notify_chats=${notifyChatsOnRollout ? "true" : "false"}`
      : "";
    return write(key, `/api/admin/settings/${encodeURIComponent(key)}${qs}`, {
      method: "DELETE",
    });
  };

  const runModelRollout = async (dryRun: boolean) => {
    setRolloutStatus("running");
    try {
      const response = await fetch("/api/admin/models/rollout", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          sync_current: true,
          notify_chats: notifyChatsOnRollout,
          dry_run: dryRun,
        }),
      });
      if (response.status === 401) {
        redirectToLogin();
        return;
      }
      if (!response.ok) {
        throw new Error(await responseErrorText(response, tr("Model rollout failed.")));
      }
      const result = (await response.json()) as ModelRolloutSummary;
      if (result.partial_error) {
        setRolloutStatus({
          state: "err",
          message: `${formatModelRolloutSummary(result)} — ${result.partial_error}`,
        });
      } else {
        setRolloutStatus({ state: "ok", message: formatModelRolloutSummary(result) });
      }
    } catch (err) {
      setRolloutStatus({
        state: "err",
        message: errorMessage(err, tr("Model rollout failed.")),
      });
    }
  };

  const byKey = new Map((settings ?? []).map((s) => [s.key, s]));
  const grouped = GROUPS.map((g) => ({
    title: g.title,
    settings: g.keys.map((k) => byKey.get(k)).filter(Boolean) as ResolvedSetting[],
  })).filter((g) => g.settings.length > 0);
  // Settings the server registers that this file doesn't know yet.
  const known = new Set(GROUPS.flatMap((g) => g.keys));
  const other = (settings ?? []).filter((s) => !known.has(s.key));
  if (other.length > 0) grouped.push({ title: tr("Other"), settings: other });

  // Live filtering over label + description + env var + key (plus the guardrail
  // and model-rollout action blocks' search text); a group with nothing visible hides entirely.
  const ql = q.trim().toLowerCase();
  const rowMatches = (s: ResolvedSetting) => {
    if (!ql) return true;
    const meta = metaFor(s.key);
    return `${meta.label} ${meta.description} ${s.env_var} ${s.key}`.toLowerCase().includes(ql);
  };
  const guardrailActionsMatch = !ql || GUARDRAIL_ACTIONS_SEARCH_TEXT.includes(ql);
  const modelRolloutActionsMatch = !ql || MODEL_ROLLOUT_SEARCH_TEXT.includes(ql);
  const visibleGroups = grouped
    .map((g) => ({
      title: g.title,
      settings: g.settings.filter(rowMatches),
      guardrailActions: g.title === PRIVACY_GROUP && guardrailActionsMatch,
      modelRolloutActions: g.title === MODEL_TIERS_GROUP && modelRolloutActionsMatch,
    }))
    .filter((g) => g.settings.length > 0 || g.guardrailActions || g.modelRolloutActions);

  return (
    <SetSection
      title={tr("Features")}
      intro={tr("Settings for everyone in this workspace. Changes apply from the next message or task run, with no restart. Reset puts a setting back to the server default.")}
    >
      <div data-testid="feature-settings-panel">
        <div className="sticky top-0 z-20 mb-[1.1rem] grid gap-[0.55rem] border-b border-[var(--color-border)] bg-[var(--color-bg)] pb-[0.75rem] pt-[0.7rem] [background-attachment:fixed] [background-image:var(--gradient-bg)] [background-size:cover]">
          <DirSearch
            value={q}
            onChange={setQ}
            placeholder={tr("Filter settings…")}
            label={tr("Filter settings")}
          />
        </div>

        {error ? (
          <div className="flex items-center gap-3">
            <NoticeBanner tone="danger" className="flex-1">
              {error}
            </NoticeBanner>
            <button
              type="button"
              onClick={() => void reloadAll()}
              className={btnClass({ sm: true, reveal: true })}
            >
              {tr("Retry")}
            </button>
          </div>
        ) : loading || settings === null ? (
          <p className="text-[0.8rem] text-[var(--color-text-muted)]">{tr("Loading…")}</p>
        ) : visibleGroups.length === 0 ? (
          ql ? (
            <ConnEmpty>{tr("No settings match “")}{q}”.</ConnEmpty>
          ) : (
            <ConnEmpty>{tr("No workspace settings are registered on this deployment.")}</ConnEmpty>
          )
        ) : (
          visibleGroups.map((group) => (
            <ConnGroup key={group.title}>
              <ConnGroupHead title={group.title} />
              {/* `!` on the panel's pt/pb overrides — Tailwind v4 orders
                  same-property utilities by value, not class position. */}
              <ConnPanel className="@container pb-[0.35rem]! pt-[0.15rem]!">
                {group.settings.map((s) => (
                  <FeatRow
                    key={s.key}
                    setting={s}
                    busy={saving[s.key] === true}
                    rowError={rowErrors[s.key] ?? ""}
                    draft={drafts[s.key]}
                    setDraft={(v) => setDrafts((prev) => ({ ...prev, [s.key]: v }))}
                    onSave={(value) => void save(s.key, value)}
                    onReset={() => void reset(s.key)}
                  />
                ))}
                {group.guardrailActions ? <GuardrailActions /> : null}
                {group.modelRolloutActions ? (
                  <ModelTierRolloutActions
                    rolloutOnSave={rolloutOnSave}
                    setRolloutOnSave={setRolloutOnSave}
                    notifyChats={notifyChatsOnRollout}
                    setNotifyChats={setNotifyChatsOnRollout}
                    status={rolloutStatus}
                    onPreview={() => void runModelRollout(true)}
                    onSyncNow={() => void runModelRollout(false)}
                  />
                ) : null}
              </ConnPanel>
            </ConnGroup>
          ))
        )}
      </div>
    </SetSection>
  );
}

function FeatRow({
  setting,
  busy,
  rowError,
  draft,
  setDraft,
  onSave,
  onReset,
}: {
  setting: ResolvedSetting;
  busy: boolean;
  rowError: string;
  draft: string | undefined;
  setDraft: (v: string) => void;
  onSave: (value: string) => void;
  onReset: () => void;
}) {
  const meta = metaFor(setting.key);
  const overridden = setting.source === "admin";
  // A stale row (stored override no longer within the current bounds) is not
  // in effect, but it must stay visible and resettable — silently hiding it
  // would let it spring back to life if a later release loosens the bounds.
  const stale = setting.stale === true;
  const resettable = overridden || stale;

  return (
    <div
      className="grid grid-cols-[minmax(0,1fr)_15rem] items-start gap-x-5 border-b border-[var(--color-border-subtle)] px-[0.1rem] py-[0.95rem] last:border-b-0 @max-[34rem]:grid-cols-1 @max-[34rem]:gap-y-[0.6rem]"
      data-testid={`setting-${setting.key}`}
    >
      <div className="grid min-w-0 gap-[0.4rem]">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[0.88rem] font-semibold text-[var(--color-text-primary)]">
            {meta.label}
          </span>
          {overridden ? (
            <ConnBadge variant="overridden">{tr("Overridden")}</ConnBadge>
          ) : (
            <ConnBadge variant="neutral">{tr("Server default")}</ConnBadge>
          )}
          {resettable ? (
            <button
              type="button"
              onClick={onReset}
              disabled={busy}
              data-testid={`reset-${setting.key}`}
              className="cursor-pointer border-none bg-transparent p-0 text-[0.72rem] text-[var(--color-accent)] underline underline-offset-2 focus-visible:rounded-[0.25rem] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
            >
              {tr("Reset")}
            </button>
          ) : null}
        </div>
        {meta.description ? (
          <p className="m-0 text-[0.76rem] leading-[1.55] text-[var(--color-text-secondary)]">
            {meta.description}
          </p>
        ) : null}
        <p className="m-0 text-[0.7rem] text-[var(--color-text-muted)]">
          {tr("Default")}{" "}
          {setting.default !== "" ? (
            <CodeChip>{setting.default}</CodeChip>
          ) : meta.emptyDefault ? (
            <>({meta.emptyDefault.toLowerCase()} {tr("when unset)")}</>
          ) : null}{" "}
          {tr("from")}{" "}<CodeChip>{setting.env_var}</CodeChip>
          {resettable && setting.updated_by ? <> {tr("· set by")}{" "}{setting.updated_by}</> : null}
        </p>
        {setting.kind === "enum" && meta.optionHelp?.[setting.value] ? (
          <p className="m-0 text-[0.73rem] text-[var(--color-text-muted)]">
            {meta.optionHelp[setting.value]}
          </p>
        ) : null}
        {stale ? (
          <p className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]">
            {tr("A stored override is outside this setting's current bounds and is being ignored — the server default is in effect. Reset to clear it.")}
          </p>
        ) : null}
        {setting.apply_error ? (
          <p
            className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]"
            data-testid={`apply-error-${setting.key}`}
          >
        {rich("This value is saved but NOT in effect — it failed to apply at startup: {0}. Fix it or Reset.", setting.apply_error)}
      </p>
        ) : null}
        {rowError ? (
          <p className="m-0 text-[0.73rem] text-[var(--color-danger)]" role="alert">
            {rowError}
          </p>
        ) : null}
      </div>

      <div className="flex flex-wrap items-center justify-end gap-[0.45rem] pt-[0.1rem] @max-[34rem]:justify-start @max-[34rem]:pt-0">
        {setting.kind === "bool" ? (
          <SetSwitch
            on={setting.value === "true"}
            onToggle={() => onSave(setting.value === "true" ? "false" : "true")}
            label={meta.label}
            disabled={busy}
            testId={`toggle-${setting.key}`}
          />
        ) : setting.kind === "enum" ? (
          <Segmented
            value={setting.value}
            options={(setting.enum ?? []).map((o) => ({ value: o, label: humanizeVarName(o) }))}
            onChange={(next) => {
              // Re-picking the active option must not fire a workspace write.
              if (next !== setting.value) onSave(next);
            }}
            label={meta.label}
            disabled={busy}
          />
        ) : setting.kind === "url" ? (
          <UrlControl
            setting={setting}
            label={meta.label}
            busy={busy}
            draft={draft}
            setDraft={setDraft}
            onSave={onSave}
          />
        ) : setting.key === "escalation_classifier_model" ? (
          <DecisionModelControl
            setting={setting}
            label={meta.label}
            busy={busy}
            draft={draft}
            setDraft={setDraft}
            onSave={onSave}
          />
        ) : setting.kind === "model" ? (
          <ModelControl
            setting={setting}
            placeholder={meta.emptyDefault}
            busy={busy}
            draft={draft}
            setDraft={setDraft}
            onSave={onSave}
          />
        ) : (
          <IntControl
            setting={setting}
            label={meta.label}
            busy={busy}
            draft={draft}
            setDraft={setDraft}
            unitHint={meta.unitHint}
            onSave={onSave}
          />
        )}
      </div>
    </div>
  );
}

// UrlControl — a text field for KindURL settings with an explicit Save once
// dirty (mirrors IntControl; a URL shouldn't fire a write per keystroke).
function UrlControl({
  setting,
  label,
  busy,
  draft,
  setDraft,
  onSave,
}: {
  setting: ResolvedSetting;
  label: string;
  busy: boolean;
  draft: string | undefined;
  setDraft: (v: string) => void;
  onSave: (value: string) => void;
}) {
  const value = draft ?? setting.value;
  const dirty = draft !== undefined && draft !== setting.value;
  return (
    <>
      <input
        id={`input-${setting.key}`}
        type="url"
        value={value}
        disabled={busy}
        placeholder="http://127.0.0.1:8787/v1/redact"
        aria-label={label}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && dirty) onSave(value);
        }}
        data-testid={`input-${setting.key}`}
        // Same-property overrides need `!`: Tailwind v4 emits utilities in a
        // fixed value-sorted order, so a plain min-h/px/py/text appended after
        // SETTINGS_INPUT would lose to the base values.
        className={`${SETTINGS_INPUT} min-h-[2.2rem]! w-full px-[0.6rem]! py-[0.3rem]! font-[family-name:var(--font-code)] text-[0.74rem]!`}
      />
      {dirty ? (
        <button
          type="button"
          onClick={() => onSave(value)}
          disabled={busy}
          data-testid={`save-${setting.key}`}
          className={btnClass({ sm: true, reveal: true })}
        >
          {tr("Save")}</button>
      ) : null}
    </>
  );
}

type DecisionModelOption = { id: string; name: string; input_per_million: number };

// DecisionModelControl — the escalation classifier's model: a slug field with
// suggestions from OpenRouter's live decision-model list
// (GET /api/admin/decision-models), so a model released after this build is
// still one click away. The chat ModelPicker is the wrong source: decision
// models are not chat models and are absent from its catalog. Any slug can be
// typed; the moving default is offered first.
function DecisionModelControl({
  setting,
  label,
  busy,
  draft,
  setDraft,
  onSave,
}: {
  setting: ResolvedSetting;
  label: string;
  busy: boolean;
  draft: string | undefined;
  setDraft: (v: string) => void;
  onSave: (value: string) => void;
}) {
  const [options, setOptions] = useState<DecisionModelOption[]>([]);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const response = await fetch("/api/admin/decision-models", { cache: "no-store" });
        if (!response.ok) return;
        const data = (await response.json()) as { models?: DecisionModelOption[] };
        if (!cancelled && Array.isArray(data.models)) setOptions(data.models);
      } catch {
        // The field still takes a typed slug.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);
  const value = draft ?? setting.value;
  const dirty = draft !== undefined && draft !== setting.value;
  const listId = `decision-models-${setting.key}`;
  return (
    <>
      <input
        id={`input-${setting.key}`}
        type="text"
        list={listId}
        value={value}
        disabled={busy}
        placeholder="~typesafe/jev-latest"
        aria-label={label}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && dirty && value.trim() !== "") onSave(value.trim());
        }}
        data-testid={`input-${setting.key}`}
        className={`${SETTINGS_INPUT} min-h-[2.2rem]! w-full px-[0.6rem]! py-[0.3rem]! font-[family-name:var(--font-code)] text-[0.74rem]!`}
      />
      <datalist id={listId} data-testid={listId}>
        <option value="~typesafe/jev-latest">{tr("Jev, newest release (recommended)")}</option>
        {options.map((m) => (
          <option key={m.id} value={m.id}>
            {tr("{0} · ${1}/M in", [m.name, formatFixed(m.input_per_million, 3)])}
          </option>
        ))}
      </datalist>
      {dirty ? (
        <button
          type="button"
          onClick={() => onSave(value.trim())}
          disabled={busy || value.trim() === ""}
          data-testid={`save-${setting.key}`}
          className={btnClass({ sm: true, reveal: true })}
        >
          {tr("Save")}</button>
      ) : null}
    </>
  );
}

// ModelControl — the provider/model combobox for KindModel settings (#1187),
// with an explicit Save once dirty (a workspace-wide tier change shouldn't
// fire per keystroke). The picker unions the OpenRouter catalog with the
// admin-configured workspace providers ("<provider>/<model>" — Bedrock,
// OpenAI-direct, … from Admin → Model providers, catch-alls expanded from the
// catwalk model database), and any free-typed slug commits too — the server
// only enforces the provider/model shape.
function ModelControl({
  setting,
  placeholder,
  busy,
  draft,
  setDraft,
  onSave,
}: {
  setting: ResolvedSetting;
  // Shown while the value is empty (task_model unset follows the advanced model).
  placeholder?: string;
  busy: boolean;
  draft: string | undefined;
  setDraft: (v: string) => void;
  onSave: (value: string) => void;
}) {
  const value = draft ?? setting.value;
  const dirty = draft !== undefined && draft !== setting.value;
  return (
    <>
      <div className="w-full min-w-[16rem]" data-testid={`model-picker-${setting.key}`}>
        <ModelPicker
          id={`model-${setting.key}`}
          value={value}
          onChange={setDraft}
          placeholder={placeholder ?? "provider/model"}
          aria-describedby={undefined}
        />
      </div>
      {dirty ? (
        <button
          type="button"
          onClick={() => onSave(value.trim())}
          disabled={busy || value.trim() === ""}
          data-testid={`save-${setting.key}`}
          className={btnClass({ sm: true, reveal: true })}
        >
          {tr("Save")}</button>
      ) : null}
    </>
  );
}

// IntControl — a numeric field with an explicit Save that appears once the
// value is dirty (numbers shouldn't fire a workspace-wide write per keystroke).
function IntControl({
  setting,
  label,
  busy,
  draft,
  setDraft,
  unitHint,
  onSave,
}: {
  setting: ResolvedSetting;
  label: string;
  busy: boolean;
  draft: string | undefined;
  setDraft: (v: string) => void;
  unitHint?: string;
  onSave: (value: string) => void;
}) {
  const value = draft ?? setting.value;
  const dirty = draft !== undefined && draft !== setting.value;
  return (
    <span className="flex flex-col items-end gap-[0.3rem] @max-[34rem]:items-start">
      <span className="flex flex-wrap items-center gap-[0.45rem]">
        <input
          id={`input-${setting.key}`}
          type="number"
          inputMode={setting.kind === "float" ? "decimal" : "numeric"}
          step={setting.kind === "float" ? "0.01" : undefined}
          value={value}
          min={setting.min_zero_ok ? 0 : setting.min}
          max={setting.max}
          disabled={busy}
          aria-label={label}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && dirty) onSave(value);
          }}
          data-testid={`input-${setting.key}`}
          // `!` on every same-property override — see UrlControl.
          className={`${SETTINGS_INPUT} min-h-8! w-[5.5rem]! px-2! py-1! text-[0.78rem]!`}
        />
        {dirty ? (
          <button
            type="button"
            onClick={() => onSave(value)}
            disabled={busy}
            data-testid={`save-${setting.key}`}
            className={btnClass({ sm: true, reveal: true })}
          >
            {tr("Save")}</button>
        ) : null}
      </span>
      {unitHint ? (
        <span className="max-w-[11rem] text-right text-[0.68rem] text-[var(--color-text-muted)] @max-[34rem]:text-left">
          {unitHint}
        </span>
      ) : null}
    </span>
  );
}

/* ── Guardrail actions (Privacy group): detector probe ── */

type GuardrailProbeResult = {
  ok: boolean;
  mode: string;
  profile: string;
  flagged: boolean;
  score?: number;
  detail?: string;
  latency_ms: number;
};

function GuardrailActions() {
  const [probe, setProbe] = useState<"idle" | "running" | GuardrailProbeResult>("idle");
  const runProbe = async () => {
    setProbe("running");
    try {
      const response = await fetch("/api/admin/guardrail/test", { method: "POST" });
      if (!response.ok) throw new Error(await responseErrorText(response, tr("Probe failed")));
      setProbe((await response.json()) as GuardrailProbeResult);
    } catch (err) {
      setProbe({
        ok: false,
        mode: "",
        profile: "",
        flagged: false,
        detail: errorMessage(err, tr("Probe failed.")),
        latency_ms: 0,
      });
    }
  };
  return (
    <div className="grid gap-[0.55rem] px-[0.1rem] py-[0.95rem]" data-testid="guardrail-probe">
      <div className="flex flex-wrap items-center gap-[0.65rem]">
        <button
          type="button"
          onClick={() => void runProbe()}
          disabled={probe === "running"}
          data-testid="guardrail-probe-run"
          className={btnClass({ sm: true, reveal: true })}
        >
          {tr("Test prompt-injection guardrail")}
        </button>
        {probe === "running" ? (
          <ActStatus state="running">{tr("Checking the live detector…")}</ActStatus>
        ) : probe === "idle" ? (
          <ActNote>{tr("uses a fixed synthetic injection sample — save the URL and mode first")}</ActNote>
        ) : null}
      </div>
      {probe !== "idle" && probe !== "running" ? (
        <ActStatus state={probe.ok ? "ok" : "err"}>
          {probe.ok ? "✓" : "✕"} {probe.profile ? `${probe.profile} (${probe.mode})` : ""}
          {probe.ok ? ` — ${probe.flagged ? tr("flagged") : tr("not flagged")}` : ""}
          {probe.detail ? ` — ${probe.detail}` : ""}
          {probe.latency_ms > 0 ? tr(" ({0} ms)", [probe.latency_ms]) : ""}
        </ActStatus>
      ) : null}
    </div>
  );
}

/* ── Model rollout actions (Model tiers group): roll out upgrades across chats & tasks ── */

function ModelTierRolloutActions({
  rolloutOnSave,
  setRolloutOnSave,
  notifyChats,
  setNotifyChats,
  status,
  onPreview,
  onSyncNow,
}: {
  rolloutOnSave: boolean;
  setRolloutOnSave: (next: boolean) => void;
  notifyChats: boolean;
  setNotifyChats: (next: boolean) => void;
  status: RolloutUiStatus;
  onPreview: () => void;
  onSyncNow: () => void;
}) {
  const busy = status === "running";
  return (
    <div
      className="grid gap-[0.75rem] border-t border-[var(--color-border-subtle)] px-[0.1rem] py-[0.95rem]"
      data-testid="model-rollout-controls"
    >
      <div className="grid gap-[0.55rem]">
        <div className="flex items-center justify-between gap-4">
          <div className="grid min-w-0 gap-[0.15rem]">
            <span className="text-[0.82rem] font-semibold text-[var(--color-text-primary)]">
              {tr("Update existing chats & recurring tasks on save")}
            </span>
            <span className="text-[0.73rem] leading-[1.45] text-[var(--color-text-muted)]">
              {tr("Automatically upgrades existing conversations, patrols, and scheduled/recurring tasks whenever you save or reset a model tier above.")}
            </span>
          </div>
          <SetSwitch
            on={rolloutOnSave}
            onToggle={() => setRolloutOnSave(!rolloutOnSave)}
            label={tr("Update existing chats & recurring tasks on save")}
            small
            testId="toggle-model-rollout-on-save"
          />
        </div>

        <div className="flex items-center justify-between gap-4">
          <div className="grid min-w-0 gap-[0.15rem]">
            <span className="text-[0.82rem] font-semibold text-[var(--color-text-primary)]">
              {tr("Add an upgrade note to active chats")}
            </span>
            <span className="text-[0.73rem] leading-[1.45] text-[var(--color-text-muted)]">
              {tr("Adds a subtle inline banner in existing conversations when their model changes (never sent to the model or counted against cache).")}
            </span>
          </div>
          <SetSwitch
            on={notifyChats}
            onToggle={() => setNotifyChats(!notifyChats)}
            label={tr("Add an upgrade note to active chats")}
            small
            testId="toggle-model-rollout-notify"
          />
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-[0.55rem]">
        <button
          type="button"
          onClick={onSyncNow}
          disabled={busy}
          data-testid="model-rollout-sync-now"
          className={btnClass({ sm: true, reveal: true })}
        >
          {tr("Sync all chats & tasks to current tiers")}
        </button>
        <button
          type="button"
          onClick={onPreview}
          disabled={busy}
          data-testid="model-rollout-preview"
          className={btnClass({ sm: true })}
        >
          {tr("Preview")}
        </button>
        {status === "running" ? (
          <ActStatus state="running">{tr("Rolling out model tiers…")}</ActStatus>
        ) : status === "idle" ? (
          <ActNote>{tr("one-click sync aligns pre-existing chats and recurring tasks after a binary upgrade")}</ActNote>
        ) : null}
      </div>

      {status !== "idle" && status !== "running" ? (
        <ActStatus state={status.state}>
          {status.state === "ok" ? "✓ " : "✕ "}
          {status.message}
        </ActStatus>
      ) : null}
    </div>
  );
}
