"use client";

// Storage panel for Settings → Admin → Server: byte accounting for the
// fleet data trees (chat attachment uploads, orchestrator temp uploads,
// per-conversation workspaces), the largest workspaces with owner/pinned
// context, and a "clean up now" action that deletes old unpinned chats
// and sweeps aged files. Pinned, archived, shared, and project chats are
// never touched — the server enforces that; the copy here just says so.

import { useEffect, useState } from "react";

import { AdminStats, ConnGroup, type AdminStat } from "../../ui/panels";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

type StorageTree = { path: string; bytes: number; files: number };

type StorageWorkspaceRow = {
  conversation_id: string;
  bytes: number;
  title?: string;
  user_email?: string;
  pinned: boolean;
  orphaned: boolean;
};

type StorageResponse = {
  disk_total_bytes: number;
  disk_available_bytes: number;
  uploads: StorageTree;
  temp_uploads: StorageTree;
  workspaces: StorageTree;
  conversations_total: number;
  conversations_pinned: number;
  conversations_protected: number;
  reclaimable_conversations: number;
  default_days: number;
  largest_workspaces: StorageWorkspaceRow[] | null;
};

type CleanupResponse = {
  deleted_conversations: number;
  removed_upload_files: number;
  removed_temp_files: number;
  removed_workspaces: number;
  bytes_freed: number;
};

// Above this share of the disk, the panel's tone flips to a warning so the
// operator notices before boxdoctor starts failing health checks (85/95%).
const CHAT_DATA_WARN_FRACTION = 0.5;

