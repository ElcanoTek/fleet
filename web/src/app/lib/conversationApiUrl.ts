// Conversation API URLs. Conversation ids (store.CreateConversation) and
// approval ids (store.CreateApproval) are server-minted UUIDs
// (uuid.NewString). Encoding each id as a single path segment AND refusing
// anything that is not a single path-safe token closes client-side request
// forgery: a hostile id cannot rewrite the path (`../`, extra segments,
// query/hash) or target a different origin.
//
// Allowed: letters, digits, underscore, hyphen (covers UUIDs). No `.` so
// `..` cannot traverse; no `/`, `:`, `?`, `#` or `%` (so a pre-encoded
// `%2e%2e` or `%2f` cannot smuggle one in either).

const apiIdPattern = /^[A-Za-z0-9_-]+$/;

// isApiId reports whether `id` is safe to place in an API path as one
// segment. It is the shared gate for every server-minted id the chat UI
// interpolates into a same-origin request.
export function isApiId(id: string): boolean {
  return id.length > 0 && apiIdPattern.test(id);
}

export function isConversationId(id: string): boolean {
  return isApiId(id);
}

// conversationApiUrl returns `/api/conversations/<id>` plus an optional
// suffix (`/inflight`, `/stream?turn_id=…`). Returns null when `id` is not
// a single path-safe token, so callers skip the request rather than
// interpolating untrusted text into a URL. `suffix` must be a literal path
// (plus any query built with encodeURIComponent) — never a raw id; nested
// ids get their own builder below so they pass the same gate.
export function conversationApiUrl(id: string, suffix = ""): string | null {
  if (!isConversationId(id)) return null;
  return `/api/conversations/${encodeURIComponent(id)}${suffix}`;
}

// conversationApiPath returns `/api/conversations/<id>/<seg>/<seg>…` for a
// path with further ids in it (an approval, a queued input, a turn, a
// subagent child). Every segment — literal or id — must pass the same
// single-token gate and is encoded on its own, so no value can add a
// segment, climb with `..`, or start a query. Returns null when any segment
// fails. Callers append a query built with URLSearchParams/encodeURIComponent.
export function conversationApiPath(
  conversationId: string,
  ...segments: string[]
): string | null {
  if (!isConversationId(conversationId)) return null;
  if (!segments.every(isApiId)) return null;
  const tail = segments.map((segment) => `/${encodeURIComponent(segment)}`).join("");
  return `/api/conversations/${encodeURIComponent(conversationId)}${tail}`;
}

// conversationApprovalApiUrl returns
// `/api/conversations/<id>/approvals/<approvalId>`, or null when either id
// fails the gate.
export function conversationApprovalApiUrl(
  conversationId: string,
  approvalId: string,
): string | null {
  return conversationApiPath(conversationId, "approvals", approvalId);
}

// conversationWorkspaceUrl returns the workspace file proxy URL
// `/api/conversations/<id>/workspace/<path>` (just the `…/workspace/` base
// when `filePath` is empty). Workspace paths are file names, so `.` inside a
// segment is allowed (`report.pptx`), but an empty, `.` or `..` segment is
// refused and every segment is encoded on its own — a path can nest
// (`out/chart.png`) but never climb out of the workspace or leave the origin.
// Returns null when the conversation id or any path segment fails.
export function conversationWorkspaceUrl(
  conversationId: string,
  filePath = "",
): string | null {
  const base = conversationApiUrl(conversationId, "/workspace/");
  if (base === null) return null;
  if (filePath === "") return base;
  const segments = filePath.split("/");
  if (segments.some((s) => s === "" || s === "." || s === "..")) return null;
  return base + segments.map((segment) => encodeURIComponent(segment)).join("/");
}
