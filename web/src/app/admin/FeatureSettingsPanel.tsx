"use client";

import { useCallback, useEffect, useState } from "react";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { humanizeVarName } from "@/app/shared/lib/taskTemplates";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { StatusChip } from "@/app/shared/ui/StatusChip";

// FeatureSettingsPanel — the admin Features panel: every workspace feature
// setting the server registers (GET /api/admin/settings), with its effective
// value, where it came from (env default vs admin override), and controls to
// change or reset it. Every registered setting applies LIVE — the server
// registry only admits settings whose consumers re-read per turn/run — so
// there is deliberately no "restart required" state to render.
//
// Presentation (labels, grouping, help copy) lives here; the server owns the
// registry, types, bounds, and validation. A setting the server adds before
// this file learns about it still renders (in "Other", from its raw key) —
// new backend settings never silently vanish from the panel.

type ResolvedSetting = {
  key: string;
  kind: "bool" | "int" | "enum" | "url";
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
  // Suffix hint for numeric fields ("bytes — 0 disables the ceiling").
  unitHint?: string;
};

const META: Record<string, SettingMeta> = {
  pii_redaction_mode: {
    label: "PII redaction",
    description:
      "Scan every tool result for PII (emails, SSNs, credit cards, IPs, phone numbers) before it reaches the model, on top of the always-on secret scrubber. Deterministic pattern matching — a redaction aid, not a certified DLP engine.",
    optionHelp: {
      off: "Tool output passes through unchanged.",
      observe: "Detect and audit-log findings (kind + count, never the value) without changing the output — a monitoring posture.",
      redact: "Replace each detected span with a [PII:kind] marker so the model sees the structure without the value.",
      block: "Withhold any tool result containing PII entirely — the strictest posture; the model sees only a blocked notice.",
    },
  },
  pii_redaction_engine: {
    label: "PII detection engine",
    description:
      "How PII is detected. Pattern: built-in deterministic regexes (emails, SSNs, cards, IPs, phones) — no dependencies. Rampart: a small ML token-classification model (17 entity types incl. names, addresses, government IDs, bank numbers) running as a service you deploy next to fleet — see docs/PII-REDACTION.md. If the Rampart service is unreachable, tool calls fall back to the pattern engine.",
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
  tool_disclosure_threshold: {
    label: "Tool disclosure threshold",
    description:
      "When a conversation's tool roster grows past this many tools, MCP tools are searched on demand (tool_search → tool_call) instead of all being advertised at once. Lower it to shrink prompts on tool-heavy workspaces.",
    unitHint: "tools",
  },
  max_tool_output_bytes: {
    label: "Tool output ceiling",
    description:
      "Cap any single tool result before it enters the context window; oversized output keeps its head and tail. Protects against a cat of a huge file blowing out the conversation.",
    unitHint: "bytes — 0 removes the ceiling",
  },
  phone_a_friend_enabled: {
    label: "Phone-a-friend review",
    description:
      "Have a second model review each scheduled run's final output once before it's accepted. Adds one extra model call per run.",
  },
  subagents_enabled: {
    label: "Sub-agent delegation",
    description:
      "Allow scheduled tasks to spawn governed child agents fleet-wide (individual tasks can also opt in with allow_delegation). Children inherit ceilings and policy from the parent run.",
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
};

const GROUPS: { title: string; keys: string[] }[] = [
  {
    title: "Privacy & data protection",
    keys: ["pii_redaction_mode", "pii_redaction_engine", "pii_rampart_url"],
  },
  {
    title: "Agent runtime",
    keys: [
      "tool_disclosure_threshold",
      "max_tool_output_bytes",
      "phone_a_friend_enabled",
      "subagents_enabled",
    ],
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

async function fetchSettings(): Promise<ResolvedSetting[] | null> {
  const response = await fetch("/api/admin/settings", { cache: "no-store" });
  if (response.status === 401) {
    window.location.href = "/login";
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

export function FeatureSettingsPanel() {
  // Load via the shared cancellation-safe hook; edits below patch rows into
  // `edits` (keyed by setting) so a PUT/DELETE response re-renders one row
  // without a refetch, and reload() gives the error state a real Retry.
  const { data: loaded, loading, error, reload } = useCancellableFetch<ResolvedSetting[] | null>(
    fetchSettings,
    [],
  );
  const [edits, setEdits] = useState<Record<string, ResolvedSetting>>({});
  // Per-key UI state: one write at a time, numeric drafts, row-level errors.
  const [saving, setSaving] = useState(false);
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({});

  const settings = loaded ? loaded.map((s) => edits[s.key] ?? s) : null;

  const write = async (key: string, init: RequestInit) => {
    setSaving(true);
    setRowErrors((prev) => ({ ...prev, [key]: "" }));
    try {
      const response = await fetch(`/api/admin/settings/${encodeURIComponent(key)}`, init);
      if (response.status === 401) {
        // Session expired mid-edit: re-authenticate instead of surfacing a raw
        // 401 body as a row error (mirrors fetchSettings).
        window.location.href = "/login";
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
      setSaving(false);
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

  return (
    <section
      className="mt-4 rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]"
      data-testid="feature-settings-panel"
    >
      <div className="flex items-center justify-between border-b border-[var(--color-border)] px-4 py-3">
        <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
          Feature settings
        </span>
      </div>
      <p className="px-4 pt-3 text-[0.75rem] text-[var(--color-text-muted)]">
        Workspace-wide feature toggles. Every change applies live — the next turn, run, or tool
        call picks it up; nothing here needs a restart. Each setting&apos;s deployment default
        comes from the env var shown on the row; <em>Reset</em> reverts to it.
      </p>

      {error ? (
        <div className="flex items-center gap-3 px-4 py-3">
          <NoticeBanner tone="danger" className="flex-1">
            {error}
          </NoticeBanner>
          <button
            type="button"
            onClick={() => void reload()}
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] transition hover:bg-[var(--color-overlay-soft)]"
          >
            Retry
          </button>
        </div>
      ) : loading || settings === null ? (
        <p className="px-4 py-4 text-[0.8125rem] text-[var(--color-text-muted)]">Loading…</p>
      ) : (
        grouped.map((group) => (
          <div key={group.title} className="border-t border-[var(--color-border-subtle)] first:border-t-0">
            <h3 className="px-4 pt-3 text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              {group.title}
            </h3>
            <ul>
              {group.settings.map((s) => (
                <SettingRow
                  key={s.key}
                  setting={s}
                  busy={saving}
                  rowError={rowErrors[s.key] ?? ""}
                  draft={drafts[s.key]}
                  setDraft={(v) => setDrafts((prev) => ({ ...prev, [s.key]: v }))}
                  onSave={(value) => save(s.key, value)}
                  onReset={() => reset(s.key)}
                />
              ))}
            </ul>
            {group.title === "Privacy & data protection" ? <PIIProbe /> : null}
          </div>
        ))
      )}
    </section>
  );
}

function SettingRow({
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
  const customized = setting.source === "admin";
  // A stale row (stored override no longer within the current bounds) is not
  // in effect, but it must stay visible and resettable — silently hiding it
  // would let it spring back to life if a later release loosens the bounds.
  const stale = setting.stale === true;
  const resettable = customized || stale;

  return (
    <li
      className="border-t border-[var(--color-border-subtle)] px-4 py-3 first:border-t-0"
      data-testid={`setting-${setting.key}`}
    >
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="min-w-0 flex-1 basis-64">
          <div className="flex items-center gap-2">
            <span className="text-[0.875rem] font-medium text-[var(--color-text-primary)]">
              {meta.label}
            </span>
            {customized ? (
              <StatusChip tone="success">Customized</StatusChip>
            ) : stale ? (
              <StatusChip tone="warning">Ignored override</StatusChip>
            ) : (
              <StatusChip tone="neutral">Server default</StatusChip>
            )}
          </div>
          {meta.description ? (
            <p className="mt-1 text-[0.75rem] leading-relaxed text-[var(--color-text-secondary)]">
              {meta.description}
            </p>
          ) : null}
          <p className="mt-1 text-[0.6875rem] text-[var(--color-text-muted)]">
            Default{" "}
            <code className="rounded bg-[var(--color-overlay-soft)] px-1">{setting.default}</code>{" "}
            from{" "}
            <code className="rounded bg-[var(--color-overlay-soft)] px-1">{setting.env_var}</code>
            {resettable && setting.updated_by ? <> · set by {setting.updated_by}</> : null}
          </p>
          {stale ? (
            <p className="mt-1 text-[0.75rem] text-[var(--color-warning-soft)]">
              A stored override is outside this setting&apos;s current bounds and is being
              ignored — the server default is in effect. Reset to clear it.
            </p>
          ) : null}
          {setting.apply_error ? (
            <p className="mt-1 text-[0.75rem] text-[var(--color-warning-soft)]" data-testid={`apply-error-${setting.key}`}>
              This value is saved but NOT in effect — it failed to apply at startup:{" "}
              {setting.apply_error}. Fix it or Reset.
            </p>
          ) : null}
        </div>

        <div className="flex shrink-0 items-center gap-2">
          {setting.kind === "bool" ? (
            <BoolControl setting={setting} busy={busy} onSave={onSave} />
          ) : setting.kind === "enum" ? (
            <EnumControl setting={setting} busy={busy} onSave={onSave} />
          ) : setting.kind === "url" ? (
            <UrlControl
              setting={setting}
              busy={busy}
              draft={draft}
              setDraft={setDraft}
              onSave={onSave}
            />
          ) : (
            <IntControl
              setting={setting}
              busy={busy}
              draft={draft}
              setDraft={setDraft}
              unitHint={meta.unitHint}
              onSave={onSave}
            />
          )}
          {resettable ? (
            <button
              type="button"
              onClick={onReset}
              disabled={busy}
              data-testid={`reset-${setting.key}`}
              className="rounded-full border border-[var(--color-border-subtle)] px-2.5 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-50"
            >
              Reset
            </button>
          ) : null}
        </div>
      </div>

      {setting.kind === "enum" && meta.optionHelp?.[setting.value] ? (
        <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
          {meta.optionHelp[setting.value]}
        </p>
      ) : null}

      {rowError ? (
        <p className="mt-2 text-[0.75rem] text-[var(--color-danger-soft)]" role="alert">
          {rowError}
        </p>
      ) : null}
    </li>
  );
}

// BoolControl — an accessible switch styled as the app's pill toggle.
function BoolControl({
  setting,
  busy,
  onSave,
}: {
  setting: ResolvedSetting;
  busy: boolean;
  onSave: (value: string) => void;
}) {
  const on = setting.value === "true";
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={metaFor(setting.key).label}
      disabled={busy}
      onClick={() => onSave(on ? "false" : "true")}
      data-testid={`toggle-${setting.key}`}
      className={`relative h-6 w-11 rounded-full border transition disabled:opacity-50 ${
        on
          ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_35%,transparent)]"
          : "border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)]"
      }`}
    >
      <span
        aria-hidden
        className={`absolute top-1/2 h-4 w-4 -translate-y-1/2 rounded-full transition-[left] ${
          on ? "left-[1.4rem] bg-[var(--color-accent)]" : "left-1 bg-[var(--color-text-muted)]"
        }`}
      />
    </button>
  );
}

// EnumControl — a segmented pill row, one pill per legal value.
function EnumControl({
  setting,
  busy,
  onSave,
}: {
  setting: ResolvedSetting;
  busy: boolean;
  onSave: (value: string) => void;
}) {
  return (
    <div
      role="radiogroup"
      aria-label={metaFor(setting.key).label}
      className="flex flex-wrap gap-1"
    >
      {(setting.enum ?? []).map((option) => {
        const active = setting.value === option;
        return (
          <button
            key={option}
            type="button"
            role="radio"
            aria-checked={active}
            disabled={busy || active}
            onClick={() => onSave(option)}
            data-testid={`option-${setting.key}-${option}`}
            className={`rounded-full border px-3 py-1 text-[0.75rem] transition disabled:opacity-60 ${
              active
                ? "border-[var(--color-accent)] font-medium text-[var(--color-text-primary)]"
                : "border-[var(--color-border-subtle)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            }`}
          >
            {humanizeVarName(option)}
          </button>
        );
      })}
    </div>
  );
}

// UrlControl — a text field for KindURL settings with an explicit Save once
// dirty (mirrors IntControl; a URL shouldn't fire a write per keystroke).
function UrlControl({
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
    <div className="flex items-center gap-2">
      <input
        type="url"
        value={value}
        disabled={busy}
        placeholder="http://127.0.0.1:8787/v1/redact"
        aria-label={metaFor(setting.key).label}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && dirty) onSave(value);
        }}
        data-testid={`input-${setting.key}`}
        className="w-72 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-1.5 font-mono text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)] disabled:opacity-50"
      />
      {dirty ? (
        <button
          type="button"
          onClick={() => onSave(value)}
          disabled={busy}
          data-testid={`save-${setting.key}`}
          className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
        >
          Save
        </button>
      ) : null}
    </div>
  );
}

// PIIProbe — the Privacy group's "Test detection" affordance: runs the LIVE
// redactor (exactly what tool calls go through) over a synthetic sample and
// shows the engine, detected kinds, latency, and the redacted preview so the
// admin can see the marker style. A dead Rampart service reports as a failure
// here (tool calls themselves fall back to the pattern engine).
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

// PIIInstall — the one-click Rampart service install: fleet builds the
// service container (model baked in), runs it on loopback, supervises it, and
// saves the URL setting. 501 (installer not wired) hides the affordance.
function PIIInstall() {
  const [status, setStatus] = useState<PIIInstallStatus | null | "unavailable">(null);
  const [error, setError] = useState("");

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
    const timer = setInterval(() => {
      void refresh().then((st) => {
        if (st && st.state !== "running") clearInterval(timer);
      });
    }, 3000);
  }, [refresh]);

  useEffect(() => {
    // Kick off the status fetch on a microtask so no setState runs in the
    // effect's synchronous phase (react-hooks/set-state-in-effect); resume
    // polling if an install is already running when the panel mounts.
    let cancelled = false;
    const id = setTimeout(() => {
      void refresh().then((st) => {
        if (!cancelled && st?.state === "running") poll();
      });
    }, 0);
    return () => {
      cancelled = true;
      clearTimeout(id);
    };
  }, [refresh, poll]);

  const install = async () => {
    setError("");
    const response = await fetch("/api/admin/pii-redaction/install", { method: "POST" });
    if (!response.ok) {
      setError((await response.text()).trim() || `Install failed: ${response.status}`);
      return;
    }
    setStatus((await response.json()) as PIIInstallStatus);
    poll();
  };

  const uninstall = async () => {
    if (!window.confirm("Remove the fleet-managed Rampart service container?")) return;
    setError("");
    const response = await fetch("/api/admin/pii-redaction/install", { method: "DELETE" });
    if (!response.ok) {
      setError((await response.text()).trim() || `Uninstall failed: ${response.status}`);
      return;
    }
    setStatus((await response.json()) as PIIInstallStatus);
  };

  if (status === "unavailable" || status === null) return null;

  const running = status.state === "running";
  const lastLog = status.log?.length ? status.log[status.log.length - 1] : "";

  return (
    <div className="mt-2 flex flex-wrap items-center gap-2" data-testid="pii-install">
      {status.container_running ? (
        <>
          <StatusChip tone="success">Service installed</StatusChip>
          <span className="font-mono text-[0.6875rem] text-[var(--color-text-muted)]">{status.url}</span>
          <button
            type="button"
            onClick={() => void uninstall()}
            data-testid="pii-install-remove"
            className="rounded-full border border-[var(--color-border-subtle)] px-2.5 py-1 text-[0.6875rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
          >
            Remove
          </button>
        </>
      ) : (
        <>
          <button
            type="button"
            onClick={() => void install()}
            disabled={running}
            data-testid="pii-install-run"
            className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
          >
            {running ? "Installing…" : "Install Rampart service"}
          </button>
          <span className="text-[0.6875rem] text-[var(--color-text-muted)]">
            {running
              ? lastLog || "working…"
              : "builds + runs the detection service on this box (podman, loopback) and fills in the URL"}
          </span>
        </>
      )}
      {status.state === "failed" && lastLog ? (
        <p className="w-full text-[0.75rem] text-[var(--color-danger-soft)]" role="alert" data-testid="pii-install-error">
          {lastLog}
        </p>
      ) : null}
      {error ? (
        <p className="w-full text-[0.75rem] text-[var(--color-danger-soft)]" role="alert">
          {error}
        </p>
      ) : null}
    </div>
  );
}

function PIIProbe() {
  const [state, setState] = useState<"idle" | "running" | PIIProbeResult>("idle");

  const run = async () => {
    setState("running");
    try {
      const response = await fetch("/api/admin/pii-redaction/test", { method: "POST" });
      if (!response.ok) {
        throw new Error((await response.text()).trim() || `Probe failed: ${response.status}`);
      }
      setState((await response.json()) as PIIProbeResult);
    } catch (err) {
      setState({
        ok: false,
        engine: "",
        mode: "",
        detail: err instanceof Error ? err.message : "Probe failed.",
        latency_ms: 0,
      });
    }
  };

  return (
    <div className="border-t border-[var(--color-border-subtle)] px-4 py-3" data-testid="pii-probe">
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => void run()}
          disabled={state === "running"}
          data-testid="pii-probe-run"
          className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-50"
        >
          {state === "running" ? "Testing…" : "Test detection"}
        </button>
        <span className="text-[0.6875rem] text-[var(--color-text-muted)]">
          runs the live redactor over a synthetic sample — save changes first
        </span>
      </div>
      <PIIInstall />
      {state !== "idle" && state !== "running" ? (
        <div className="mt-2 text-[0.75rem]" data-testid="pii-probe-result">
          <p className={state.ok ? "text-[var(--color-success-soft)]" : "text-[var(--color-danger-soft)]"}>
            {state.ok ? "✓" : "✕"} {state.engine ? `${state.engine} engine (${state.mode})` : ""}
            {state.detail ? ` — ${state.detail}` : ""}
            {state.latency_ms > 0 ? ` (${state.latency_ms} ms)` : ""}
          </p>
          {state.redacted ? (
            <code className="mt-1 block overflow-x-auto rounded bg-[var(--color-overlay-soft)] px-2 py-1 font-mono text-[0.6875rem] text-[var(--color-text-secondary)]">
              {state.redacted}
            </code>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

// IntControl — a numeric field with an explicit Save that appears once the
// value is dirty (numbers shouldn't fire a workspace-wide write per keystroke).
function IntControl({
  setting,
  busy,
  draft,
  setDraft,
  unitHint,
  onSave,
}: {
  setting: ResolvedSetting;
  busy: boolean;
  draft: string | undefined;
  setDraft: (v: string) => void;
  unitHint?: string;
  onSave: (value: string) => void;
}) {
  const value = draft ?? setting.value;
  const dirty = draft !== undefined && draft !== setting.value;
  return (
    <div className="flex items-center gap-2">
      <input
        type="number"
        inputMode="numeric"
        value={value}
        min={setting.min_zero_ok ? 0 : setting.min}
        max={setting.max}
        disabled={busy}
        aria-label={metaFor(setting.key).label}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && dirty) onSave(value);
        }}
        data-testid={`input-${setting.key}`}
        className="w-32 rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-1.5 text-right text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)] disabled:opacity-50"
      />
      {unitHint ? (
        <span className="hidden text-[0.6875rem] text-[var(--color-text-muted)] sm:inline">
          {unitHint}
        </span>
      ) : null}
      {dirty ? (
        <button
          type="button"
          onClick={() => onSave(value)}
          disabled={busy}
          data-testid={`save-${setting.key}`}
          className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 text-[0.75rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
        >
          Save
        </button>
      ) : null}
    </div>
  );
}
