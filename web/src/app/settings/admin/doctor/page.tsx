"use client";

// Settings → Admin → Doctor — the box-health report. Renders the read-only
// /api/admin/doctor checks (DBs, disk headroom, rootless-podman prerequisites,
// sandbox image, systemd units) with the on-box repair command next to every
// warn/fail. The deep run (launches a throwaway sandbox container — the
// definitive smoke, but slow) sits behind an explicit button; page load only
// runs the quick checks. Repairs never happen from the browser: the fix is
// always a command the operator runs on the box (`sudo fleet doctor`).

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";

import { useIsAdmin } from "../../useIsAdmin";
import { ConnGroup, SetSection } from "../../ui/panels";
import { ConnBadge } from "../../ui/atoms";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Icon } from "@/app/shared/ui/Icon";

type DoctorStatus = "ok" | "warn" | "fail" | "skip";

type DoctorCheck = {
  name: string;
  status: DoctorStatus;
  detail: string;
  fix?: string;
};

type DoctorReport = {
  generated_at: string;
  duration_ms: number;
  deep: boolean;
  healthy: boolean;
  summary: { ok: number; warn: number; fail: number; skip: number };
  checks: DoctorCheck[];
};

const STATUS_META: Record<DoctorStatus, { icon: string; className: string; label: string }> = {
  ok: { icon: "check", className: "text-[var(--color-success)]", label: "ok" },
  warn: { icon: "warning", className: "text-[var(--color-warning)]", label: "warning" },
  fail: { icon: "warning", className: "text-[var(--color-danger)]", label: "failing" },
  skip: { icon: "info", className: "text-[var(--color-text-muted)]", label: "skipped" },
};

export default function AdminDoctorPage() {
  const router = useRouter();
  const admin = useIsAdmin();
  const [report, setReport] = useState<DoctorReport | null>(null);
  const [error, setError] = useState<string | null>(null);
  // "quick" initially: the mount effect starts the first quick run, and
  // seeding the state here (instead of setState inside the effect) keeps the
  // buttons disabled from the first paint.
  const [running, setRunning] = useState<"quick" | "deep" | null>("quick");
  const staleRef = useRef(false);

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  const runFetch = useCallback((mode: "quick" | "deep") => {
    fetch(mode === "deep" ? "/api/admin/doctor?deep=1" : "/api/admin/doctor", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`doctor request failed: ${res.status}`);
        return (await res.json()) as DoctorReport;
      })
      .then((next) => {
        if (staleRef.current) return;
        setReport(next);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!staleRef.current) setError(err instanceof Error ? err.message : "doctor report unavailable");
      })
      .finally(() => {
        if (!staleRef.current) setRunning(null);
      });
  }, []);

  const run = useCallback(
    (mode: "quick" | "deep") => {
      setRunning(mode);
      runFetch(mode);
    },
    [runFetch],
  );

  useEffect(() => {
    if (admin !== "admin") return;
    staleRef.current = false;
    runFetch("quick");
    return () => {
      staleRef.current = true;
    };
  }, [admin, runFetch]);

  if (admin !== "admin") return null;

  const attention = report ? report.checks.filter((c) => c.status === "fail" || c.status === "warn") : [];

  return (
    <SetSection
      title="Doctor"
      intro="Read-only box-health checks run from inside the Fleet process: databases, disk headroom, rootless-podman prerequisites, the sandbox image, and the systemd units. Repairs run on the box, not from here."
    >
      <ConnGroup>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">Box health</h3>
          {report ? (
            <ConnBadge variant={report.healthy ? "success" : "warn"} title={`${report.summary.fail} failing / ${report.summary.warn} warnings`}>
              {report.healthy ? "healthy" : `${report.summary.fail} failing`}
            </ConnBadge>
          ) : null}
          {report ? (
            <span className="text-xs text-[var(--color-text-muted)]">
              {report.deep ? "deep run" : "quick run"} · {(report.duration_ms / 1000).toFixed(1)}s
            </span>
          ) : null}
          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              disabled={running !== null}
              onClick={() => run("quick")}
              aria-label="Re-run checks"
              className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-border)] px-2.5 py-1.5 text-xs font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
            >
              <Icon name="refresh" className={`size-[0.85rem]${running === "quick" ? " animate-spin" : ""}`} />
              Re-run checks
            </button>
            <button
              type="button"
              disabled={running !== null}
              onClick={() => run("deep")}
              data-testid="doctor-run-deep"
              title="Also launches a throwaway sandbox container — the definitive smoke, but it can take a minute or two."
              className="inline-flex items-center gap-1.5 rounded-[var(--radius-md)] border border-[var(--color-border)] px-2.5 py-1.5 text-xs font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
            >
              <Icon name="activity" className={`size-[0.85rem]${running === "deep" ? " animate-pulse" : ""}`} />
              {running === "deep" ? "Running deep checks…" : "Run deep checks"}
            </button>
          </div>
        </div>

        {error ? (
          <NoticeBanner tone="danger" data-testid="doctor-error">Doctor report unavailable: {error}</NoticeBanner>
        ) : !report ? (
          <p className="text-sm text-[var(--color-text-muted)]" data-testid="doctor-loading">Running box checks…</p>
        ) : (
          <div data-testid="doctor-panel">
            {attention.length > 0 ? (
              <NoticeBanner tone={report.healthy ? "warning" : "danger"} data-testid="doctor-attention">
                {report.summary.fail > 0
                  ? `${report.summary.fail} check(s) failing — run “sudo fleet doctor” on the box to repair.`
                  : `${report.summary.warn} warning(s) — review below.`}
              </NoticeBanner>
            ) : null}
            <ul className="m-0 mt-2 grid list-none gap-0 p-0">
              {report.checks.map((c) => {
                const meta = STATUS_META[c.status] ?? STATUS_META.skip;
                return (
                  <li
                    key={c.name}
                    className="flex items-start gap-2.5 border-b border-[var(--color-border)] py-2.5 last:border-b-0"
                  >
                    <Icon name={meta.icon} className={`mt-0.5 size-[0.95rem] shrink-0 ${meta.className}`} />
                    <div className="min-w-0">
                      <div className="flex flex-wrap items-baseline gap-x-2">
                        <span className="text-sm font-medium text-[var(--color-text-primary)]">{c.name}</span>
                        <span className={`text-[0.68rem] font-semibold uppercase tracking-[0.05em] ${meta.className}`}>{meta.label}</span>
                      </div>
                      <p className="m-0 mt-0.5 break-words text-[0.8rem] text-[var(--color-text-secondary)]">{c.detail}</p>
                      {c.fix ? (
                        <p className="m-0 mt-1 text-[0.75rem] text-[var(--color-text-muted)]">
                          Fix:{" "}
                          <code className="rounded bg-[var(--color-surface-2)] px-1.5 py-0.5 font-mono text-[0.72rem] text-[var(--color-text-primary)]">
                            {c.fix}
                          </code>
                        </p>
                      ) : null}
                    </div>
                  </li>
                );
              })}
            </ul>
            <p className="m-0 mt-3 text-xs text-[var(--color-text-muted)]">
              This report is read-only. To diagnose <em>and repair</em> box-level drift (packages, unit drift, podman
              prerequisites), run{" "}
              <code className="rounded bg-[var(--color-surface-2)] px-1.5 py-0.5 font-mono text-[0.72rem]">sudo fleet doctor</code>{" "}
              on the box.
            </p>
          </div>
        )}
      </ConnGroup>
    </SetSection>
  );
}
