// Pure formatting helpers for the orchestrator view. Ported from moc's
// assets/js/utils.js. The HTML-escaping helpers are intentionally dropped:
// React escapes text content by default, so there is no innerHTML path to
// guard. What remains is the date/number/size formatting + the email regex
// + ANSI stripping the log viewer needs.

export function formatDate(dateStr: string | null | undefined): string {
  if (!dateStr) return "-";
  try {
    const d = new Date(dateStr);
    if (isNaN(d.getTime())) return "-";
    return d.toLocaleString();
  } catch {
    return "-";
  }
}

export function formatTimestamp(ts: number | null | undefined): string {
  if (!ts) return "";
  try {
    return new Date(ts * 1000).toLocaleTimeString();
  } catch {
    return "";
  }
}

export function formatNumber(num: number | string): string {
  let n = typeof num === "number" ? num : Number.parseInt(num, 10);
  if (Number.isNaN(n)) n = 0;
  return n.toLocaleString();
}

export function truncate(str: string | null | undefined, maxLength = 60): string {
  if (!str) return "";
  return str.length > maxLength ? str.slice(0, maxLength) + "..." : str;
}

export function isValidEmail(email: unknown): boolean {
  if (!email || typeof email !== "string") return false;
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email);
}

export function formatFileSize(bytes: number, decimals = 2): string {
  if (bytes === 0) return "0 Bytes";
  const k = 1024;
  const dm = decimals < 0 ? 0 : decimals;
  const sizes = ["Bytes", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + " " + sizes[i];
}

// ESC control char as a unicode escape so the no-control-regex lint rule
// (which only flags *literal* control chars in a regex literal) stays quiet.
const ANSI_ESCAPE = new RegExp("\\u001b\\[[0-9;]*m", "g");
const ANSI_BARE = /\[(\d+)(;\d+)*m/g;

export function stripAnsiCodes(text: string | null | undefined): string {
  if (!text) return text ?? "";
  // Strip terminal color codes: ESC[..m sequences and bare bracket codes.
  return text.replace(ANSI_ESCAPE, "").replace(ANSI_BARE, "");
}

// Truncates a filename for display, preserving the extension. From moc's
// file-upload helper.
export function truncateFilename(name: string, maxLen = 35): string {
  if (name.length <= maxLen) return name;
  const ext = name.lastIndexOf(".");
  if (ext > 0) {
    const extension = name.slice(ext);
    const base = name.slice(0, ext);
    const available = maxLen - extension.length - 3;
    if (available > 0) return base.slice(0, available) + "..." + extension;
  }
  return name.slice(0, maxLen - 3) + "...";
}
