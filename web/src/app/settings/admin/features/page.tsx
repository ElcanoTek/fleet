"use client";

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
// This page renders ONLY what the live registry serves plus the live Rampart
// actions (detection probe + one-click service install) — no mock-only rows.

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
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
  InlineConfirmButton,
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

type ResolvedSetting = {
  key: string;
  kind: "bool" | "int" | "enum" | "url" | "model";
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
  // The stored value failed to apply at boot (e.g. a rampart engine whose
  // service URL vanished from the env) — env-derived behavior serves.
  apply_error?: string;
};

type SettingMeta = {
  label: string;
  description: string;
  // Per-enum-option one-liners, shown for the selected option (PII modes).
  optionHelp?: Record<string, string>;
  // Suffix hint for numeric fields ("bytes — 0 uses 64 KiB; hard max 128 KiB").
  unitHint?: string;
};

const META: Record<string, SettingMeta> = {
  pii_redaction_mode: {
    label: "PII redaction",
    description:
      "OPTIONAL and OFF by default. When on, scans every tool result for PII (emails, SSNs, credit cards, IPs, phone numbers) before it reaches the model, on top of the always-on secret scrubber. It only ever touches TOOL OUTPUT — never what users type into chat — so you can always send PII in a message; set the mode to Off to disable it entirely. Deterministic pattern matching (or the Rampart engine below) — a redaction aid, not a certified DLP engine.",
    optionHelp: {
      off: "Off (default): tool output passes through unchanged. PII is never redacted anywhere.",
      observe: "Detect and audit-log findings (kind + count, never the value) without changing the output — a monitoring posture.",
      redact: "Replace each detected span with a [PII:kind] marker so the model sees the structure without the value.",
      block: "Withhold any tool result containing PII entirely — the strictest posture; the model sees only a blocked notice.",
    },
  },
  pii_redaction_engine: {
    label: "PII detection engine",
    description:
      "Which detector PII redaction uses WHEN it is on (no effect while the mode above is Off). Pattern: built-in deterministic regexes (emails, SSNs, cards, IPs, phones) — no dependencies. Rampart: a small ML token-classification model (17 entity types incl. names, addresses, government IDs, bank numbers) running as a service you deploy next to fleet — see docs/PII-REDACTION.md. If the Rampart service is unreachable, tool calls fall back to the pattern engine.",
    optionHelp: {
      pattern: "Deterministic regex detection: five PII shapes, zero moving parts.",
      rampart: "ML detection via your Rampart service (requires the service URL below). Redacts with stable numbered placeholders like [GIVEN_NAME_1].",
    },
  },
  pii_rampart_url: {
    label: "Rampart service URL",
    description:
      "Endpoint of your Rampart detection service (e.g. http://127.0.0.1:8787/v1/redact). Deploy it from scripts/rampart-service in the fleet repo. Leave empty when using the pattern engine.",
  },
  guardrail_mode: {
    label: "Prompt-injection guardrail",
    description:
      "Host-side screening of user/task input and tool output before it enters the model context. The detector is probabilistic and complements, never replaces, Fleet's sandbox and approvals.",
    optionHelp: {
      off: "Off (default): no detector calls and existing behavior is unchanged.",
      observe: "Record verdicts and detector outages without blocking content.",
      block: "Withhold flagged content; detector outages fail closed.",
    },
  },
  guardrail_url: {
    label: "Guardrail detector URL",
    description:
      "HTTP endpoint for the operator-deployed detector. Fleet sends profile, source, and text and expects a flagged verdict. Keep it on loopback or a private network.",
  },
  tool_disclosure_threshold: {
    label: "Tool disclosure threshold",
    description:
      "When a conversation's tool roster grows past this many tools, MCP tools are searched on demand (tool_search → tool_call) instead of all being advertised at once. Lower it to shrink prompts on tool-heavy workspaces.",
    unitHint: "tools",
  },
  max_tool_output_bytes: {
    label: "Tool output ceiling",
    description:
      "Cap any single tool result before it enters the context window. Oversized structured output stays valid and points to governed workspace recovery when available; binary is never inlined.",
    unitHint: "bytes — 0 uses 64 KiB; hard max 128 KiB",
  },
  approval_timeout_seconds: {
    label: "Approval timeout",
    description:
      "How long a pending approval card (send email, risky bash, scheduled-task changes, bundle-declared critical tools) waits for a human before it is auto-denied. The card shows a live countdown; a card that times out is safely denied and the agent is told the action was not taken. Set it once for the workspace — a per-conversation override and per-tool bundle windows still win when set. Email previews never expire (there is nothing to deny).",
    unitHint: "seconds — 60 to 86400 (24h); default 3600 (1h)",
  },
  phone_a_friend_enabled: {
    label: "Phone-a-friend review",
    description:
      "Have a second model review each scheduled run's final output once before it's accepted. Adds one extra model call per run.",
  },
  subagents_enabled: {
    label: "Sub-agent delegation",
    description:
      "Fleet-wide kill switch for governed sub-agents (on by default). When on, scheduled tasks (unless a task opts out via allow_delegation) and chat turns register spawn_subagent — the agent decides whether to use it. Children inherit ceilings and policy from the parent run; turning this off hides the tool everywhere.",
  },
  memory_autoindex_enabled: {
    label: "Memory auto-index",
    description:
      "After each chat turn, extract durable facts into the user's memory automatically instead of relying on explicit remember requests. Uses a cheap model call per turn.",
  },
  error_analysis_enabled: {
    label: "Task error analysis",
    description:
      "When a scheduled task fails terminally, run an LLM diagnosis of what went wrong and surface it on the task page.",
  },
  auto_title_enabled: {
    label: "Auto-title conversations",
    description: "Generate a conversation title from the first exchange with a cheap model call.",
  },
  connector_recommendations_enabled: {
    label: "Connector recommendations",
    description:
      "Suggest a not-yet-enabled connector when a chat message looks relevant to it. Deterministic keyword matching over the catalog — no model call, nothing leaves the box.",
  },
  context_handles_enabled: {
    label: "Composer context handles",
    description: "Let users @-reference files and past conversations from the chat composer.",
  },
  default_model: {
    label: "Default model",
    description:
      "What a new conversation starts on — the first pinned, \u201Crecommended\u201D row in every model picker. Pick from the OpenRouter catalog or any workspace provider configured under Admin \u2192 Model providers (Bedrock, OpenAI-direct, \u2026 appear as provider/model), or type any slug. Applies live: the web re-reads it on every shell mount. Conversations that already picked a model keep it.",
  },
  advanced_model: {
    label: "Advanced model",
    description:
      "The stronger escalation target — what the agent\u2019s \u201Cswitch to the advanced model\u201D suggestion and the spreadsheet nudge move a conversation to, and the second pinned picker row. Same sources as the default model. Applies live, including to suggestion cards already on screen.",
  },
};

