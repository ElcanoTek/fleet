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

// ── the inverse question: files a read-only reader cannot fetch ────────────
//
// Everything above rewrites an agent-emitted path INTO an owner-scoped
// workspace route. The two read-only transcript views need the opposite
// answer — "does this href promise a file only the owner can fetch?" —
// because neither of their readers can fetch one. A team share and a public
// share link both expose the TRANSCRIPT ONLY: attachments and generated files
// stay behind the owner-scoped workspace route (docs/TEAM-SHARING.md, #226).
// So a live <a> or <img> pointing at one is a guaranteed 404 — a dead promise
// dressed as a download — and the honest render is plain text saying the file
// was not part of what was shared.

/**
 * The owner-scoped file routes: the authenticated endpoints that stream bytes
 * out of ONE user's per-run workspace dir. Uploaded attachments have no GET
 * route today (`/api/attachments` is POST-only); if one lands, add it here.
 */
const OWNER_SCOPED_FILE_ROUTE =
  /^\/api\/(?:conversations|orchestrator\/tasks)\/[^/]+\/workspace\/(?=[^/])/i;

/** The marker a withheld file reference renders as, after its filename. */
const NOT_SHARED_SUFFIX = " (file not shared)";

/**
 * unsharedFileName returns the filename an href promises when that href points
 * at owner-scoped file content, and null for everything a read-only reader CAN
 * still follow (http(s) links, mailto, data URIs, in-page anchors). Three
 * shapes count, because all three reach a reader who can resolve none of them:
 *
 *   - a relative path the agent emitted (`chart.png`, `out/spend.png`) — what
 *     resolveWorkspaceHref would have rewritten to the workspace API;
 *   - a hallucinated sandbox path (`sandbox:/opt/chat/workspace/<id>/x.png`);
 *   - an already-resolved workspace route, root-relative or absolute.
 *
 * The first two are decided by resolveScopedWorkspaceHref itself, so the
 * rewrite and the withholding can never disagree about what counts as a
 * "workspace-local" reference — including its traversal rejects, which keep a
 * prompt-injected `../../auth/x` out of BOTH directions.
 */
export function unsharedFileName(raw: string | undefined | null): string | null {
  const value = typeof raw === "string" ? raw.trim() : "";
  if (!value) return null;

  // An already-resolved route, either `/api/…` or `https://host/api/…`. Both
  // conversation- and task-workspace prefixes carry an id segment plus
  // `/workspace/`, which is specific enough that matching it on an absolute
  // URL cannot swallow an unrelated third-party link.
  let path = value;
  if (/^https?:\/\//i.test(value)) {
    try {
      path = new URL(value).pathname;
    } catch {
      return null;
    }
  }
  if (OWNER_SCOPED_FILE_ROUTE.test(path)) return routeBasename(path);

  const scoped = resolveScopedWorkspaceHref(value, "/");
  return scoped.isWorkspaceFile ? scoped.downloadFilename : null;
}

