"use client";

import { useState } from "react";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { StatusChip } from "@/app/shared/ui/StatusChip";

// NotificationsPanel — admin-managed task notifications (email + outbound
// webhook). Previously FLEET_SMTP_*/FLEET_WEBHOOK_*/FLEET_NOTIFY_ON env vars +
// a restart; now editable here and hot-swapped into the running notifier —
// the next task completion uses the new config. Secrets (SMTP password,
// webhook signing secret) are WRITE-ONLY: stored encrypted, never shown again;
// leave the field blank to keep the stored value. A saved admin config
// replaces the env config wholesale; "Use env config" reverts.

type NotifySettings = {
  notify_on: string;
  smtp_host: string;
  smtp_port: string;
  smtp_username: string;
  has_smtp_password: boolean;
  smtp_from: string;
  email_to: string;
  webhook_url: string;
  webhook_method: string;
  webhook_body_template: string;
  has_webhook_secret: boolean;
  updated_at?: number;
  updated_by?: string;
};

type View = {
  source: "admin" | "env";
  settings: NotifySettings;
  email_enabled: boolean;
  webhook_enabled: boolean;
  // Non-empty when the saved config is NOT in effect (e.g. secrets sealed
  // under a rotated encryption key) — rendered as a warning with the recovery
  // paths, which this panel itself provides (re-save or revert).
  degraded?: string;
};

type TestResult = { ok: boolean; detail: string; latency_ms: number };

// Draft mirrors the PUT body; the secret fields hold what the admin typed this
// session ("" = untouched → omitted from the payload → keep stored value).
type Draft = {
  notify_on: string;
  smtp_host: string;
  smtp_port: string;
  smtp_username: string;
  smtp_password: string;
  clear_smtp_password: boolean;
  smtp_from: string;
  email_to: string;
  webhook_url: string;
  webhook_method: string;
  webhook_body_template: string;
  webhook_secret: string;
  clear_webhook_secret: boolean;
};

function draftFrom(view: View): Draft {
  const s = view.settings;
  return {
    notify_on: s.notify_on,
    smtp_host: s.smtp_host,
    smtp_port: s.smtp_port || "587",
    smtp_username: s.smtp_username,
    smtp_password: "",
    clear_smtp_password: false,
    smtp_from: s.smtp_from,
    email_to: s.email_to,
    webhook_url: s.webhook_url,
    webhook_method: s.webhook_method || "POST",
    webhook_body_template: s.webhook_body_template,
    webhook_secret: "",
    clear_webhook_secret: false,
  };
}

async function fetchView(): Promise<View | null> {
  const response = await fetch("/api/admin/notify-settings", { cache: "no-store" });
  if (response.status === 401) {
    window.location.href = "/login";
    return null;
  }
  if (response.status === 403) {
    throw new Error("You are not on the admin allowlist.");
  }
  if (response.status === 501) {
    throw new Error("Notification settings are not available on this deployment.");
  }
  if (!response.ok) {
    throw new Error(`Notification settings request failed: ${response.status}`);
  }
  return (await response.json()) as View;
}

const STATUS_OPTIONS = ["success", "failure", "progress"] as const;

const inputClass =
  "rounded-[0.6rem] border border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] px-3 py-2 text-[0.875rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]";
const labelClass = "grid gap-1 text-[0.75rem] text-[var(--color-text-muted)]";

