// Client-side attachment size screening.
//
// The server enforces a per-file cap on /attachments (FLEET_UPLOAD_MAX_BYTES,
// default 1 GiB) and reports it via /server-config as `upload_max_bytes`.
// Before this helper existed, an oversize file sat silently in the composer
// until Send, burned a full upload round-trip, and surfaced only as a raw
// 413 body in tiny inline text — the user saw a grayed-out button and no
// explanation. Screening at pick time turns that into an immediate,
// readable message.
//
// The helpers are pure. The composer wires the results to the attachment
// error line and the large-upload banner; tests assert the logic without
// touching the DOM.

// Mirrors the server-side default in server/internal/config/config.go.
// Used only until /server-config responds (or when talking to an older
// server that doesn't advertise the limit).
export const DEFAULT_UPLOAD_MAX_BYTES = 1 << 30; // 1 GiB

// Above this total, the composer shows a "large upload" banner: still
// allowed, but worth telling the user the send will take a while. 200 MB
// is roughly a minute on a fast home uplink and several on a slow one.
export const LARGE_UPLOAD_WARNING_BYTES = 200 * 1024 * 1024;

export type FileSizeLike = {
  name: string;
  size: number;
};

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

export type ScreenedFiles<T extends FileSizeLike> = {
  accepted: T[];
  rejected: T[];
  // Human-readable explanation for the rejected set, null when all fit.
  error: string | null;
};

// screenFilesForUpload partitions picked files against the server's
// per-file cap. Files that fit are still attached — one oversize file in
// a multi-select shouldn't discard its siblings.
export function screenFilesForUpload<T extends FileSizeLike>(
  files: readonly T[],
  maxBytes: number,
): ScreenedFiles<T> {
  const accepted: T[] = [];
  const rejected: T[] = [];
  for (const f of files) {
    (f.size > maxBytes ? rejected : accepted).push(f);
  }
  let error: string | null = null;
  if (rejected.length === 1) {
    const f = rejected[0];
    error = `"${f.name}" is ${formatBytes(f.size)} — over this server's ${formatBytes(maxBytes)} per-file upload limit, so it was not attached.`;
  } else if (rejected.length > 1) {
    const names = rejected.map((f) => `"${f.name}"`).join(", ");
    error = `${rejected.length} files are over this server's ${formatBytes(maxBytes)} per-file upload limit and were not attached: ${names}.`;
  }
  return { accepted, rejected, error };
}

// largeUploadWarning returns banner copy when the queued total is big
// enough that the user should expect a slow send, null otherwise.
export function largeUploadWarning(totalBytes: number): string | null {
  if (totalBytes < LARGE_UPLOAD_WARNING_BYTES) return null;
  return `Large upload (${formatBytes(totalBytes)}) — sending may take a while, especially on a slow connection. Keep this tab open until the upload finishes.`;
}