const PRIVACY_GROUP = "Privacy & data protection";

const GROUPS: { title: string; keys: string[] }[] = [
  {
    title: PRIVACY_GROUP,
    keys: ["pii_redaction_mode", "pii_redaction_engine", "pii_rampart_url", "guardrail_mode", "guardrail_url"],
  },
  {
    title: "Agent runtime",
    keys: [
      "tool_disclosure_threshold",
      "max_tool_output_bytes",
      "approval_timeout_seconds",
      "phone_a_friend_enabled",
      "subagents_enabled",
    ],
  },
  {
    title: "Model tiers",
    keys: ["default_model", "advanced_model"],
  },
  {
    title: "Workspace features",
    keys: [
      "memory_autoindex_enabled",
      "error_analysis_enabled",
      "auto_title_enabled",
      "connector_recommendations_enabled",
      "context_handles_enabled",
    ],
  },
];

// The Rampart action block has no setting row of its own; this is the haystack
// the filter matches so "install", "podman", etc. keep the block visible.
const RAMPART_ACTIONS_SEARCH_TEXT =
  "rampart test detection install rampart service redactor podman";

async function fetchSettings(): Promise<ResolvedSetting[] | null> {
  const response = await fetch("/api/admin/settings", { cache: "no-store" });
  if (response.status === 401) {
    // location.assign() rather than a location.href write: identical navigation
    // (full document load, one history entry), stated as a call instead of a
    // mutation of a global — the same form used in write() below, whose comment
    // says it mirrors this one.
    window.location.assign("/login");
    return null;
  }
  if (response.status === 403) {
    throw new Error("You are not on the admin allowlist.");
  }
  if (response.status === 501) {
    throw new Error("Workspace settings are not available on this deployment.");
  }
  if (!response.ok) {
    throw new Error(`Settings request failed: ${response.status}`);
  }
  const data = (await response.json()) as { settings: ResolvedSetting[] };
  return data.settings ?? [];
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
  if (admin !== "admin") return null;
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
  const [q, setQ] = useState("");

  const settings = loaded ? loaded.map((s) => edits[s.key] ?? s) : null;

  const write = async (key: string, init: RequestInit) => {
    setSaving((prev) => ({ ...prev, [key]: true }));
    setRowErrors((prev) => ({ ...prev, [key]: "" }));
    try {
      const response = await fetch(`/api/admin/settings/${encodeURIComponent(key)}`, init);
      if (response.status === 401) {
        // Session expired mid-edit: re-authenticate instead of surfacing a raw
        // 401 body as a row error (mirrors fetchSettings).
        window.location.assign("/login");
        return;
      }
      if (!response.ok) {
        const text = (await response.text()).trim();
        throw new Error(text || `Request failed: ${response.status}`);
      }
      const updated = (await response.json()) as ResolvedSetting;
      setEdits((prev) => ({ ...prev, [updated.key]: updated }));
      setDrafts((prev) => {
        const next = { ...prev };
        delete next[updated.key];
        return next;
      });
    } catch (err) {
      setRowErrors((prev) => ({
        ...prev,
        [key]: err instanceof Error ? err.message : "Failed to save.",
      }));
    } finally {
      setSaving((prev) => {
        const next = { ...prev };
        delete next[key];
        return next;
      });
    }
  };

  const save = (key: string, value: string) =>
    write(key, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ value }),
    });

  const reset = (key: string) => write(key, { method: "DELETE" });

  const byKey = new Map((settings ?? []).map((s) => [s.key, s]));
  const grouped = GROUPS.map((g) => ({
    title: g.title,
    settings: g.keys.map((k) => byKey.get(k)).filter(Boolean) as ResolvedSetting[],
  })).filter((g) => g.settings.length > 0);
  // Settings the server registers that this file doesn't know yet.
  const known = new Set(GROUPS.flatMap((g) => g.keys));
  const other = (settings ?? []).filter((s) => !known.has(s.key));
  if (other.length > 0) grouped.push({ title: "Other", settings: other });

  // Live filtering over label + description + env var + key (plus the Rampart
  // action block's search text); a group with nothing visible hides entirely.
  const ql = q.trim().toLowerCase();
  const rowMatches = (s: ResolvedSetting) => {
    if (!ql) return true;
    const meta = metaFor(s.key);
    return `${meta.label} ${meta.description} ${s.env_var} ${s.key}`.toLowerCase().includes(ql);
  };
  const actionsMatch = !ql || RAMPART_ACTIONS_SEARCH_TEXT.includes(ql);
  const visibleGroups = grouped
    .map((g) => ({
      title: g.title,
      settings: g.settings.filter(rowMatches),
      actions: g.title === PRIVACY_GROUP && actionsMatch,
    }))
    .filter((g) => g.settings.length > 0 || g.actions);

  return (
    <SetSection
      title="Features"
      intro="Workspace-wide feature toggles. Every change applies live — the next turn, run, or tool call picks it up; nothing here needs a restart. Each setting’s deployment default comes from the env var shown on the row. Reset reverts to it."
    >
      <div data-testid="feature-settings-panel">
        <div className="sticky top-0 z-20 mb-[1.1rem] grid gap-[0.55rem] border-b border-[var(--color-border)] bg-[var(--color-bg)] pb-[0.75rem] pt-[0.7rem] [background-attachment:fixed] [background-image:var(--gradient-bg)] [background-size:cover]">
          <DirSearch
            value={q}
            onChange={setQ}
            placeholder="Filter settings…"
            label="Filter settings"
          />
        </div>

        {error ? (
          <div className="flex items-center gap-3">
            <NoticeBanner tone="danger" className="flex-1">
              {error}
            </NoticeBanner>
            <button
              type="button"
              onClick={() => void reload()}
              className={btnClass({ sm: true, reveal: true })}
            >
              Retry
            </button>
          </div>
        ) : loading || settings === null ? (
          <p className="text-[0.8rem] text-[var(--color-text-muted)]">Loading…</p>
        ) : visibleGroups.length === 0 ? (
          ql ? (
            <ConnEmpty>No settings match “{q}”.</ConnEmpty>
          ) : (
            <ConnEmpty>No workspace settings are registered on this deployment.</ConnEmpty>
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
                {group.actions ? (
                  <>
                    <RampartActions />
                    <GuardrailActions />
                  </>
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
            <ConnBadge variant="overridden">Overridden</ConnBadge>
          ) : (
            <ConnBadge variant="neutral">Server default</ConnBadge>
          )}
          {resettable ? (
            <button
              type="button"
              onClick={onReset}
              disabled={busy}
              data-testid={`reset-${setting.key}`}
              className="cursor-pointer border-none bg-transparent p-0 text-[0.72rem] text-[var(--color-accent)] underline underline-offset-2 focus-visible:rounded-[0.25rem] focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50"
            >
              Reset
            </button>
          ) : null}
        </div>
        {meta.description ? (
          <p className="m-0 text-[0.76rem] leading-[1.55] text-[var(--color-text-secondary)]">
            {meta.description}
          </p>
        ) : null}
        <p className="m-0 text-[0.7rem] text-[var(--color-text-muted)]">
          Default {setting.default !== "" ? <CodeChip>{setting.default}</CodeChip> : null} from{" "}
          <CodeChip>{setting.env_var}</CodeChip>
          {resettable && setting.updated_by ? <> · set by {setting.updated_by}</> : null}
        </p>
        {setting.kind === "enum" && meta.optionHelp?.[setting.value] ? (
          <p className="m-0 text-[0.73rem] text-[var(--color-text-muted)]">
            {meta.optionHelp[setting.value]}
          </p>
        ) : null}
        {stale ? (
          <p className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]">
            A stored override is outside this setting&apos;s current bounds and is being ignored —
            the server default is in effect. Reset to clear it.
          </p>
        ) : null}
        {setting.apply_error ? (
          <p
            className="m-0 text-[0.73rem] text-[var(--color-warning-soft)]"
            data-testid={`apply-error-${setting.key}`}
          >
            This value is saved but NOT in effect — it failed to apply at startup:{" "}
            {setting.apply_error}. Fix it or Reset.
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
        ) : setting.kind === "model" ? (
          <ModelControl
            setting={setting}
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
          Save
        </button>
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
  busy,
  draft,
  setDraft,
  onSave,
}: {
  setting: ResolvedSetting;
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
          placeholder="provider/model"
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
          Save
        </button>
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
          type="number"
          inputMode="numeric"
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
            Save
          </button>
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

/* ── Rampart actions (Privacy group): detection probe + one-click install ── */

// PIIProbe result — the live redactor (exactly what tool calls go through) run
// over a synthetic sample: engine, detected kinds, latency, redacted preview.
// A dead Rampart service reports as a failure here (tool calls themselves fall
// back to the pattern engine).
type PIIProbeResult = {
  ok: boolean;
  engine: string;
  mode: string;
  detail: string;
  redacted?: string;
  latency_ms: number;
};

// Install-job status from /api/admin/pii-redaction/install.
type PIIInstallStatus = {
  state: "idle" | "running" | "done" | "failed";
  log: string[] | null;
  container_running: boolean;
  url?: string;
};

function RampartActions() {
  const [probe, setProbe] = useState<"idle" | "running" | PIIProbeResult>("idle");

  const runProbe = async () => {
    setProbe("running");
    try {
      const response = await fetch("/api/admin/pii-redaction/test", { method: "POST" });
      if (!response.ok) {
        throw new Error((await response.text()).trim() || `Probe failed: ${response.status}`);
      }
      setProbe((await response.json()) as PIIProbeResult);
    } catch (err) {
      setProbe({
        ok: false,
        engine: "",
        mode: "",
        detail: err instanceof Error ? err.message : "Probe failed.",
        latency_ms: 0,
      });
    }
  };

  return (
    <div
      className="grid gap-[0.55rem] border-b border-[var(--color-border-subtle)] px-[0.1rem] py-[0.95rem] last:border-b-0"
      data-testid="pii-probe"
    >
      <div className="flex flex-wrap items-center gap-[0.65rem]">
        <button
          type="button"
          onClick={() => void runProbe()}
          disabled={probe === "running"}
          data-testid="pii-probe-run"
          className={btnClass({ sm: true, reveal: true })}
        >
          Test detection
        </button>
        {probe === "running" ? (
          <ActStatus state="running">Running the redactor over the sample…</ActStatus>
        ) : probe === "idle" ? (
          <ActNote>runs the live redactor over a synthetic sample — save changes first</ActNote>
        ) : null}
      </div>
      {probe !== "idle" && probe !== "running" ? (
        <div className="grid gap-[0.3rem]" data-testid="pii-probe-result">
          <ActStatus state={probe.ok ? "ok" : "err"}>
            {probe.ok ? "✓" : "✕"} {probe.engine ? `${probe.engine} engine (${probe.mode})` : ""}
            {probe.detail ? ` — ${probe.detail}` : ""}
            {probe.latency_ms > 0 ? ` (${probe.latency_ms} ms)` : ""}
          </ActStatus>
          {probe.redacted ? (
            <code className="block overflow-x-auto rounded-[0.3rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-1 font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-secondary)]">
              {probe.redacted}
            </code>
          ) : null}
        </div>
      ) : null}
      <PIIInstallLine />
    </div>
  );
}

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
      if (!response.ok) throw new Error((await response.text()).trim() || `Probe failed: ${response.status}`);
      setProbe((await response.json()) as GuardrailProbeResult);
    } catch (err) {
      setProbe({
        ok: false,
        mode: "",
        profile: "",
        flagged: false,
        detail: err instanceof Error ? err.message : "Probe failed.",
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
          Test prompt-injection guardrail
        </button>
        {probe === "running" ? (
          <ActStatus state="running">Checking the live detector…</ActStatus>
        ) : probe === "idle" ? (
          <ActNote>uses a fixed synthetic injection sample — save the URL and mode first</ActNote>
        ) : null}
      </div>
      {probe !== "idle" && probe !== "running" ? (
        <ActStatus state={probe.ok ? "ok" : "err"}>
          {probe.ok ? "✓" : "✕"} {probe.profile ? `${probe.profile} (${probe.mode})` : ""}
          {probe.ok ? ` — ${probe.flagged ? "flagged" : "not flagged"}` : ""}
          {probe.detail ? ` — ${probe.detail}` : ""}
          {probe.latency_ms > 0 ? ` (${probe.latency_ms} ms)` : ""}
        </ActStatus>
      ) : null}
    </div>
  );
}

// PIIInstallLine — the one-click Rampart service install: fleet builds the
// service container (model baked in), runs it on loopback, supervises it, and
// saves the URL setting. 501 (installer not wired) hides the affordance.
function PIIInstallLine() {
  const [status, setStatus] = useState<PIIInstallStatus | null | "unavailable">(null);
  const [error, setError] = useState("");
  // The in-flight poll timer lives in a ref so the mount effect's cleanup can
  // stop it: an install takes minutes, and an admin who navigates away mid-way
  // must not leave a 3s fetch loop setting state on an unmounted component.
  const pollTimer = useRef<ReturnType<typeof setInterval> | null>(null);

  const stopPolling = useCallback(() => {
    if (pollTimer.current !== null) {
      clearInterval(pollTimer.current);
      pollTimer.current = null;
    }
  }, []);

  const refresh = useCallback(async (): Promise<PIIInstallStatus | null> => {
    const response = await fetch("/api/admin/pii-redaction/install", { cache: "no-store" });
    if (response.status === 501) {
      setStatus("unavailable");
      return null;
    }
    if (!response.ok) return null;
    const st = (await response.json()) as PIIInstallStatus;
    setStatus(st);
    return st;
  }, []);

  const poll = useCallback(() => {
    stopPolling();
    pollTimer.current = setInterval(() => {
      // A transient network failure mid-poll is not a terminal state: swallow
      // it and let the next tick retry rather than reject unhandled.
      void refresh()
        .then((st) => {
          if (st && st.state !== "running") stopPolling();
        })
        .catch(() => undefined);
    }, 3000);
  }, [refresh, stopPolling]);

  useEffect(() => {
    // Kick off the status fetch on a microtask so no setState runs in the
    // effect's synchronous phase (react-hooks/set-state-in-effect); resume
    // polling if an install is already running when the panel mounts.
    let cancelled = false;
    const id = setTimeout(() => {
      void refresh()
        .then((st) => {
          if (!cancelled && st?.state === "running") poll();
        })
        .catch(() => undefined);
    }, 0);
    return () => {
      cancelled = true;
      clearTimeout(id);
      stopPolling();
    };
  }, [refresh, poll, stopPolling]);

  // The two mutations share one shape: a failed HTTP status or a thrown fetch
  // (network down, JSON parse) both land in the inline error line instead of
  // an unhandled rejection that leaves the button looking like it did nothing.
  const mutate = async (method: "POST" | "DELETE", failLabel: string) => {
    setError("");
    try {
      const response = await fetch("/api/admin/pii-redaction/install", { method });
      if (!response.ok) {
        setError((await response.text()).trim() || `${failLabel}: ${response.status}`);
        return null;
      }
      const st = (await response.json()) as PIIInstallStatus;
      setStatus(st);
      return st;
    } catch (err) {
      setError(err instanceof Error ? err.message : failLabel);
      return null;
    }
  };

  const install = async () => {
    const st = await mutate("POST", "Install failed");
    if (st) poll();
  };

  const uninstall = () => mutate("DELETE", "Uninstall failed");

  if (status === "unavailable" || status === null) return null;

  const running = status.state === "running";
  const lastLog = status.log?.length ? status.log[status.log.length - 1] : "";

  return (
    <div className="flex flex-wrap items-center gap-[0.65rem]" data-testid="pii-install">
      {status.container_running ? (
        <>
          <ConnBadge variant="success">Service installed</ConnBadge>
          <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
            {status.url}
          </span>
          <InlineConfirmButton
            label="Remove"
            confirmLabel="Confirm remove"
            onConfirm={() => void uninstall()}
            testId="pii-install-remove"
          />
        </>
      ) : (
        <>
          <button
            type="button"
            onClick={() => void install()}
            disabled={running}
            data-testid="pii-install-run"
            className={btnClass({ sm: true, reveal: true })}
          >
            Install Rampart service
          </button>
          {running ? (
            <ActStatus state="running">{lastLog || "working…"}</ActStatus>
          ) : (
            <ActNote>
              builds + runs the detection service on this box (podman, loopback) and fills in the
              URL
            </ActNote>
          )}
        </>
      )}
      {status.state === "failed" && lastLog ? (
        <p
          className="m-0 w-full text-[0.74rem] text-[var(--color-danger)]"
          role="alert"
          data-testid="pii-install-error"
        >
          {lastLog}
        </p>
      ) : null}
      {error ? (
        <p
          className="m-0 w-full text-[0.74rem] text-[var(--color-danger)]"
          role="alert"
          data-testid="pii-install-request-error"
        >
          {error}
        </p>
      ) : null}
    </div>
  );
}
