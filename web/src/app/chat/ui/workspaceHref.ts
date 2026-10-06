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

import { conversationWorkspaceUrl } from "@/app/lib/conversationApiUrl";

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
  // A pending key, or an id that fails the conversation URL gate, has no
  // workspace to point at: the href passes through unrewritten.
  const base =
    conversationId && conversationId !== PENDING_CONV_KEY
      ? conversationWorkspaceUrl(conversationId)
      : null;
  if (!base) {
    const value = typeof raw === "string" ? raw : "";
    return { href: value, isWorkspaceFile: false, downloadFilename: "" };
  }
  return resolveScopedWorkspaceHref(raw, base);
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

  // An already-resolved route — `/api/conversations/<id>/workspace/<file>` or
  // the task equivalent, ROOT-RELATIVE ONLY.
  //
  // A fully-qualified `https://host/api/…` is never withheld, and that is a
  // deliberate pair of decisions rather than an oversight.
  //
  // It is not ours to claim. The route shape is specific, but a third-party
  // page at `https://example.com/api/conversations/42/workspace/chart.png` is
  // a link the reader can simply follow, and replacing it with "file not
  // shared" would be a false statement — the exact dead promise this function
  // exists to remove.
  //
  // And the obvious refinement — withhold when the origin matches ours —
  // cannot be made deterministic here. `location` does not exist during the
  // server pass, so the same href would render as a live link on the server
  // and as plain text after hydration: a mismatch, a flash, and in the public
  // shared transcript (which is prerendered) a dead owner-scoped link visible
  // until hydration replaced it. An origin-dependent answer is worse than a
  // consistent one.
  //
  // Nothing is lost in practice: resolveWorkspaceHref only ever PRODUCES
  // root-relative routes, so an absolute self-referencing URL can only arrive
  // by a model typing one out, and the shapes that actually occur — relative
  // paths, sandbox: paths, root-relative routes — are all still caught.
  if (OWNER_SCOPED_FILE_ROUTE.test(value)) return routeBasename(value);

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
  return rewriteFileRefs(markdown, {
    image: () => imagePlaceholder,
    link: (ref) => withheldFile(ref.name),
    bare: (ref) => withheldFile(ref.name),
  });
}

// ── the team view: shared outputs become live, the rest stay locked ────────
//
// A chat shared with the team carries its OUTPUTS (docs/TEAM-SHARING.md):
// every file the agent presented in a reply, minus the ones the owner
// unchecked. A teammate fetches a shared one through the team-files route —
// never the owner-scoped workspace route.
//
// That decision is made at RENDER time, not by rewriting markdown source: the
// assistant renderer's `a` and `img` overrides (AssistantContent.tsx) hand the
// href the CommonMark parser actually produced to decideReadOnlyFile below.
// Regexes over source cannot follow CommonMark's grammar — nested brackets in
// a label (`[outer [inner]](a.csv)`), an escaped `]`, balanced parens in a
// destination (`foo(and(more)).csv`) — while the server lists outputs with a
// real parser (goldmark, outputs.go), so a source rewrite disagreed with the
// server about which references were links, and a shared output rendered
// locked. Deciding on the parsed href is the same answer the server gets.
//
// The public share link decides at render time too (every workspace reference
// is withheld there); it additionally keeps redactUnsharedFiles above as a
// belt-and-braces pre-pass, because public links never expose files.

/** The marker a locked output renders with, after its filename (B19). */
export const LOCKED_SUFFIX = " (not shared)";

/** The marker a withheld file renders with on a public link (and as before). */
export const WITHHELD_SUFFIX = NOT_SHARED_SUFFIX;

/**
 * The owner-private workspace dirs: `attachments/` (their uploads) and
 * `user-skills/` (their private skills, which fleet materializes into every
 * workspace of theirs). Nothing under either is ever an output or shared —
 * the server fences the same two (privateWorkspaceDirs in outputs.go) — so
 * the team view renders a reference into them as a plain name.
 */
const PRIVATE_WORKSPACE_DIRS = ["attachments/", "user-skills/"];

/** isPrivateWorkspacePath: under one of PRIVATE_WORKSPACE_DIRS. */
export function isPrivateWorkspacePath(path: string): boolean {
  return PRIVATE_WORKSPACE_DIRS.some((dir) => path.startsWith(dir));
}