function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = value >= 100 || unit === 0 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${units[unit]}`;
}

export function StoragePanel() {
  const [data, setData] = useState<StorageResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [days, setDays] = useState(30);
  const [deleteChats, setDeleteChats] = useState(true);
  const [sweepFiles, setSweepFiles] = useState(true);
  const [cleaning, setCleaning] = useState(false);
  const [result, setResult] = useState<string | null>(null);

  const load = (isStale: () => boolean) =>
    fetch("/api/admin/storage", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`storage request failed: ${res.status}`);
        const parsed = (await res.json()) as StorageResponse;
        // Guard the shape: an older server (or a proxy error page) must
        // degrade to the error banner, not crash the whole settings page.
        if (!parsed || typeof parsed !== "object" || !parsed.uploads || !parsed.workspaces || !parsed.temp_uploads) {
          throw new Error("storage stats unavailable on this server");
        }
        return parsed;
      })
      .then((next) => {
        if (isStale()) return;
        setData(next);
        setDays((d) => (d === 30 && next.default_days > 0 ? next.default_days : d));
        setError(null);
      })
      .catch((err: unknown) => {
        if (!isStale()) setError(err instanceof Error ? err.message : "storage stats unavailable");
      });

  useEffect(() => {
    let stale = false;
    void load(() => stale);
    return () => {
      stale = true;
    };
  }, []);

  const runCleanup = async () => {
    setCleaning(true);
    setResult(null);
    try {
      const res = await fetch("/api/admin/storage/cleanup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          older_than_days: days,
          delete_conversations: deleteChats,
          sweep_files: sweepFiles,
        }),
      });
      if (!res.ok) {
        const text = await res.text().catch(() => "");
        throw new Error(text || `cleanup failed: ${res.status}`);
      }
      const out = (await res.json()) as CleanupResponse;
      setResult(
        `Removed ${out.deleted_conversations} conversation${out.deleted_conversations === 1 ? "" : "s"}, ` +
          `${out.removed_workspaces} workspace dir${out.removed_workspaces === 1 ? "" : "s"}, ` +
          `${out.removed_upload_files + out.removed_temp_files} file${out.removed_upload_files + out.removed_temp_files === 1 ? "" : "s"} — ` +
          `${formatBytes(out.bytes_freed)} freed.`,
      );
      void load(() => false);
    } catch (err) {
      setError(err instanceof Error ? err.message : "cleanup failed");
    } finally {
      setCleaning(false);
    }
  };

  const chatDataBytes = data
    ? data.uploads.bytes + data.temp_uploads.bytes + data.workspaces.bytes
    : 0;
  const heavy =
    data !== null &&
    data.disk_total_bytes > 0 &&
    chatDataBytes > data.disk_total_bytes * CHAT_DATA_WARN_FRACTION;

  const items: AdminStat[] = data
    ? [
        {
          title: "Chat data on disk",
          value: formatBytes(chatDataBytes),
          sub: `${formatBytes(data.disk_available_bytes)} free of ${formatBytes(data.disk_total_bytes)}`,
        },
        {
          title: "Attachment uploads",
          value: formatBytes(data.uploads.bytes),
          sub: `${data.uploads.files} files`,
        },
        {
          title: "Task file uploads",
          value: formatBytes(data.temp_uploads.bytes),
          sub: `${data.temp_uploads.files} files`,
        },
        {
          title: "Workspaces",
          value: formatBytes(data.workspaces.bytes),
          sub: `${data.conversations_total} conversations (${data.conversations_pinned} pinned)`,
        },
      ]
    : [];

  return (
    <ConnGroup>
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">Storage</h3>
      </div>

      {error ? (
        <NoticeBanner tone="danger" data-testid="storage-error">Storage statistics unavailable: {error}</NoticeBanner>
      ) : !data ? (
        <p className="text-sm text-[var(--color-text-muted)]" data-testid="storage-loading">Loading storage statistics…</p>
      ) : (
        <div data-testid="storage-panel">
          {heavy ? (
            <NoticeBanner tone="warning" data-testid="storage-heavy-warning">
              Chat data is using {formatBytes(chatDataBytes)} — more than half of this disk. Consider running a cleanup below.
            </NoticeBanner>
          ) : null}
          <AdminStats items={items} />

          {data.largest_workspaces && data.largest_workspaces.length > 0 ? (
            <div className="mt-5 border-t border-[var(--color-border)] pt-4">
              <h4 className="m-0 mb-2 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">Largest conversation workspaces</h4>
              <ul className="m-0 list-none p-0 text-sm">
                {data.largest_workspaces.map((w) => (
                  <li key={w.conversation_id} className="flex items-baseline justify-between gap-3 border-b border-[var(--color-border)] py-1.5 last:border-b-0">
                    <span className="min-w-0 truncate text-[var(--color-text-secondary)]">
                      {w.orphaned ? "(orphaned — reclaimed on next sweep)" : w.title || "(untitled)"}
                      {w.user_email ? <span className="text-[var(--color-text-muted)]"> · {w.user_email}</span> : null}
                      {w.pinned ? <span className="text-[var(--color-text-muted)]"> · pinned</span> : null}
                    </span>
                    <span className="shrink-0 tabular-nums text-[var(--color-text-primary)]">{formatBytes(w.bytes)}</span>
                  </li>
                ))}
              </ul>
            </div>
          ) : null}

          <div className="mt-5 border-t border-[var(--color-border)] pt-4">
            <h4 className="m-0 mb-1 text-[0.85rem] font-semibold text-[var(--color-text-primary)]">Clean up now</h4>
            <p className="m-0 mb-3 text-[0.8rem] text-[var(--color-text-muted)]">
              Reclaims disk from chats and files idle longer than the cutoff. Pinned, archived, shared, and project
              chats are never touched. A cleanup at {days} days would remove{" "}
              <span className="font-medium text-[var(--color-text-primary)]">{data.reclaimable_conversations}</span>{" "}
              conversation{data.reclaimable_conversations === 1 ? "" : "s"}.
            </p>
            <div className="flex flex-wrap items-center gap-3">
              <label className="flex items-center gap-1.5 text-[0.8rem] text-[var(--color-text-secondary)]">
                Idle more than
                <input
                  type="number"
                  min={1}
                  value={days}
                  onChange={(e) => setDays(Math.max(1, Number(e.target.value) || 1))}
                  className="w-16 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-transparent px-2 py-1 text-[0.8rem] text-[var(--color-text-primary)]"
                />
                days
              </label>
              <label className="flex items-center gap-1.5 text-[0.8rem] text-[var(--color-text-secondary)]">
                <input type="checkbox" checked={deleteChats} onChange={(e) => setDeleteChats(e.target.checked)} />
                Delete unpinned chats
              </label>
              <label className="flex items-center gap-1.5 text-[0.8rem] text-[var(--color-text-secondary)]">
                <input type="checkbox" checked={sweepFiles} onChange={(e) => setSweepFiles(e.target.checked)} />
                Sweep aged upload files
              </label>
              <button
                type="button"
                disabled={cleaning || (!deleteChats && !sweepFiles)}
                onClick={() => void runCleanup()}
                className="rounded-[var(--radius-md)] border border-[var(--color-border)] px-3 py-1.5 text-[0.8rem] text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
                data-testid="storage-cleanup-run"
              >
                {cleaning ? "Cleaning…" : "Run cleanup"}
              </button>
            </div>
            {result ? (
              <p className="m-0 mt-3 text-[0.8rem] text-[var(--color-text-secondary)]" data-testid="storage-cleanup-result" role="status">
                {result}
              </p>
            ) : null}
          </div>
        </div>
      )}
    </ConnGroup>
  );
}
