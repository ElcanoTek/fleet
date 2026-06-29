"use client";

// Read-only public render of a shared conversation snapshot (#226). No session,
// no sidebar, no SSE — a static snapshot fetched at page load. Assistant text
// reuses the same markdown pipeline as the live chat (renderAssistantContent);
// user text renders verbatim. Tool calls / reasoning are intentionally omitted
// — this is the conversation transcript, not the agent's full working trace.

import { renderAssistantContent } from "@/app/chat/ui/AssistantContent";

type RawEntry = { role: string; type: string; content: unknown };

export type SharedSnapshot = {
  title: string;
  persona: string;
  model: string;
  created_at: number;
  shared_at: number;
  messages: RawEntry[];
};

type Bubble = { role: "user" | "assistant"; text: string };

// toBubbles flattens the stored history entries into a clean user/assistant
// text thread, merging consecutive same-role text (assistant replies can land
// as several text entries within one turn).
function toBubbles(entries: RawEntry[]): Bubble[] {
  const out: Bubble[] = [];
  for (const e of entries ?? []) {
    if (e.type !== "text" || (e.role !== "user" && e.role !== "assistant")) continue;
    const text = String((e.content as { text?: string } | null)?.text ?? "");
    if (!text) continue;
    const last = out[out.length - 1];
    if (last && last.role === e.role) {
      last.text += text;
    } else {
      out.push({ role: e.role, text });
    }
  }
  return out;
}

// Deterministic UTC date so server-render and client hydration agree.
function formatDate(unixSeconds: number): string {
  if (!unixSeconds) return "";
  return new Date(unixSeconds * 1000).toISOString().slice(0, 10);
}

export function SharedConversationView({ snapshot }: { snapshot: SharedSnapshot }) {
  const bubbles = toBubbles(snapshot.messages);
  const created = formatDate(snapshot.created_at);

  return (
    <main className="mx-auto flex min-h-screen w-full max-w-[48rem] flex-col gap-6 px-4 py-8 text-[var(--color-text-primary)]">
      <header className="border-b border-[var(--color-border-strong)] pb-4">
        <h1 className="text-[1.4rem] font-semibold leading-tight">{snapshot.title || "Shared conversation"}</h1>
        <p className="mt-1 text-[0.8125rem] text-[var(--color-text-muted)]">
          {[snapshot.model, created].filter(Boolean).join(" · ")}
        </p>
        <p className="mt-2 text-[0.75rem] text-[var(--color-text-muted)]">
          🔗 Read-only shared conversation — anyone with this link can view it.
        </p>
      </header>

      <div className="flex flex-col gap-5">
        {bubbles.length === 0 ? (
          <p className="text-[0.875rem] text-[var(--color-text-muted)]">This conversation has no messages to show.</p>
        ) : (
          bubbles.map((b, i) =>
            b.role === "user" ? (
              <div key={i} className="flex justify-end">
                <div className="max-w-[85%] whitespace-pre-wrap rounded-[1rem] bg-[var(--color-overlay-strong)] px-4 py-2.5 text-[0.9375rem] leading-[1.55]">
                  {b.text}
                </div>
              </div>
            ) : (
              <div key={i} className="assistant-markdown max-w-full text-[0.9375rem] leading-[1.6]">
                {renderAssistantContent(b.text, false, null)}
              </div>
            ),
          )
        )}
      </div>

      <footer className="mt-auto border-t border-[var(--color-border-strong)] pt-4 text-[0.75rem] text-[var(--color-text-muted)]">
        Shared from Fleet.
      </footer>
    </main>
  );
}