export type SharedFileLinks = {
  /** The owner's shared outputs, by workspace-relative path (`out/report.xlsx`). */
  shared: ReadonlySet<string>;
  /** Builds the reader's download URL for a shared path (teamFileUrl). */
  fileUrl: (path: string) => string;
};

/**
 * How a read-only transcript treats the files its replies reference:
 *
 *   - `withhold` — a public link, or a team view from a server that sends no
 *     file list: every workspace reference is plain text, images become
 *     `imagePlaceholder`. Nothing is ever a link or an image.
 *   - `shared` — the team view: a reference to a path in `links.shared` is a
 *     live team-files download (images inline from it); every other workspace
 *     reference is a locked name.
 */
export type ReadOnlyFilePolicy =
  | { mode: "withhold"; imagePlaceholder: string }
  | { mode: "shared"; links: SharedFileLinks };

export type ReadOnlyFileDecision =
  /** Not a workspace reference (http(s), mailto, anchors): render as usual. */
  | { kind: "external" }
  /** A shared output: link/image at `url` (the team-files route). */
  | { kind: "shared"; name: string; path: string; url: string }
  /**
   * An upload (or the owner's private skill file) on the team view: its plain
   * name — neither is ever an output.
   */
  | { kind: "upload"; name: string; path: string }
  /** Team view, not shared: the locked name. */
  | { kind: "locked"; name: string; path: string | null }
  /** Public view: plain withheld text. */
  | { kind: "withheld"; name: string; path: string | null };

/**
 * decideReadOnlyFile decides one href the markdown parser produced (after the
 * renderer's urlTransform). Only a path in the policy's shared set can produce
 * a URL, and that URL is built from the server's own list — so a
 * prompt-injected reference can never mint a download for a file the owner
 * did not share (the server re-checks anyway). Every other workspace reference
 * — unshared, an upload, a traversal-rejected route — is never a link.
 */
export function decideReadOnlyFile(
  raw: string | undefined | null,
  policy: ReadOnlyFilePolicy,
): ReadOnlyFileDecision {
  const ref = workspaceFileRef(raw);
  if (!ref) return { kind: "external" };
  if (policy.mode === "withhold") return { kind: "withheld", ...ref };
  if (ref.path && isPrivateWorkspacePath(ref.path)) {
    return { kind: "upload", name: ref.name, path: ref.path };
  }
  if (ref.path && policy.links.shared.has(ref.path)) {
    return {
      kind: "shared",
      name: ref.name,
      path: ref.path,
      url: policy.links.fileUrl(ref.path),
    };
  }
  return { kind: "locked", ...ref };
}

/**
 * workspaceFileRef resolves an href to the workspace file it names: its
 * display name and, when the path is safe to compare, its workspace-relative
 * path (`out/chart.png`, segments percent-decoded). null for everything that
 * is not a workspace reference (http(s), mailto, data:, anchors). Same rules
 * as unsharedFileName, which is this function's `name`.
 */
