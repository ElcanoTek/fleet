// Full-text search helpers (#308). The original Cmd/Ctrl+K palette merged
// into the rail's unified search bar (ConversationSidebar), which reuses the
// result type and the snippet renderer below; ⌘K now focuses that bar.

import { createElement, type ReactNode } from "react";

export type SearchResult = {
  conversation_id: string;
  title: string;
  match_preview: string;
  matched_at: number;
};

// previewNodes turns a server-produced snippet into React nodes: plain text,
// plus a <mark> element for each balanced <mark>…</mark> pair ts_headline
// inserted. Message content is arbitrary user text (it may contain real
// HTML/markup), so everything is rendered as text by React — no HTML string
// ever reaches the DOM. Only the literal "<mark>"/"</mark>" tokens are
// recognised; an unbalanced or nested tag degrades to visible text.
const MARK_PAIR = /<mark>([\s\S]*?)<\/mark>/g;

export function previewNodes(raw: string): ReactNode[] {
  const nodes: ReactNode[] = [];
  let last = 0;
  let key = 0;
  for (const m of raw.matchAll(MARK_PAIR)) {
    const start = m.index ?? 0;
    if (start > last) nodes.push(raw.slice(last, start));
    nodes.push(createElement("mark", { key: key++ }, m[1]));
    last = start + m[0].length;
  }
  if (last < raw.length) nodes.push(raw.slice(last));
  return nodes;
}
