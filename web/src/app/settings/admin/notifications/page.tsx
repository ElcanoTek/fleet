"use client";

// Settings → Admin → Notifications (fleet-unified settings pass): admin-managed
// task notifications (email + outbound webhook). Previously FLEET_SMTP_*/
// FLEET_WEBHOOK_*/FLEET_NOTIFY_ON env vars + a restart; now editable here and
// hot-swapped into the running notifier — the next task completion uses the
// new config. Secrets (SMTP password, webhook signing secret) are WRITE-ONLY:
// stored encrypted, never shown again; leave the field blank to keep the
// stored value. A saved admin config replaces the env config wholesale;
// "Use env config" reverts.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import {
  ActStatus,
  btnClass,
  ConnBadge,
  InlineConfirmButton,
  SETTINGS_INPUT,
} from "../../ui/atoms";
import {
  ConnField,
  ConnFormActions,
  ConnGroup,
  ConnPanel,
  DirChip,
  SetSection,
} from "../../ui/panels";
import { useIsAdmin } from "../../useIsAdmin";
import { AdminGateFallback } from "../../AdminGateFallback";

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
  // paths, which this page itself provides (re-save or revert).
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

export default function NotificationsAdminPage() {
  // Client-side visibility gate only — every /api/admin call below is
  // independently authorized server-side regardless of what renders here.
  const admin = useIsAdmin();
  const router = useRouter();
  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);
  if (admin !== "admin") return <AdminGateFallback state={admin} />;
  return <NotificationsAdmin />;
}

