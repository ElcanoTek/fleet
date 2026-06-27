"use client";

import { useEffect, useRef, useState } from "react";

// Full-text search palette (#308): a Cmd/Ctrl+K command-palette modal that
// queries GET /api/search (proxied to the chat server) with a 300ms debounce and
// lists matching conversations with a highlighted preview. Selecting a result
// loads that conversation.
//
// The parent mounts this ONLY while the palette is open ({searchOpen && <SearchBar/>}),
// so every open is a fresh mount — no reset-on-open effect needed.

export type SearchResult = {
  conversation_id: string;
  title: string;
  match_preview: string;
  matched_at: number;
};

// renderPreview escapes ALL HTML in the server-produced snippet, then re-enables
// ONLY the <mark> highlight tags ts_headline inserted. Message content is
// arbitrary user text (it may contain real HTML/markup), so this is what keeps
// the dangerouslySetInnerHTML below from becoming an injection vector.
export function renderPreview(raw: string): string {
  const escaped = raw
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
  return escaped
    .replace(/&lt;mark&gt;/g, "<mark>")
    .replace(/&lt;\/mark&gt;/g, "</mark>");
}

function formatMatchedAt(unixSeconds: number): string {
  if (!unixSeconds) return "";
  try {
    return new Date(unixSeconds * 1000).toLocaleDateString();
  } catch {
    return "";
  }
}

export function SearchBar({
  onClose,
  onSelect,
}: {
  onClose: () => void;
  onSelect: (conversationId: string) => void;
}) {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState<SearchResult[]>([]);
  const [loading, setLoading] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  // Focus the input on mount (DOM side effect, no state update).
  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  // Close on Escape via a window-level listener — focus-independent, so it fires
  // even if focus has left the input (more robust than relying on the dialog's
  // onKeyDown bubbling, which proved flaky under headless CI).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  // Debounced search: fire 300ms after the last keystroke (no Enter needed). All
  // state updates happen inside the timer/async callback, never synchronously in
  // the effect body.
  useEffect(() => {
    const q = query.trim();
    if (!q) return;
    let cancelled = false;
    const handle = setTimeout(() => {
      setLoading(true);
      void (async () => {
        try {
          const res = await fetch(`/api/search?q=${encodeURIComponent(q)}`, { cache: "no-store" });
          const data = res.ok ? ((await res.json()) as { results: SearchResult[] | null }) : { results: [] };
          if (!cancelled) setResults(data.results ?? []);
        } catch {
          if (!cancelled) setResults([]);
        } finally {
          if (!cancelled) setLoading(false);
        }
      })();
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(handle);
    };
  }, [query]);

  const trimmed = query.trim();

  return (
    <div className="fixed inset-0 z-50 flex items-start justify-center px-4 pt-[12vh]" data-testid="search-overlay">
      <button
        aria-label="Close search"
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onClose}
      />
      <div
        className="relative z-10 flex w-full max-w-[34rem] flex-col overflow-hidden rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] shadow-[var(--composer-shadow)] backdrop-blur-sm"
        role="dialog"
        aria-label="Search conversations"
        onKeyDown={(e) => {
          if (e.key === "Escape") {
            e.preventDefault();
            onClose();
          }
        }}
      >
        <input
          ref={inputRef}
          data-testid="search-input"
          type="text"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Search conversations…"
          className="w-full border-b border-[var(--color-border-strong)] bg-transparent px-4 py-3 text-[0.95rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)]"
          aria-label="Search query"
        />
        <div className="max-h-[55vh] overflow-y-auto" data-testid="search-results">
          {loading ? (
            <div className="px-4 py-3 text-[0.85rem] text-[var(--color-text-muted)]">Searching…</div>
          ) : trimmed && results.length === 0 ? (
            <div data-testid="search-empty" className="px-4 py-3 text-[0.85rem] text-[var(--color-text-muted)]">
              No matching conversations.
            </div>
          ) : trimmed ? (
            results.map((r) => (
              <button
                key={r.conversation_id}
                data-testid="search-result"
                type="button"
                onClick={() => onSelect(r.conversation_id)}
                className="block w-full border-b border-[var(--color-border)] px-4 py-3 text-left transition hover:bg-[var(--color-overlay-soft)]"
              >
                <div className="flex items-baseline justify-between gap-3">
                  <span className="truncate font-medium text-[var(--color-text-primary)]">{r.title}</span>
                  <span className="shrink-0 text-[0.7rem] text-[var(--color-text-muted)]">
                    {formatMatchedAt(r.matched_at)}
                  </span>
                </div>
                <div
                  className="search-preview mt-1 line-clamp-2 text-[0.8rem] leading-[1.5] text-[var(--color-text-secondary)]"
                  dangerouslySetInnerHTML={{ __html: renderPreview(r.match_preview) }}
                />
              </button>
            ))
          ) : null}
        </div>
      </div>
    </div>
  );
}
