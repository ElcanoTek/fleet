import { t as tr } from "@/app/lib/i18n";
// A patrol on Settings → Admin → Chats (ADR-0115): pause or resume it, edit
// its instructions (compare-and-swap on the prompt's sha256, with a diff
// before saving), or change its schedule (compare-and-swap on def_version,
// previewed by the server). Every change takes a reason and is noted in the
// chat.

import { useEffect, useMemo, useState } from "react";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { formatCompactCronCadence } from "@/app/shared/lib/patrolSchedule";
import { timeZoneOptions } from "@/app/shared/lib/timezones";
import { cn } from "@/lib/utils";

import { ConnBadge, SETTINGS_INPUT, btnClass } from "../../ui/atoms";
import { ConnPanel, ConnPanelHead, ConnPanelSub } from "../../ui/panels";
import { formatRunTime, lineDiff, sendJSON, type AdminPatrol } from "./api";
import { Field, Status } from "./StepIn";

export function PatrolPanel({ patrol, onDone }: { patrol: AdminPatrol; onDone: () => void }) {
  const [editing, setEditing] = useState(false);
  const [editingSchedule, setEditingSchedule] = useState(false);
  const [draft, setDraft] = useState(patrol.prompt);
  const [reason, setReason] = useState("");
  const [status, setStatus] = useState<{ text: string; error: boolean } | null>(null);
  const diff = useMemo(() => (editing ? lineDiff(patrol.prompt, draft.trim()) : []), [editing, patrol.prompt, draft]);
  const changedLines = diff.filter((d) => d.kind !== " ").length;

  const patch = async (body: Record<string, unknown>, ok: string) => {
    try {
      await sendJSON(`/api/admin/patrols/${encodeURIComponent(patrol.id)}`, "PATCH", { ...body, reason });
      setStatus({ text: ok, error: false });
      setEditing(false);
      setEditingSchedule(false);
      onDone();
    } catch (err) {
      setStatus({ text: errorMessage(err, tr("The change failed")), error: true });
    }
  };
  const live = patrol.status === "active" || patrol.status === "paused";
  return (
    <ConnPanel>
      <ConnPanelHead title={patrol.title || tr("Untitled patrol")}>
        <ConnBadge variant={patrol.status === "active" ? "success" : "neutral"}>{patrol.status}</ConnBadge>
      </ConnPanelHead>
      <ConnPanelSub>
        {patrol.cron
          ? tr("{0} ({1}, {2}) · owner {3}", [formatCompactCronCadence(patrol.cron), patrol.cron, patrol.timezone, patrol.user_email])
          : tr("One-shot timer · owner {0}", [patrol.user_email])}
        {patrol.status === "active" && patrol.next_run_at ? tr(" · next run {0}", [formatRunTime(patrol.next_run_at, patrol.timezone)]) : ""}
      </ConnPanelSub>
      <Field label={tr("Reason")} hint={tr("recorded and shown in the chat")}>
        <input className={SETTINGS_INPUT} value={reason} onChange={(e) => setReason(e.target.value)} data-testid="admin-patrol-reason" />
      </Field>
      <div className="mt-2 flex flex-wrap gap-2">
        {live ? (
          <button
            type="button"
            className={btnClass({ sm: true, reveal: true })}
            disabled={!reason.trim()}
            onClick={() => void patch({ status: patrol.status === "active" ? "paused" : "active" }, patrol.status === "active" ? tr("Patrol paused.") : tr("Patrol resumed."))}
          >
            {patrol.status === "active" ? tr("Pause") : tr("Resume")}
          </button>
        ) : null}
        {live ? (
          <button type="button" className={btnClass({ sm: true, reveal: true })} onClick={() => setEditing((v) => !v)} aria-expanded={editing}>
            {editing ? tr("Cancel editing") : tr("Edit instructions")}
          </button>
        ) : null}
        {live && patrol.cron ? (
          <button
            type="button"
            className={btnClass({ sm: true, reveal: true })}
            onClick={() => setEditingSchedule((v) => !v)}
            aria-expanded={editingSchedule}
          >
            {editingSchedule ? tr("Cancel schedule change") : tr("Change schedule")}
          </button>
        ) : null}
      </div>
      {editingSchedule ? (
        <ScheduleEditor
          patrol={patrol}
          canSave={Boolean(reason.trim())}
          onSave={(cron, timezone) =>
            void patch({ cron, timezone, expect_def_version: patrol.def_version }, tr("Schedule changed and noted in the chat."))
          }
        />
      ) : null}
      {editing ? (
        <div className="mt-3 grid gap-2">
          <textarea
            className={cn(SETTINGS_INPUT, "min-h-[16rem] font-mono text-[0.75rem]")}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            aria-label={tr("Patrol instructions")}
            data-testid="admin-patrol-prompt"
          />
          {changedLines > 0 ? (
            <pre className="m-0 max-h-72 overflow-auto whitespace-pre-wrap rounded-md border border-[var(--color-border)] p-2 font-mono text-[0.7rem] leading-[1.5] [overflow-wrap:anywhere]" data-testid="admin-patrol-diff">
              {diff
                .map((d, i) => ({ ...d, i }))
                .filter((d, i, all) => d.kind !== " " || all.slice(Math.max(0, i - 2), i + 3).some((x) => x.kind !== " "))
                .map((d) => (
                  <div
                    key={d.i}
                    className={d.kind === "-" ? "text-[var(--color-danger)]" : d.kind === "+" ? "text-[var(--color-success)]" : "text-[var(--color-text-muted)]"}
                  >
                    {d.kind} {d.text}
                  </div>
                ))}
            </pre>
          ) : (
            <p className="m-0 text-[0.75rem] text-[var(--color-text-muted)]">{tr("No changes yet.")}</p>
          )}
          <div>
            <button
              type="button"
              className={btnClass({ variant: "primary", sm: true })}
              disabled={changedLines === 0 || !reason.trim() || !draft.trim()}
              onClick={() => void patch({ prompt: draft, expect_sha256: patrol.prompt_sha256 }, tr("Instructions updated and noted in the chat."))}
            >
              {tr("Save instructions")}
            </button>
          </div>
        </div>
      ) : null}
      {status ? <div className="mt-2"><Status tone={status.error ? "error" : "ok"} text={status.text} /></div> : null}
    </ConnPanel>
  );
}