function NotificationsAdmin() {
  const { data: view, loading, error, reload } = useCancellableFetch<View | null>(fetchView, []);
  // edits holds only what the admin changed this session; the rendered form is
  // edits ?? the server view, derived in-render (no seeding effect — the form
  // appears in the same render as the header, and a fresh view after
  // save/revert re-seeds by clearing edits).
  const [edits, setEdits] = useState<Draft | null>(null);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [tests, setTests] = useState<Record<string, "running" | TestResult>>({});
  // Transient "Saved" flash on the primary button after a successful save.
  const [savedFlash, setSavedFlash] = useState(false);
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(
    () => () => {
      if (flashTimer.current) clearTimeout(flashTimer.current);
    },
    [],
  );

  const draft = edits ?? (view ? draftFrom(view) : null);
  const setDraft = (d: Draft) => setEdits(d);

  const statuses = new Set(
    (draft?.notify_on ?? "")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
  );

  // Toggling rewrites the CSV through a Set, preserving any tokens beyond the
  // three chips (a future status the server understands must survive a toggle).
  const toggleStatus = (status: string) => {
    if (!draft || busy) return;
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
      setSavedFlash(true);
      if (flashTimer.current) clearTimeout(flashTimer.current);
      flashTimer.current = setTimeout(() => setSavedFlash(false), 1400);
      await reload();
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : "Failed to save.");
    } finally {
      setBusy(false);
    }
  };

  // Confirmed inline on the button itself (InlineConfirmButton arms on the
  // first click) — no window.confirm.
  const revert = async () => {
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
    <SetSection
      title="Notifications"
      intro="Task notification delivery — where task completions go. Distinct from your personal browser alerts in General."
    >
      <div data-testid="notifications-panel">
        <ConnGroup>
          <div className="mb-[0.8rem]">
            <div className="flex flex-wrap items-center gap-[0.55rem]">
              <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">
                Task notification delivery
              </h3>
              {view ? (
                view.source === "admin" ? (
                  <ConnBadge variant="overridden">Overridden</ConnBadge>
                ) : (
                  <ConnBadge variant="neutral">Env config</ConnBadge>
                )
              ) : null}
            </div>
            <p className="mb-0 mt-[0.35rem] text-[0.78rem] leading-[1.55] text-[var(--color-text-muted)]">
              An email (SMTP) and/or a signed webhook. Saving applies immediately — the next task
              completion uses this config, no restart. Secrets are stored encrypted and never
              shown again; leave a secret field blank to keep the stored value.
              {view?.source === "admin" && view.settings.updated_by ? (
                <> Last saved by {view.settings.updated_by}.</>
              ) : null}
            </p>
          </div>

          {view?.degraded ? (
            <NoticeBanner tone="warning" className="mb-4" data-testid="notify-degraded">
              {view.degraded}
            </NoticeBanner>
          ) : null}

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
          ) : loading || !view || !draft ? (
            <p className="text-[0.8rem] text-[var(--color-text-muted)]">Loading…</p>
          ) : (
            <>
              <div className="mb-4">
                <div className="text-[0.62rem] font-bold uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
                  Notify on
                </div>
                <div className="mt-2 flex flex-wrap items-center gap-[0.4rem]">
                  {STATUS_OPTIONS.map((status) => {
                    const active = statuses.has(status);
                    return (
                      <DirChip
                        key={status}
                        active={active}
                        ariaPressed={active}
                        onClick={() => toggleStatus(status)}
                        leading={
                          <Icon
                            name="check"
                            className={[
                              "h-[0.8rem] shrink-0 self-center overflow-hidden transition-all",
                              active ? "mr-[0.32rem] w-[0.8rem] opacity-100" : "w-0 opacity-0",
                            ].join(" ")}
                          />
                        }
                      >
                        {status}
                      </DirChip>
                    );
                  })}
                  <span className="ml-[0.2rem] text-[0.72rem] text-[var(--color-text-muted)]">
                    none selected = all terminal statuses
                  </span>
                </div>
              </div>

              <ChannelPanel
                title="Email (SMTP)"
                configured={view.email_enabled}
                testState={tests.email}
                onTest={() => void runTest("email")}
                busy={busy}
                dirty={edits !== null}
                testId="email"
                runningText="Sending test email…"
              >
                <div className="mt-[0.9rem] grid grid-cols-2 gap-[0.75rem_0.85rem] max-[640px]:grid-cols-1">
                  <ConnField label="SMTP host">
                    <input
                      value={draft.smtp_host}
                      onChange={(e) => setDraft({ ...draft, smtp_host: e.target.value })}
                      placeholder="smtp.example.com"
                      data-testid="notify-smtp-host"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                  <ConnField label="Port">
                    <input
                      value={draft.smtp_port}
                      onChange={(e) => setDraft({ ...draft, smtp_port: e.target.value })}
                      inputMode="numeric"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                  <ConnField label="Username (optional)">
                    <input
                      value={draft.smtp_username}
                      onChange={(e) => setDraft({ ...draft, smtp_username: e.target.value })}
                      autoComplete="off"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                  <SecretField
                    label="Password"
                    stored={view.settings.has_smtp_password}
                    value={draft.smtp_password}
                    clear={draft.clear_smtp_password}
                    onChange={(value) =>
                      setDraft({ ...draft, smtp_password: value, clear_smtp_password: false })
                    }
                    onClear={(clear) =>
                      setDraft({ ...draft, clear_smtp_password: clear, smtp_password: "" })
                    }
                    testId="notify-smtp-password"
                  />
                  <ConnField label="From address">
                    <input
                      value={draft.smtp_from}
                      onChange={(e) => setDraft({ ...draft, smtp_from: e.target.value })}
                      placeholder="fleet@example.com"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                  <ConnField label="Recipients (comma-separated)">
                    <input
                      value={draft.email_to}
                      onChange={(e) => setDraft({ ...draft, email_to: e.target.value })}
                      placeholder="ops@example.com, oncall@example.com"
                      data-testid="notify-email-to"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                </div>
              </ChannelPanel>

              <ChannelPanel
                title="Webhook"
                configured={view.webhook_enabled}
                testState={tests.webhook}
                onTest={() => void runTest("webhook")}
                busy={busy}
                dirty={edits !== null}
                testId="webhook"
                runningText="POSTing a sample event…"
              >
                <div className="mt-[0.9rem] grid grid-cols-[1fr_9rem] gap-[0.75rem_0.85rem] max-[640px]:grid-cols-1">
                  <ConnField label="URL">
                    <input
                      value={draft.webhook_url}
                      onChange={(e) => setDraft({ ...draft, webhook_url: e.target.value })}
                      placeholder="https://hooks.example.com/fleet"
                      data-testid="notify-webhook-url"
                      className={SETTINGS_INPUT}
                    />
                  </ConnField>
                  <ConnField label="Method">
                    <span className="select-wrap block">
                      <select
                        value={draft.webhook_method}
                        onChange={(e) => setDraft({ ...draft, webhook_method: e.target.value })}
                        // pr overrides the base px and needs `!` under
                        // Tailwind v4's fixed utility ordering.
                        className={`${SETTINGS_INPUT} appearance-none pr-8!`}
                      >
                        <option>POST</option>
                        <option>PUT</option>
                        <option>PATCH</option>
                      </select>
                    </span>
                  </ConnField>
                  <SecretField
                    label="Signing secret (outbound HMAC — see docs/WEBHOOK-SIGNING.md)"
                    stored={view.settings.has_webhook_secret}
                    value={draft.webhook_secret}
                    clear={draft.clear_webhook_secret}
                    onChange={(value) =>
                      setDraft({ ...draft, webhook_secret: value, clear_webhook_secret: false })
                    }
                    onClear={(clear) =>
                      setDraft({ ...draft, clear_webhook_secret: clear, webhook_secret: "" })
                    }
                    testId="notify-webhook-secret"
                    className="col-span-full"
                  />
                  <ConnField label="Body template (optional; Go text/template over the event)" full>
                    <textarea
                      value={draft.webhook_body_template}
                      onChange={(e) =>
                        setDraft({ ...draft, webhook_body_template: e.target.value })
                      }
                      placeholder={'default: {"task_id":"{{.TaskID}}",…}'}
                      // `!` on same-property overrides of SETTINGS_INPUT
                      // (Tailwind v4 orders utilities by value, not position).
                      className={`${SETTINGS_INPUT} min-h-[4.4rem]! resize-y pt-2! font-[family-name:var(--font-code)] text-[0.74rem]! leading-[1.55]`}
                    />
                  </ConnField>
                </div>
              </ChannelPanel>

              {saveError ? (
                <p className="m-0 text-[0.74rem] text-[var(--color-danger)]" role="alert">
                  {saveError}
                </p>
              ) : null}

              <ConnFormActions>
                {view.source === "admin" ? (
                  <InlineConfirmButton
                    label="Use env config"
                    confirmLabel="Confirm: discard saved settings"
                    disabled={busy}
                    onConfirm={() => void revert()}
                    testId="notify-revert"
                  />
                ) : null}
                <button
                  type="button"
                  onClick={() => void save()}
                  disabled={busy}
                  data-testid="notify-save"
                  className={btnClass({ variant: "primary" })}
                >
                  {busy ? "Saving…" : savedFlash ? "Saved" : "Save notification settings"}
                </button>
              </ConnFormActions>
            </>
          )}
        </ConnGroup>
      </div>
    </SetSection>
  );
}