function routeBasename(path: string): string {
  const segments = path.split(/[?#]/)[0].split("/").filter((s) => s.length > 0);
  return decodeURIComponentSafe(segments[segments.length - 1] ?? "");
}

// Markdown destination + optional title, shared by the image and link forms so
// the two cannot drift on what they accept: an angle-bracketed destination or
// a bare run that may carry balanced parens, captured as two adjacent groups
// (only one of them ever matches — each replacer takes `angled ?? bare`).
const MD_DEST = "(?:<([^<>\\n]*)>|((?:[^\\s()\\\\]|\\\\.|\\([^\\s()]*\\))+))?";
const MD_TITLE = "(?:\\s+(?:\"[^\"]*\"|'[^']*'|\\([^()]*\\)))?";
const MD_IMAGE = new RegExp(
  `!\\[([^\\]]*)\\]\\(\\s*${MD_DEST}${MD_TITLE}\\s*\\)`,
  "g",
);
// Same shape with the `!` captured rather than excluded by a lookbehind (not
// every browser we serve parses one), so the link pass can skip an image the
// pass above deliberately left alone: an external `![alt](https://…)` still
// renders as a picture.
const MD_LINK = new RegExp(
  `(!?)\\[([^\\]]*)\\]\\(\\s*${MD_DEST}${MD_TITLE}\\s*\\)`,
  "g",
);
// Reference form — `[label][ref]`, collapsed `[ref][]`, shortcut `[ref]` —
// paired with its `[ref]: dest` definition line. `(?!\()` keeps it off an
// inline link the passes above left alone.
const MD_REF_USE = /(!?)\[([^\]]*)\](?:\[([^\]]*)\])?(?!\()/g;
const MD_REF_DEF = /^\s{0,3}\[([^\]]+)\]:\s*(?:<([^<>\n]*)>|(\S+))/;
// A bare route pasted into prose: GFM autolinks the absolute form, and even
// the root-relative one reads as a fetchable path. Deliberately narrow — a
// bare filename in prose ("I saved spend.png") is prose, not a promise, and
// stays exactly as the owner wrote it.
const BARE_FILE_REF = new RegExp(
  "(?:https?://[^\\s<>()]+)?/api/(?:conversations|orchestrator/tasks)/[^\\s<>()/]+/workspace/[^\\s<>()]+" +
    "|(?:sandbox:)?/?opt/chat/workspace/[^\\s<>()]+",
  "gi",
);
const CODE_FENCE = /^\s{0,3}(`{3,}|~{3,})/;
const INLINE_CODE = /(`+[^`]*`+)/;

/**
 * redactUnsharedFiles rewrites the markdown of one transcript bubble so that
 * every reference to an owner-scoped file renders as PLAIN TEXT — no anchor
 * element at all, because a disabled link is still a dead promise — carrying
 * the filename and a `(file not shared)` marker. Image references become
 * `imagePlaceholder`, which each read-only view words for the reader it
 * actually has; nothing is fetched, so the reader never sees a load error for
 * something that was never shared.
 *
 * Fenced blocks and inline code pass through verbatim: a path inside a code
 * block is quoted source, not a link, and mangling it would corrupt what the
 * owner wrote. Anything this cannot parse is left alone, which fails toward
 * "unchanged markdown" rather than toward mangled prose — a reference it
 * misses is a 404 the reader was already getting, not a new leak: the files
 * themselves stay behind the owner-scoped route regardless of what the
 * transcript says about them.
 */
export function redactUnsharedFiles(
  markdown: string,
  imagePlaceholder: string,
): string {
  if (!markdown) return markdown;

  const lines = scanFenced(markdown.split("\n"));
  const refs = new Map<string, string>();
  const defLines = new Set<number>();
  lines.forEach((line, i) => {
    if (line.code) return;
    const def = MD_REF_DEF.exec(line.text);
    if (!def) return;
    const name = unsharedFileName(def[2] ?? def[3] ?? "");
    if (!name) return;
    refs.set(normalizeRefLabel(def[1]), name);
    // Drop the definition itself: with it gone the usages this pass rewrites
    // cannot be resurrected by a later one, and a usage it missed renders as
    // literal bracket text rather than as a link.
    defLines.add(i);
  });

  const out: string[] = [];
  lines.forEach((line, i) => {
    if (defLines.has(i)) return;
    out.push(
      line.code ? line.text : redactLine(line.text, refs, imagePlaceholder),
    );
  });
  return out.join("\n");
}

type ScannedLine = { text: string; code: boolean };

/** Tag each line with whether it sits inside a fenced code block (or is a fence). */
function scanFenced(lines: string[]): ScannedLine[] {
  let fence: string | null = null;
  return lines.map((text) => {
    const marker = CODE_FENCE.exec(text)?.[1];
    if (marker && (!fence || marker[0] === fence[0])) {
      fence = fence ? null : marker;
      return { text, code: true };
    }
    return { text, code: fence !== null };
  });
}

function redactLine(
  line: string,
  refs: Map<string, string>,
  imagePlaceholder: string,
): string {
  // split() on a single-group regex interleaves the separators at odd indexes,
  // so the code spans come back untouched.
  return line
    .split(INLINE_CODE)
    .map((part, i) =>
      i % 2 === 1 ? part : redactChunk(part, refs, imagePlaceholder),
    )
    .join("");
}

function redactChunk(
  chunk: string,
  refs: Map<string, string>,
  imagePlaceholder: string,
): string {
  // Images first: an image nested in a link (`[![alt](chart.png)](chart.png)`)
  // must lose its inner destination before the link pass reads the label.
  let out = chunk.replace(MD_IMAGE, (whole, _alt, angled, bare) =>
    unsharedFileName(angled ?? bare ?? "") ? imagePlaceholder : whole,
  );
  out = out.replace(MD_LINK, (whole, bang, _label, angled, bare) => {
    if (bang) return whole;
    const name = unsharedFileName(angled ?? bare ?? "");
    return name ? withheldFile(name) : whole;
  });
  if (refs.size > 0) {
    out = out.replace(MD_REF_USE, (whole, bang, label, ref) => {
      const key = normalizeRefLabel(
        typeof ref === "string" && ref.trim() ? ref : label,
      );
      const name = refs.get(key);
      if (!name) return whole;
      return bang ? imagePlaceholder : withheldFile(name);
    });
  }
  return out.replace(BARE_FILE_REF, (whole) => {
    // Keep sentence punctuation the URL ran into out of the filename.
    const trailing = /[.,;:!?)\]]+$/.exec(whole)?.[0] ?? "";
    const core = trailing ? whole.slice(0, -trailing.length) : whole;
    const name = unsharedFileName(core);
    return name ? withheldFile(name) + trailing : whole;
  });
}

/** `daily_spend.png (file not shared)`, escaped so it re-parses as plain text. */
function withheldFile(filename: string): string {
  return escapeMarkdown(filename) + NOT_SHARED_SUFFIX;
}

// Any ASCII punctuation may be backslash-escaped in CommonMark, and a filename
// is full of characters markdown would otherwise read as syntax (`_`, `*`,
// `[`). Escaping them keeps the marker literal text.
function escapeMarkdown(text: string): string {
  return text.replace(/[\\`*_{}[\]<>()#+\-.!|~]/g, "\\$&");
}

function normalizeRefLabel(label: string): string {
  return label.trim().replace(/\s+/g, " ").toLowerCase();
}
