// Conversation API URLs. Conversation ids are server-minted UUIDs
// (store.CreateConversation → uuid.NewString). Encoding the id as a single
// path segment AND refusing anything that is not a single path-safe token
// closes client-side request forgery: a hostile id cannot rewrite the path
// (`../`, extra segments, query/hash) or target a different origin.
//
// Allowed: letters, digits, underscore, hyphen (covers UUIDs). No `.` so
// `..` cannot traverse; no `/`, `:`, `?`, or `#`.

const conversationIdPattern = /^[A-Za-z0-9_-]+$/;

export function isConversationId(id: string): boolean {
  return id.length > 0 && conversationIdPattern.test(id);
}

// conversationApiUrl returns `/api/conversations/<id>` plus an optional
// suffix (`/inflight`, `/stream?turn_id=…`). Returns null when `id` is not
// a UUID, so callers skip the request rather than interpolating untrusted
// text into a URL.
export function conversationApiUrl(id: string, suffix = ""): string | null {
  if (!isConversationId(id)) return null;
  return `/api/conversations/${encodeURIComponent(id)}${suffix}`;
}
