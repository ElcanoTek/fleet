// Full-text search helpers (#308). The original Cmd/Ctrl+K palette merged
// into the rail's unified search bar (ConversationSidebar), which reuses the
// result type and the snippet sanitizer below; ⌘K now focuses that bar.

export type SearchResult = {
  conversation_id: string;
  title: string;
  match_preview: string;
  matched_at: number;
};

// renderPreview escapes ALL HTML in the server-produced snippet, then re-enables
// ONLY the <mark> highlight tags ts_headline inserted. Message content is
// arbitrary user text (it may contain real HTML/markup), so this is what keeps
// the dangerouslySetInnerHTML at the render site from becoming an injection
// vector.
export function renderPreview(raw: string): string {
  const escaped = raw
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return escaped
    .replace(/&lt;mark&gt;/g, "<mark>")
    .replace(/&lt;\/mark&gt;/g, "</mark>");
}