// The one sentence about what Send test exercises — the button's tooltip
// while the form is dirty, and the tail of every result line.
const TEST_USES_SAVED_CONFIG = "Tests use the saved config; save your changes first.";

// ChannelPanel frames one channel with an honest configured badge (computed
// from the SAVED effective config, not the unsaved form) and its Send test
// button (which also exercises the saved config). While the form is dirty
// the button is disabled and says why, so an admin can't test stale settings
// and only learn afterwards that the edits weren't included.
function ChannelPanel({
  title,
  configured,
  testState,
  onTest,
  busy,
  dirty,
  testId,
  runningText,
  children,
}: {
  title: string;
  configured: boolean;
  testState: "running" | TestResult | undefined;
  onTest: () => void;
  busy: boolean;
  dirty: boolean;
  testId: string;
  runningText: string;
  children: ReactNode;
}) {
  return (
    <ConnPanel>
      <div className="flex min-h-[1.8rem] items-center gap-4">
        <span className="text-[0.85rem] font-semibold text-[var(--color-text-primary)]">
          {title}
        </span>
        <span className="ml-auto flex items-center gap-[0.55rem]">
          {configured ? (
            <ConnBadge variant="success">Configured</ConnBadge>
          ) : (
            <ConnBadge variant="warn">Not configured</ConnBadge>
          )}
          <button
            type="button"
            onClick={onTest}
            disabled={busy || dirty || testState === "running"}
            title={dirty ? TEST_USES_SAVED_CONFIG : undefined}
            data-testid={`notify-test-${testId}`}
            className={btnClass({ sm: true, reveal: true })}
          >
            Send test
          </button>
        </span>
      </div>
      {testState === "running" ? (
        <p className="m-0 mt-[0.55rem]">
          <ActStatus state="running">{runningText}</ActStatus>
        </p>
      ) : testState ? (
        <p className="m-0 mt-[0.55rem]" data-testid={`notify-test-result-${testId}`}>
          <ActStatus state={testState.ok ? "ok" : "err"}>
            {testState.ok ? "✓" : "✕"} {testState.detail}
            {testState.latency_ms > 0 ? ` (${testState.latency_ms} ms)` : ""} —{" "}
            {TEST_USES_SAVED_CONFIG}
          </ActStatus>
        </p>
      ) : null}
      {children}
    </ConnPanel>
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
  className,
}: {
  label: string;
  stored: boolean;
  value: string;
  clear: boolean;
  onChange: (value: string) => void;
  onClear: (clear: boolean) => void;
  testId: string;
  className?: string;
}) {
  return (
    <div className={["grid min-w-0 content-start gap-[0.3rem]", className ?? ""].join(" ")}>
      <label className="grid gap-[0.3rem]">
        <span className="text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
          {label}
          {stored && !clear ? " (stored — leave blank to keep)" : ""}
        </span>
        <input
          type="password"
          autoComplete="new-password"
          value={value}
          disabled={clear}
          onChange={(e) => onChange(e.target.value)}
          placeholder={clear ? "will be cleared on save" : stored ? "••••••••" : ""}
          data-testid={testId}
          className={`${SETTINGS_INPUT} disabled:opacity-60`}
        />
      </label>
      {stored ? (
        <label className="flex cursor-pointer items-center gap-[0.4rem] text-[0.7rem] text-[var(--color-text-muted)]">
          <input
            type="checkbox"
            checked={clear}
            onChange={(e) => onClear(e.target.checked)}
            data-testid={`${testId}-clear`}
            className="size-[0.85rem] accent-[var(--color-primary)]"
          />
          Clear the stored value on save
        </label>
      ) : null}
    </div>
  );
}