export function NotificationsPanel() {
  const { data: view, loading, error, reload } = useCancellableFetch<View | null>(fetchView, []);
  // edits holds only what the admin changed this session; the rendered form is
  // edits ?? the server view, derived in-render (no seeding effect — the form
  // appears in the same render as the header, and a fresh view after
  // save/revert re-seeds by clearing edits).
  const [edits, setEdits] = useState<Draft | null>(null);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [tests, setTests] = useState<Record<string, "running" | TestResult>>({});

  const draft = edits ?? (view ? draftFrom(view) : null);
  const setDraft = (d: Draft) => setEdits(d);

  const statuses = new Set(
    (draft?.notify_on ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
  );

  const toggleStatus = (status: string) => {
    if (!draft) return;
    const next = new Set(statuses);
    if (next.has(status)) {
      next.delete(status);
    } else {
      next.add(status);
    }
    setDraft({ ...draft, notify_on: [...next].join(",") });
  };

  const save = async () => {
    if (!draft) return;
    setBusy(true);
    setSaveError("");
    try {
      const body: Record<string, unknown> = {
        notify_on: draft.notify_on,
        smtp_host: draft.smtp_host,
        smtp_port: draft.smtp_port,
        smtp_username: draft.smtp_username,
        smtp_from: draft.smtp_from,
        email_to: draft.email_to,
        webhook_url: draft.webhook_url,
        webhook_method: draft.webhook_method,
        webhook_body_template: draft.webhook_body_template,
      };
      // Write-only secrets: include only when typed (replace) or explicitly
      // cleared; omitted = keep the stored value.
      if (draft.clear_smtp_password) body.smtp_password = "";
      else if (draft.smtp_password !== "") body.smtp_password = draft.smtp_password;
      if (draft.clear_webhook_secret) body.webhook_secret = "";
      else if (draft.webhook_secret !== "") body.webhook_secret = draft.webhook_secret;

      const response = await fetch("/api/admin/notify-settings", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      });
      if (response.status === 401) {
        window.location.href = "/login";
        return;
      }
      if (!response.ok) {
        throw new Error((await response.text()).trim() || `Save failed: ${response.status}`);
      }
      setTests({});
      setEdits(null);
      await reload();
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "Failed to save.");
    } finally {
      setBusy(false);
    }
  };

  const revert = async () => {
    if (!window.confirm("Discard the saved notification settings and use the server's env configuration?")) {
      return;
    }
    setBusy(true);
    setSaveError("");
    try {
      const response = await fetch("/api/admin/notify-settings", { method: "DELETE" });
      if (!response.ok) {
        throw new Error((await response.text()).trim() || `Revert failed: ${response.status}`);
      }
      setTests({});
      setEdits(null);
      await reload();
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "Failed to revert.");
    } finally {
      setBusy(false);
    }
  };

  const runTest = async (channel: "email" | "webhook") => {
    setTests((prev) => ({ ...prev, [channel]: "running" }));
    try {
      const response = await fetch("/api/admin/notify-settings/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ channel }),
      });
      if (!response.ok) {
        throw new Error((await response.text()).trim() || `Test failed: ${response.status}`);
      }
      const result = (await response.json()) as TestResult;
      setTests((prev) => ({ ...prev, [channel]: result }));
    } catch (err) {
      setTests((prev) => ({
        ...prev,
        [channel]: {
          ok: false,
          detail: err instanceof Error ? err.message : "Test failed.",
          latency_ms: 0,
        },
      }));
    }
  };

  return (
    <section
      className="mt-4 rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]"
      data-testid="notifications-panel"
    >
      <div className="flex items-center justify-between gap-2 border-b border-[var(--color-border)] px-4 py-3">
        <div className="flex items-center gap-2">
          <span className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
            Notifications
          </span>
          {view ? (
            view.source === "admin" ? (
              <StatusChip tone="success">Customized</StatusChip>
            ) : (
              <StatusChip tone="neutral">Env config</StatusChip>
            )
          ) : null}
        </div>
        {view?.source === "admin" ? (
          <button
            type="button"
            onClick={() => void revert()}
            disabled={busy}
            data-testid="notify-revert"
            className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-50"
          >
            Use env config
          </button>
        ) : null}
      </div>
      <p className="px-4 pt-3 text-[0.75rem] text-[var(--color-text-muted)]">
        Where task completions go: an email (SMTP) and/or a signed webhook. Saving applies
        immediately — the next task completion uses this config, no restart. Secrets are stored
        encrypted and never shown again; leave a secret field blank to keep the stored value.
        {view?.source === "admin" && view.settings.updated_by ? (
          <> Last saved by {view.settings.updated_by}.</>
        ) : null}
      </p>

      {view?.degraded ? (
        <div className="px-4 pt-3">
          <NoticeBanner tone="warning" data-testid="notify-degraded">
            {view.degraded}
          </NoticeBanner>
        </div>
      ) : null}

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
      ) : loading || !view || !draft ? (
        <p className="px-4 py-4 text-[0.8125rem] text-[var(--color-text-muted)]">Loading…</p>
      ) : (
        <div className="grid gap-4 px-4 py-4">
          <div>
            <h3 className="text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              Notify on
            </h3>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              {STATUS_OPTIONS.map((status) => {
                const active = statuses.has(status);
                return (
                  <button
                    key={status}
                    type="button"
                    role="checkbox"
                    aria-checked={active}
                    onClick={() => toggleStatus(status)}
                    disabled={busy}
                    data-testid={`notify-on-${status}`}
                    className={`rounded-full border px-3 py-1 text-[0.75rem] transition disabled:opacity-50 ${
                      active
                        ? "border-[var(--color-accent)] font-medium text-[var(--color-text-primary)]"
                        : "border-[var(--color-border-subtle)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)]"
                    }`}
                  >
                    {status}
                  </button>
                );
              })}
              <span className="text-[0.6875rem] text-[var(--color-text-muted)]">
                none selected = all terminal statuses
              </span>
            </div>
          </div>

          <ChannelSection
            title="Email (SMTP)"
            enabled={view.email_enabled}
            testState={tests.email}
            onTest={() => void runTest("email")}
            busy={busy}
            testId="email"
          >
            <div className="grid gap-2 sm:grid-cols-2">
              <label className={labelClass}>
                SMTP host
                <input
                  value={draft.smtp_host}
                  onChange={(e) => setDraft({ ...draft, smtp_host: e.target.value })}
                  placeholder="smtp.example.com"
                  data-testid="notify-smtp-host"
                  className={inputClass}
                />
              </label>
              <label className={labelClass}>
                Port
                <input
                  value={draft.smtp_port}
                  onChange={(e) => setDraft({ ...draft, smtp_port: e.target.value })}
                  inputMode="numeric"
                  className={inputClass}
                />
              </label>
              <label className={labelClass}>
                Username (optional)
                <input
                  value={draft.smtp_username}
                  onChange={(e) => setDraft({ ...draft, smtp_username: e.target.value })}
                  autoComplete="off"
                  className={inputClass}
                />
              </label>
              <SecretField
                label="Password"
                stored={view.settings.has_smtp_password}
                value={draft.smtp_password}
                clear={draft.clear_smtp_password}
                onChange={(value) => setDraft({ ...draft, smtp_password: value, clear_smtp_password: false })}
                onClear={(clear) => setDraft({ ...draft, clear_smtp_password: clear, smtp_password: "" })}
                testId="notify-smtp-password"
              />
              <label className={labelClass}>
                From address
                <input
                  value={draft.smtp_from}
                  onChange={(e) => setDraft({ ...draft, smtp_from: e.target.value })}
                  placeholder="fleet@example.com"
                  className={inputClass}
                />
              </label>
              <label className={labelClass}>
                Recipients (comma-separated)
                <input
                  value={draft.email_to}
                  onChange={(e) => setDraft({ ...draft, email_to: e.target.value })}
                  placeholder="ops@example.com, oncall@example.com"
                  data-testid="notify-email-to"
                  className={inputClass}
                />
              </label>
            </div>
          </ChannelSection>

          <ChannelSection
            title="Webhook"
            enabled={view.webhook_enabled}
            testState={tests.webhook}
            onTest={() => void runTest("webhook")}
            busy={busy}
            testId="webhook"
          >
            <div className="grid gap-2">
              <div className="grid gap-2 sm:grid-cols-[1fr_8rem]">
                <label className={labelClass}>
                  URL
                  <input
                    value={draft.webhook_url}
                    onChange={(e) => setDraft({ ...draft, webhook_url: e.target.value })}
                    placeholder="https://hooks.example.com/fleet"
                    data-testid="notify-webhook-url"
                    className={inputClass}
                  />
                </label>
                <label className={labelClass}>
                  Method
                  <select
                    value={draft.webhook_method}
                    onChange={(e) => setDraft({ ...draft, webhook_method: e.target.value })}
                    className={inputClass}
                  >
                    <option>POST</option>
                    <option>PUT</option>
                    <option>PATCH</option>
                  </select>
                </label>
              </div>
              <div className="grid gap-2 sm:grid-cols-2">
                <SecretField
                  label="Signing secret (outbound HMAC — see docs/WEBHOOK-SIGNING.md)"
                  stored={view.settings.has_webhook_secret}
                  value={draft.webhook_secret}
                  clear={draft.clear_webhook_secret}
                  onChange={(value) => setDraft({ ...draft, webhook_secret: value, clear_webhook_secret: false })}
                  onClear={(clear) => setDraft({ ...draft, clear_webhook_secret: clear, webhook_secret: "" })}
                  testId="notify-webhook-secret"
                />
                <label className={labelClass}>
                  Body template (optional; Go text/template over the event)
                  <textarea
                    value={draft.webhook_body_template}
                    onChange={(e) => setDraft({ ...draft, webhook_body_template: e.target.value })}
                    rows={2}
                    placeholder='default: {"task_id":"{{.TaskID}}",…}'
                    className={`${inputClass} font-mono text-[0.8125rem]`}
                  />
                </label>
              </div>
            </div>
          </ChannelSection>

          {saveError ? (
            <p className="text-[0.75rem] text-[var(--color-danger-soft)]" role="alert">
              {saveError}
            </p>
          ) : null}

          <div className="flex justify-end">
            <button
              type="button"
              onClick={() => void save()}
              disabled={busy}
              data-testid="notify-save"
              className="rounded-full border border-[var(--color-border-strong)] px-4 py-1.5 text-[0.8125rem] font-medium transition hover:bg-[var(--color-overlay-soft)] disabled:opacity-50"
            >
              {busy ? "Saving…" : "Save notification settings"}
            </button>
          </div>
        </div>
      )}
    </section>
  );
}

