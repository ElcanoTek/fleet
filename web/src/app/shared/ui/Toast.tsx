"use client";

import { createContext, useCallback, useContext, useState } from "react";

// Toast notifications for the orchestrator view. Replaces moc's imperative
// toast.js (document.body manipulation) with a React context + portal-free
// container. showToast(message, type) is exposed via useToast().

export type ToastType = "success" | "error" | "warning" | "info";
type ToastItem = { id: number; message: string; type: ToastType };

type ToastContextValue = {
  showToast: (message: string, type?: ToastType, durationMs?: number) => void;
};

const ToastContext = createContext<ToastContextValue | null>(null);

let nextId = 1;

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const showToast = useCallback(
    (message: string, type: ToastType = "info", durationMs = 4000) => {
      const id = nextId++;
      setToasts((prev) => [...prev, { id, message, type }]);
      if (durationMs > 0) {
        setTimeout(() => dismiss(id), durationMs);
      }
    },
    [dismiss],
  );

  return (
    <ToastContext.Provider value={{ showToast }}>
      {children}
      {/* The toast itself stays a non-interactive live region (role="alert"):
          dismissal lives on a real <button> inside it, not on the container.
          The container used to carry the onClick, which made dismissal
          mouse-only — a keyboard or switch user had no way to clear a toast at
          all. Giving the container a key handler instead would have meant
          putting an auto-expiring element into the tab order and re-labelling a
          status message as a widget; a dedicated close button is the standard
          accessible toast shape and keeps the announcement semantics intact. */}
      <div className="toast-container" aria-live="polite" aria-atomic="true">
        {toasts.map((t) => (
          <div
            key={t.id}
            className={`toast toast--${t.type} flex items-start gap-2`}
            role="alert"
          >
            <span className="min-w-0 flex-1">{t.message}</span>
            <button
              type="button"
              aria-label="Dismiss notification"
              className="shrink-0 cursor-pointer border-0 bg-transparent p-0 text-current opacity-60 transition hover:opacity-100"
              onClick={() => dismiss(t.id)}
            >
              <span aria-hidden="true">&times;</span>
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  // Fall back to a no-op so components used outside a provider (e.g. isolated
  // unit tests) don't crash.
  if (!ctx) {
    return { showToast: () => {} };
  }
  return ctx;
}
