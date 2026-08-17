// Pure helpers for rewriting agent-emitted relative paths in markdown
// to a per-run workspace file API. Used by both the <img> and <a>
// interceptors in the chat transcript (chat-experience.tsx) AND the
// orchestrator task-log viewer (LogViewer.tsx), and exported separately
// so vitest can exercise the rewrite logic without booting React.
//
// Two callers, two workspace endpoints, ONE safety policy:
//   - chat reads from   /api/conversations/<convID>/workspace/<path>
//   - the task-log view reads from
//                       /api/orchestrator/tasks/<taskID>/workspace/<path>
// Both are authenticated, origin-local proxies scoped to a single run's
// workspace dir. The rewrite ONLY targets relative paths the agent
// emitted (a file it actually wrote into its own workspace); every
// absolute http(s)/data/mailto/protocol-relative/site-root href, and
// any href whose decoded path contains a `.` or `..` segment (including
// `%2e%2e` / `%252e%252e`), passes through untouched, so neither caller
// can be coaxed into fetching an arbitrary same-origin or remote URL
// (no SSRF / tracking-pixel / authenticated-GET vector — see #271, #1113).

// Sentinel for messages that belong to a brand-new chat whose server
// id we haven't received yet. Mirrors the constant in chat-experience.tsx.
export const PENDING_CONV_KEY = "__pending__";

export type WorkspaceHref = {
  /** The href to put on the <a>/<img>. Empty string if the raw value was empty. */
  href: string;
  /** True when the raw href was a relative path and we rewrote it to the workspace API. */
  isWorkspaceFile: boolean;
  /**
   * Basename of the original relative path, suitable for the <a download>
   * attribute. Empty for non-workspace hrefs. Passing this explicitly
   * (rather than relying on the browser to derive a name from the
   * percent-encoded URL) gives users a predictable saved filename
   * regardless of OS / browser URL-decoding quirks.
   */
  downloadFilename: string;
};

/**
 * resolveWorkspaceHref rewrites a relative href like `report.pptx` or
 * `out/chart.png` to `/api/conversations/<id>/workspace/<path>` so the
 * browser fetches it through the authenticated proxy that streams from
 * the conversation's workspace dir.
 *
 * Absolute http(s)/data/mailto URLs, protocol-relative `//`, site-root
 * paths, in-page `#anchor` / `?query` references, and any path with a
 * `.` / `..` segment pass through unchanged. The conversation id is
 * required and must not be the pending sentinel (we don't yet know the
 * real id at that point).
 */
export function resolveWorkspaceHref(
  raw: string | undefined | null,
  conversationId: string | null,
): WorkspaceHref {
  if (!conversationId || conversationId === PENDING_CONV_KEY) {
    const value = typeof raw === "string" ? raw : "";
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }
  return resolveScopedWorkspaceHref(
    raw,
    `/api/conversations/${encodeURIComponent(conversationId)}/workspace/`,
  );
}

/**
 * resolveTaskWorkspaceHref is the scheduled-task counterpart of
 * resolveWorkspaceHref (#271). It rewrites a relative href the agent
 * emitted in a task-log message (e.g. `![chart](weekly.png)` produced by
 * the generate_image tool) to the task's workspace file proxy
 * `/api/orchestrator/tasks/<taskID>/workspace/<path>`, which streams the
 * file from the task's own per-run workspace dir.
 *
 * It shares the EXACT safety rules of the chat path: only relative paths
 * are rewritten; absolute http(s)/data/mailto/protocol-relative/site-root
 * hrefs and any `.` / `..` segment pass through unchanged, so a task log
 * can never make the browser fetch an arbitrary remote or same-origin URL.
 */
export function resolveTaskWorkspaceHref(
  raw: string | undefined | null,
  taskId: string | null,
): WorkspaceHref {
  if (!taskId) {
    const value = typeof raw === "string" ? raw : "";
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }
  return resolveScopedWorkspaceHref(
    raw,
    `/api/orchestrator/tasks/${encodeURIComponent(taskId)}/workspace/`,
  );
}