// ChannelSection frames one channel with an honest enablement chip and its
// Test button (which exercises the SAVED effective config, not the unsaved
// form — the hint below the result says so when the form is dirty).
function ChannelSection({
  title,
  enabled,
  testState,
  onTest,
  busy,
  testId,
  children,
}: {
  title: string;
  enabled: boolean;
  testState: "running" | TestResult | undefined;
  onTest: () => void;
  busy: boolean;
  testId: string;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-[0.85rem] border border-[var(--color-border-subtle)] p-3">
      <div className="mb-2 flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <h3 className="text-[0.8125rem] font-medium text-[var(--color-text-primary)]">{title}</h3>
          {enabled ? (
            <StatusChip tone="success">Active</StatusChip>
          ) : (
            <StatusChip tone="neutral">Not configured</StatusChip>
          )}
        </div>
        <button
          type="button"
          onClick={onTest}
          disabled={busy || testState === "running"}
          data-testid={`notify-test-${testId}`}
          className="rounded-full border border-[var(--color-border-subtle)] px-3 py-1 text-[0.75rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-50"
        >
          {testState === "running" ? "Sending…" : "Send test"}
        </button>
      </div>
      {children}
      {testState && testState !== "running" ? (
        <p
          className={`mt-2 text-[0.75rem] ${testState.ok ? "text-[var(--color-success-soft)]" : "text-[var(--color-danger-soft)]"}`}
          data-testid={`notify-test-result-${testId}`}
        >
          {testState.ok ? "✓" : "✕"} {testState.detail}
          {testState.latency_ms > 0 ? ` (${testState.latency_ms} ms)` : ""} — tests use the saved
          config; save your changes first.
        </p>
      ) : null}
    </div>
  );
}

// SecretField — the write-only credential input: shows stored-status, starts
// empty ("leave blank to keep"), with an explicit clear toggle when a value is
// stored.
function SecretField({
  label,
  stored,
  value,
  clear,
  onChange,
  onClear,
  testId,
}: {
  label: string;
  stored: boolean;
  value: string;
  clear: boolean;
  onChange: (value: string) => void;
  onClear: (clear: boolean) => void;
  testId: string;
}) {
  return (
    <label className={labelClass}>
      <span>
        {label} {stored && !clear ? "(stored — leave blank to keep)" : ""}
      </span>
      <input
        type="password"
        autoComplete="new-password"
        value={value}
        disabled={clear}
        onChange={(e) => onChange(e.target.value)}
        placeholder={clear ? "will be cleared on save" : stored ? "••••••••" : ""}
        data-testid={testId}
        className={`${inputClass} disabled:opacity-60`}
      />
      {stored ? (
        <label className="flex items-center gap-1.5 text-[0.6875rem] text-[var(--color-text-muted)]">
          <input
            type="checkbox"
            checked={clear}
            onChange={(e) => onClear(e.target.checked)}
            data-testid={`${testId}-clear`}
          />
          Clear the stored value on save
        </label>
      ) : null}
    </label>
  );
}