type SchedulePreviewResponse = { changed: boolean; next_runs?: number[]; preview: AdminPatrol };

// ScheduleEditor changes a patrol's cron and timezone. Every edit is checked
// by the server (a dry run of the same change: the cron parser, the timezone,
// the minimum interval, and the compare-and-swap on def_version) and answered
// with the next runs in the patrol's timezone, so what is previewed is what
// the scheduler will do. Save is offered only for the inputs that preview is for.
function ScheduleEditor({
  patrol,
  canSave,
  onSave,
}: {
  patrol: AdminPatrol;
  canSave: boolean;
  onSave: (cron: string, timezone: string) => void;
}) {
  const [cron, setCron] = useState(patrol.cron ?? "");
  const [zone, setZone] = useState(patrol.timezone);
  const [preview, setPreview] = useState<{ cron: string; zone: string; changed: boolean; runs: number[]; timezone: string } | null>(null);
  const [previewError, setPreviewError] = useState<{ cron: string; zone: string; text: string } | null>(null);
  const zones = useMemo(() => timeZoneOptions(patrol.timezone), [patrol.timezone]);

  useEffect(() => {
    const want = { cron: cron.trim(), zone };
    if (!want.cron) return;
    let live = true;
    const timer = setTimeout(() => {
      sendJSON<SchedulePreviewResponse>(`/api/admin/patrols/${encodeURIComponent(patrol.id)}`, "PATCH", {
        cron: want.cron,
        timezone: want.zone,
        expect_def_version: patrol.def_version,
        dry_run: true,
      })
        .then((res) => {
          if (!live || !res) return;
          setPreview({ ...want, changed: res.changed, runs: res.next_runs ?? [], timezone: res.preview?.timezone || want.zone });
          setPreviewError(null);
        })
        .catch((err: unknown) => {
          if (!live) return;
          setPreview(null);
          setPreviewError({ ...want, text: errorMessage(err, tr("The schedule could not be checked")) });
        });
    }, 350);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [cron, zone, patrol.id, patrol.def_version]);

  const want = cron.trim();
  const current = preview && preview.cron === want && preview.zone === zone ? preview : null;
  const error = !want
    ? tr("Enter a cron expression.")
    : previewError && previewError.cron === want && previewError.zone === zone
      ? previewError.text
      : null;
  return (
    <div className="mt-3 grid gap-2" data-testid="admin-patrol-schedule">
      <div className="grid gap-2 sm:grid-cols-2">
        <Field label={tr("Schedule (cron: minute hour day month weekday)")}>
          <input
            className={cn(SETTINGS_INPUT, "font-mono")}
            value={cron}
            onChange={(e) => setCron(e.target.value)}
            spellCheck={false}
            data-testid="admin-patrol-cron"
          />
        </Field>
        <Field label={tr("Timezone")}>
          <select className={SETTINGS_INPUT} value={zone} onChange={(e) => setZone(e.target.value)} data-testid="admin-patrol-timezone">
            {zones.map((z) => (
              <option key={z} value={z}>
                {z}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <p className="m-0 text-[0.75rem] text-[var(--color-text-muted)]">
        {tr("Now: {0} ({1}, {2})", [formatCompactCronCadence(patrol.cron ?? ""), patrol.cron ?? "", patrol.timezone])}
      </p>
      {error ? (
        <Status tone="error" text={error} />
      ) : current ? (
        <div data-testid="admin-patrol-schedule-preview">
          <p className="m-0 text-[0.75rem] text-[var(--color-text-secondary)]">
            {current.changed
              ? tr("New: {0} ({1}, {2}). Next runs:", [formatCompactCronCadence(want), want, current.timezone])
              : tr("No changes yet. Next runs:")}
          </p>
          <ul className="m-0 mt-1 list-none p-0 font-mono text-[0.72rem] text-[var(--color-text-secondary)]">
            {current.runs.map((r) => (
              <li key={r}>{formatRunTime(r, current.timezone)}</li>
            ))}
          </ul>
          {patrol.status !== "active" ? (
            <p className="m-0 mt-1 text-[0.72rem] text-[var(--color-text-muted)]">{tr("This patrol is paused: it runs on the new schedule once resumed.")}</p>
          ) : null}
        </div>
      ) : (
        <p className="m-0 text-[0.75rem] text-[var(--color-text-muted)]">{tr("Checking the schedule…")}</p>
      )}
      <div>
        <button
          type="button"
          className={btnClass({ variant: "primary", sm: true })}
          disabled={!current?.changed || Boolean(error) || !canSave}
          onClick={() => onSave(want, zone)}
        >
          {tr("Save schedule")}
        </button>
      </div>
    </div>
  );
}