function decodeURIComponentSafe(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/**
 * Fully decode a path segment so a single pass cannot miss `%252e%252e`
 * (double-encoded `..`). Bounded so a pathological `%25%25…` chain cannot
 * loop; five rounds is well past anything a markdown href would carry.
 */
function fullyDecodeSegment(segment: string): string {
  let current = segment;
  for (let i = 0; i < 5; i++) {
    const next = decodeURIComponentSafe(current);
    if (next === current) return current;
    current = next;
  }
  return current;
}

function isDotOrDotDot(segment: string): boolean {
  return segment === "." || segment === "..";
}

/**
 * resolveScopedWorkspaceHref is the shared core: it applies the
 * sandbox-prefix stripping, the absolute-URL bailout, the `.`/`..`
 * traversal reject (including encoded forms), and the per-segment
 * percent-encoding, then joins the surviving relative path onto
 * `basePath` (which must already be a trailing-slash workspace API
 * prefix). Keeping the policy in one place is what guarantees the chat
 * and task-log callers can never drift apart on what counts as a "safe,
 * workspace-local" reference.
 */
function resolveScopedWorkspaceHref(
  raw: string | undefined | null,
  basePath: string,
): WorkspaceHref {
  const value = typeof raw === "string" ? raw : "";
  if (!value) return { href: "", isWorkspaceFile: false, downloadFilename: "" };

  // Some models (notably ChatGPT-style ones) hallucinate links that leak
  // the sandbox's view of the workspace — e.g. `sandbox:/opt/chat/workspace/
  // <convId>/file.xlsx` or just `/opt/chat/workspace/<convId>/file.xlsx`.
  // The container mounts the workspace at the same absolute path on host
  // and inside the sandbox (see server/internal/sandbox/container.go), so
  // the model legitimately sees that prefix and parrots it into markdown.
  // Strip the scheme and the workspace prefix (with or without UUID dir)
  // before the absolute-URL bailout below so those links resolve.
  const normalized = value
    .replace(/^sandbox:\/*/i, "")
    .replace(
      /^\/?opt\/chat\/workspace\/(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/)?/i,
      "",
    );

  if (
    /^[a-z][a-z0-9+.-]*:/i.test(normalized) ||
    normalized.startsWith("//") ||
    normalized.startsWith("/") ||
    normalized.startsWith("#") ||
    normalized.startsWith("?")
  ) {
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }

  const rawSegments = normalized.split("/").filter((s) => s.length > 0);
  if (rawSegments.length === 0) {
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }
  // Reject path traversal. encodeURIComponent leaves "." / ".." untouched,
  // so a prompt-injected `[x](../../auth/elcano-login)` would rewrite to
  // `/api/conversations/<id>/workspace/../../auth/elcano-login` and the
  // browser would normalize that into an authenticated same-origin GET
  // at `/api/auth/elcano-login` (#1113). Check the fully-decoded form so
  // `%2e%2e` and `%252e%252e` cannot sneak through a single decode.
  if (rawSegments.some((s) => isDotOrDotDot(fullyDecodeSegment(s)))) {
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }
  // Decode each segment before re-encoding so the encoding is idempotent.
  // Models routinely hand us a filename whose spaces / unicode are ALREADY
  // percent-encoded — both the markdown-link convention (`[x](My%20File.csv)`)
  // and the basename parroted out of a `sandbox:/opt/chat/workspace/<id>/...`
  // path arrive pre-encoded. Blindly re-encoding turns `%20` into `%2520`, so
  // the fetch 404s on a file that exists (this is exactly what broke the
  // "download link doesn't work" reports for filenames with spaces). A raw
  // space and an encoded `%20` now converge on the same single-encoded
  // segment. A stray literal `%` that decodeURIComponent rejects falls back
  // to the raw segment.
  const decodedSegments = rawSegments.map(decodeURIComponentSafe);
  const segments = decodedSegments.map((s) => encodeURIComponent(s)).join("/");
  const downloadFilename = decodedSegments[decodedSegments.length - 1];

  return {
    href: `${basePath}${segments}`,
    isWorkspaceFile: true,
    downloadFilename,
  };
}
