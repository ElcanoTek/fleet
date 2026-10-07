"use client";

import { createContext, useCallback, useContext, useMemo, useRef, useState } from "react";

// Chat-surface toasts that can carry ONE action — "Manage" after a share,
// "Share with <team>" after a move. The rail's error toast is a plain line;
// the sharing paths need a follow-up the reader can take without hunting for
// it, so this is its own small provider rather than a second meaning bolted
// onto railError.
//
// The toast is a polite live region with a real <button> for the action and
// another for dismissal (the shared/ui Toast's accessibility shape). Taking
// the action dismisses it.

export type ChatToastAction = { label: string; onClick: () => void };
export type ChatToastInput = {
  message: string;
  action?: ChatToastAction;
  /** Milliseconds; 0 keeps it until dismissed. Default 6000. */
  durationMs?: number;
};
type ChatToastItem = ChatToastInput & { id: number };

type ChatToastContextValue = { notify: (toast: ChatToastInput) => void };

const ChatToastContext = createContext<ChatToastContextValue | null>(null);

/** notify() from anywhere under ChatToastProvider; a no-op outside one (tests). */
export function useChatToast(): ChatToastContextValue {
  return useContext(ChatToastContext) ?? NOOP;
}
const NOOP: ChatToastContextValue = { notify: () => {} };

export function ChatToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ChatToastItem[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const notify = useCallback(
    (toast: ChatToastInput) => {
      const id = nextId.current++;
      // One at a time: a newer confirmation supersedes an older one rather
      // than stacking a column of stale sentences.
      setToasts([{ ...toast, id }]);
      const ms = toast.durationMs ?? 6000;
      if (ms > 0) window.setTimeout(() => dismiss(id), ms);
    },
    [dismiss],
  );

  const value = useMemo(() => ({ notify }), [notify]);

  return (
    <ChatToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        aria-atomic="true"
        className="pointer-events-none fixed inset-x-0 bottom-6 z-[70] flex flex-col items-center gap-2 px-4"
      >
        {toasts.map((t) => (
          <div
            key={t.id}
            role="status"
            data-testid="chat-toast"
            className="pointer-events-auto flex max-w-[34rem] items-center gap-3 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-2 pl-3.5 pr-2 text-[0.82rem] text-[var(--color-text-primary)] shadow-[var(--shadow-md)]"
          >
            <span className="min-w-0 flex-1 leading-[1.45]">{t.message}</span>
            {t.action ? (
              <button
                type="button"
                className="shrink-0 rounded-[var(--radius-md)] px-2 py-1 text-[0.8rem] font-semibold text-[var(--color-accent)] transition hover:bg-[var(--color-overlay-strong)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                onClick={() => {
                  dismiss(t.id);
                  t.action?.onClick();
                }}
              >
                {t.action.label}
              </button>
            ) : null}
            <button
              type="button"
              aria-label="Dismiss notification"
              className="shrink-0 rounded-[var(--radius-md)] px-1.5 py-1 text-[var(--color-text-muted)] transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
              onClick={() => dismiss(t.id)}
            >
              <span aria-hidden>×</span>
            </button>
          </div>
        ))}
      </div>
    </ChatToastContext.Provider>
  );
}