export function workspaceFileRef(
  raw: string | undefined | null,
): FileRef | null {
  const value = typeof raw === "string" ? raw.trim() : "";
  if (!value) return null;
  const route = OWNER_SCOPED_FILE_ROUTE.exec(value);
  if (route) {
    const rest = value.slice(route[0].length).split(/[?#]/)[0];
    const segments = rest.split("/").filter((s) => s.length > 0);
    const traversal = segments.some((s) => isDotOrDotDot(fullyDecodeSegment(s)));
    return {
      name: routeBasename(value),
      path:
        traversal || segments.length === 0
          ? null
          : segments.map(decodeURIComponentSafe).join("/"),
    };
  }
  const scoped = resolveScopedWorkspaceHref(value, "/");
  if (!scoped.isWorkspaceFile) return null;
  return {
    name: scoped.downloadFilename,
    path: scoped.href
      .slice(1)
      .split("/")
      .map(decodeURIComponentSafe)
      .join("/"),
  };
}

// CommonMark lets any ASCII punctuation be backslash-escaped in a link
// destination (bare or <angled>, inline or in a reference definition), and the
// renderer drops the backslash before the href exists. The Go parser the
// server lists outputs with (outputs.go unescapeDest) does the same, so a raw
// destination must be unescaped HERE before it is resolved — otherwise
// `[report](my\_file.csv)` is `my_file.csv` to the server and `my\_file.csv`
// to this rewrite, and a shared file renders locked.
const MD_BACKSLASH_ESCAPE = /\\([!-/:-@[-`{-~])/g;

/** A raw markdown destination with CommonMark backslash escapes removed. */
export function unescapeMarkdownDest(dest: string): string {
  return dest.replace(MD_BACKSLASH_ESCAPE, "$1");
}

/** workspaceFileRef for a destination read out of markdown source. */
function markdownDestRef(dest: string): FileRef | null {
  return workspaceFileRef(unescapeMarkdownDest(dest));
}

/** A workspace file a transcript references. */
export type FileRef = { name: string; path: string | null };

type FileRefRenderers = {
  /** `![alt](dest)` naming a workspace file. */
  image: (ref: FileRef, alt: string) => string;
  /** `[label](dest)` (or a reference-style use) naming one. */
  link: (ref: FileRef, label: string) => string;
  /** A bare route pasted into prose. */
  bare: (ref: FileRef) => string;
};

/**
 * The renderer's pre-parse rewrites (AssistantContent.tsx), shared so a
 * transcript is rewritten in the SAME shape it is then rendered in. The last
 * one matters here: `Label: ` followed by a code span becomes a bold label and
 * a PLAIN value, so a link quoted in that span renders as a link — and must
 * be rewritten like one. Idempotent, so rendering the result re-applies it as
 * a no-op.
 */
export function normalizeAssistantMarkdown(content: string): string {
  return content
    .replace(/(^|\n)\*\*([^*\n:]+)\*\*(?=\s*$|\n)/g, "$1**$2**")
    .replace(/(^|\n)\*\*([^*\n:]+)(?=\n|$)/g, "$1$2")
    .replace(/(^|\n)([A-Za-z][A-Za-z /]+):\s*`([^`]+)`/g, "$1**$2:** $3");
}

/**
 * The public redaction's source pre-pass: finds every workspace reference in a
 * bubble's markdown (inline, reference-style, bare routes — never inside code)
 * and hands each to the caller's renderer. Reference definitions naming a
 * workspace file are dropped and their uses rendered inline, so no later pass
 * can resurrect the original destination. It may withhold MORE than renders
 * (it is a regex scan, not a CommonMark parser), never less; the renderer's
 * ReadOnlyFilesContext enforcement backs up anything it misses.
 */
function rewriteFileRefs(markdown: string, r: FileRefRenderers): string {
  if (!markdown) return markdown;

  const lines = scanFenced(normalizeAssistantMarkdown(markdown).split("\n"));
  const refs = new Map<string, FileRef>();
  // Every label's FIRST definition, workspace or not: CommonMark (and the Go
  // parser the server withholds with) resolve a label to its first
  // definition and ignore later ones, so a later `[x]: out/a.csv` under an
  // earlier `[x]: https://…` is not a workspace reference — and a later
  // `[x]: https://…` under an earlier workspace one must not take over once
  // the first is dropped. true = the first definition was a workspace file.
  const firstDef = new Map<string, boolean>();
  const defLines = new Set<number>();
  lines.forEach((line, i) => {
    if (line.code) return;
    const def = MD_REF_DEF.exec(line.text);
    if (!def) return;
    const label = normalizeRefLabel(def[1]);
    const ref = markdownDestRef(def[2] ?? def[3] ?? "");
    const seen = firstDef.get(label);
    if (seen !== undefined) {
      // A duplicate never renders. Drop it when it names a workspace file
      // (nothing to resurrect) or when the first one was dropped (so it
      // cannot become the first); an inert external duplicate under an
      // external first definition stays exactly as written.
      if (ref || seen) defLines.add(i);
      return;
    }
    firstDef.set(label, Boolean(ref));
    if (!ref) return;
    refs.set(label, ref);
    // Drop the definition itself: with it gone the usages this pass rewrites
    // cannot be resurrected by a later one, and a usage it missed renders as
    // literal bracket text rather than as a link.
    defLines.add(i);
  });

  const out: string[] = [];
  lines.forEach((line, i) => {
    if (defLines.has(i)) return;
    out.push(line.code ? line.text : redactLine(line.text, refs, r));
  });
  return out.join("\n");
}

type ScannedLine = { text: string; code: boolean };

/** Tag each line with whether it sits inside a fenced code block (or is a fence).
 *
 * A closing fence must use the same character AND be at least as long as the
 * opener — CommonMark's rule, and load-bearing here rather than pedantry:
 * documentation quotes a ``` block inside a ```` one, and comparing only the
 * marker character closed the outer block on the inner fence. Everything after
 * it was then treated as prose (so workspace-looking text inside the sample got
 * rewritten) and the real closing fence opened a phantom block (so genuine
 * links after it escaped redaction) — wrong in both directions at once.
 */
function scanFenced(lines: string[]): ScannedLine[] {
  let fence: string | null = null;
  return lines.map((text) => {
    const marker = CODE_FENCE.exec(text)?.[1];
    if (!marker) return { text, code: fence !== null };
    if (!fence) {
      fence = marker;
      return { text, code: true };
    }
    if (marker[0] === fence[0] && marker.length >= fence.length) {
      fence = null;
      return { text, code: true };
    }
    // A shorter or different-character run inside the block is content.
    return { text, code: true };
  });
}

function redactLine(
  line: string,
  refs: Map<string, FileRef>,
  r: FileRefRenderers,
): string {
  // split() on a single-group regex interleaves the separators at odd indexes,
  // so the code spans come back untouched.
  return line
    .split(INLINE_CODE)
    .map((part, i) => (i % 2 === 1 ? part : redactChunk(part, refs, r)))
    .join("");
}

function redactChunk(
  chunk: string,
  refs: Map<string, FileRef>,
  r: FileRefRenderers,
): string {
  let out = chunk;
  // Images first: an image nested in a link (`[![alt](chart.png)](chart.png)`)
  // must lose its inner destination before the link pass reads the label.
  // Each replaced image is bracketed by REDACTED_IMAGE so the link pass can
  // tell a label that now says "not shared" from one the author wrote.
  out = out.replace(MD_IMAGE, (whole, alt, angled, bare) => {
    const ref = markdownDestRef(angled ?? bare ?? "");
    return ref ? REDACTED_IMAGE + r.image(ref, alt) + REDACTED_IMAGE : whole;
  });
  out = out.replace(MD_LINK, (whole, bang, label, angled, bare) => {
    if (bang) return whole;
    const dest = angled ?? bare ?? "";
    const ref = markdownDestRef(dest);
    if (ref) return r.link(ref, label);
    // An external link around a redacted image
    // (`[![preview](private.png)](https://example.com)`): the placeholder
    // must not stay inside a live anchor, where its "not shared" text would
    // lead to an arbitrary URL. Split it — the placeholder as text, then the
    // target as its own link whose visible text is the destination.
    if (dest && label.includes(REDACTED_IMAGE)) {
      const target = angled !== undefined ? `<${angled}>` : bare;
      return `${label} [${escapeMarkdown(dest)}](${target})`;
    }
    return whole;
  });
  if (refs.size > 0) {
    out = out.replace(MD_REF_USE, (whole, bang, label, refLabel) => {
      const key = normalizeRefLabel(
        typeof refLabel === "string" && refLabel.trim() ? refLabel : label,
      );
      const ref = refs.get(key);
      if (!ref) return whole;
      return bang ? r.image(ref, label) : r.link(ref, label);
    });
  }
  return out
    .replace(BARE_FILE_REF, (whole) => {
      // Keep sentence punctuation the URL ran into out of the filename.
      const trailing = /[.,;:!?)\]]+$/.exec(whole)?.[0] ?? "";
      const core = trailing ? whole.slice(0, -trailing.length) : whole;
      const ref = workspaceFileRef(core);
      return ref ? r.bare(ref) + trailing : whole;
    })
    .split(REDACTED_IMAGE)
    .join("");
}

// Brackets a redacted image inside redactChunk only (stripped before it
// returns): a Unicode noncharacter, which no reply legitimately contains.
const REDACTED_IMAGE = "\uFDD0";

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

// The teammate download route: /api/conversations/<id>/team-files/<path>.
// Root-relative only, like OWNER_SCOPED_FILE_ROUTE and for the same reason.
const TEAM_FILE_ROUTE =
  /^\/api\/conversations\/[A-Za-z0-9_-]+\/team-files\/(?=[^/])/;

/**
 * The filename to save a team-files download as, or null when `href` is not
 * a team-files route. The assistant renderer uses it to give a shared output
 * in the teammate view the same `download` treatment an owner's workspace
 * link gets, so clicking it saves the file instead of navigating away from
 * the read-only view.
 */
export function teamFileDownloadName(href: string | undefined | null): string | null {
  const value = typeof href === "string" ? href : "";
  if (!TEAM_FILE_ROUTE.test(value)) return null;
  return routeBasename(value) || null;
}
