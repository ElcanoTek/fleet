import { useCallback, useEffect, useRef } from "react";

// useQueuedSwitchRetry holds a "Switch & retry" pressed while the turn's
// stream is still open and sends it once streaming ends
// (docs/CHAT-ESCALATION.md). A card fleet stages after the reply lands just
// before the stream closes, and retryLastUserMessage drops a retry while a
// turn is in flight, so without this an early click pinned the advanced
// model but never retried.
//
// The held retry belongs to the conversation it was pressed in. Retry acts on
// whichever conversation is active, so if the user moves to another one
// before the stream closes, the held retry is dropped rather than run there.
export function useQueuedSwitchRetry(
  isStreaming: boolean,
  convKey: string,
  retry: (opts: { model: string }) => void | Promise<void>,
): (model: string) => void | Promise<void> {
  const pending = useRef<{ model: string; convKey: string } | null>(null);
  useEffect(() => {
    const held = pending.current;
    if (!held) return;
    if (held.convKey !== convKey) {
      pending.current = null;
      return;
    }
    if (isStreaming) return;
    pending.current = null;
    void retry({ model: held.model });
  }, [isStreaming, convKey, retry]);
  return useCallback(
    (model: string) => {
      if (isStreaming) {
        pending.current = { model, convKey };
        return;
      }
      return retry({ model });
    },
    [isStreaming, convKey, retry],
  );
}

// advancedRetryAllowed reports whether "Retry with" the advanced model may be
// offered: always outside lockdown; inside it, only when the lockdown
// allow-list names the model (a glob-only list hides it, the safe side).
export function advancedRetryAllowed(isLockdown: boolean, allowed: readonly string[], advanced: string): boolean {
  return !isLockdown || allowed.includes(advanced);
}
