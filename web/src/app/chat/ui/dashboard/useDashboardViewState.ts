// useDashboardViewState — chat-experience's dashboard-view slice
// (docs/DASHBOARD-VIEW.md), moved here verbatim (#2035): the per-conversation
// on/off state and its switch rule, the board/chat pane, the focused and
// jumped-to panel, the board prefs and derived board, the toggle's focus
// hand-off, and the transcript context the board's cards read. The hooks run
// in the same order as before, at the same point of ChatExperience's render;
// the caller still computes the conversation id and the Share-created flag
// (the flag reads a ref of its own). One change: focusDashboardPanel's delayed
// steps are cleared on unmount.

import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch, type SetStateAction } from "react";
import { deriveGenUiState } from "../genui/transcript";
import type { Message } from "../history";
import { PENDING_CONV_KEY } from "../workspaceHref";
import { boardStatus, cardRef, deriveBoard } from "./board";
import { panelDomId, type DashboardTranscriptValue } from "./context";
import {
  dashboardViewAfterSwitch,
  readDashboardView,
  useBoardPrefs,
  writeDashboardView,
} from "./prefs";

export function useDashboardViewState({
  currentConvKey,
  fullPageOverlay,
  dashboardConvId,
  dashboardJustCreated,
  messages,
  isStreaming,
  setSidebarOpen,
}: {
  currentConvKey: string;
  fullPageOverlay: boolean;
  /** realConvId(currentConvKey): the open slot's conversation id, if it has one. */
  dashboardConvId: string | null;
  /** The open conversation was just created by Share from the blank slot. */
  dashboardJustCreated: boolean;
  messages: Message[];
  isStreaming: boolean;
  setSidebarOpen: Dispatch<SetStateAction<boolean>>;
}) {
  // The chat as a live board: panels are the conversation's show_ui cards,
  // the chat narrows to a rail beside them. Remembered per conversation in
  // this browser. A draft keeps the view it had when it gets its id, and so
  // does a new chat started from dashboard view.
  const [dashboardView, setDashboardView] = useState(() =>
    readDashboardView(dashboardConvId),
  );
  // The slot key last seen. A draft becomes a conversation by way of a
  // per-submission pending key ("<PENDING>:<n>", renamed to the real id when
  // the server names it): only that path keeps the current view. Opening any
  // other conversation, from a blank chat included, restores that
  // conversation's own saved view (a chat Share just created from the blank
  // slot is still this draft: dashboardJustCreated).
  const [dashboardKeySeen, setDashboardKeySeen] = useState(currentConvKey);
  if (dashboardKeySeen !== currentConvKey) {
    setDashboardKeySeen(currentConvKey);
    const next = dashboardViewAfterSwitch(
      dashboardKeySeen,
      dashboardConvId,
      dashboardView,
      PENDING_CONV_KEY,
      dashboardJustCreated,
    );
    if (next !== dashboardView) setDashboardView(next);
  }
  useEffect(() => {
    if (dashboardConvId) writeDashboardView(dashboardConvId, dashboardView);
  }, [dashboardConvId, dashboardView]);
  const dashboardActive = dashboardView && !fullPageOverlay;
  const [dashboardPane, setDashboardPane] = useState<"board" | "chat">("board");
  const [dashboardFocus, setDashboardFocus] = useState("");
  const [dashboardJump, setDashboardJump] = useState<{
    messageId: number;
    approvalId: string;
    nonce: number;
  } | null>(null);
  const [boardPrefs, updateBoardPrefs] = useBoardPrefs(
    currentConvKey,
    dashboardConvId,
    PENDING_CONV_KEY,
    dashboardJustCreated,
  );
  const dashboardGenUi = useMemo(
    () => (dashboardActive ? deriveGenUiState(messages) : null),
    [dashboardActive, messages],
  );
  const board = useMemo(
    () =>
      dashboardGenUi ? deriveBoard(messages, dashboardGenUi, boardPrefs) : null,
    [dashboardGenUi, messages, boardPrefs],
  );
  const dashboardStatus = useMemo(
    () => boardStatus(messages, isStreaming),
    [messages, isStreaming],
  );
  // Set by a toggle; read by the focus hand-off effect below.
  const dashboardToggledRef = useRef(false);
  const toggleDashboardView = useCallback(() => {
    setDashboardView((on) => !on);
    setDashboardFocus("");
    setDashboardPane("board");
    // A drawer left open on a phone would reappear on the way out.
    setSidebarOpen(false);
    dashboardToggledRef.current = true;
  }, [setSidebarOpen]);
  // The control that was pressed unmounts with the layout it lived in: once
  // the new layout has rendered, put the keyboard focus on its toggle (HUD
  // Exit on the way in, the header Dashboard button on the way out). Only
  // after a toggle — never when a conversation simply opens in the view.
  useEffect(() => {
    if (!dashboardToggledRef.current) {
      // Opening a conversation that restores dashboard view hides the rail
      // the user just activated a row in: bring the focus into the HUD
      // rather than leave it inside a hidden subtree.
      if (
        dashboardActive &&
        document.activeElement?.closest('[data-dashboard-rail-wrapper]')
      ) {
        document.querySelector<HTMLElement>('[data-testid="dashboard-exit"]')?.focus();
      }
      return;
    }
    dashboardToggledRef.current = false;
    // Not if the toggle came from inside a dialog that keeps the focus.
    if (document.activeElement?.closest('[role="dialog"]')) return;
    if (dashboardActive) {
      document.querySelector<HTMLElement>('[data-testid="dashboard-exit"]')?.focus();
    } else {
      const enterBtn = document.querySelector<HTMLElement>('[data-testid="dashboard-enter"]');
      if (enterBtn && enterBtn.getClientRects().length > 0) {
        enterBtn.focus();
      } else {
        document.querySelector<HTMLElement>('[data-testid="composer-more-button"]')?.focus();
      }
    }
  }, [dashboardActive]);
  const focusTimersRef = useRef<Set<number>>(new Set());
  const focusDashboardPanel = useCallback((key: string) => {
    setDashboardFocus("");
    setDashboardPane("board");
    // After the pane switch paints: bring the panel into view and flash it.
    // The focus moves only if the person has not already put it somewhere
    // else in the meantime (typing in the composer, say): a delayed focus
    // must never pull the caret out from under them. A focus left on the
    // page body (the pane switch unmounted the old target) still moves.
    const focusedAtRequest = document.activeElement;
    const timers = focusTimersRef.current;
    const later = (fn: () => void, ms: number) => {
      const t = window.setTimeout(() => {
        timers.delete(t);
        fn();
      }, ms);
      timers.add(t);
    };
    later(() => {
      const el = document.getElementById(panelDomId(key));
      if (!el) return;
      el.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
      const now = document.activeElement;
      const movedOn = now !== null && now !== document.body && now !== focusedAtRequest;
      if (!movedOn) el.focus({ preventScroll: true });
      el.classList.remove("dash-panel-flash");
      void el.offsetWidth; // restart the animation
      el.classList.add("dash-panel-flash");
      later(() => el.classList.remove("dash-panel-flash"), 1600);
    }, 60);
  }, []);
  // The two delayed steps above outlive a quick unmount (leaving /chat right
  // after a pin): clear whatever is still pending so neither touches a
  // detached node.
  useEffect(() => {
    const timers = focusTimersRef.current;
    return () => {
      for (const t of timers) window.clearTimeout(t);
      timers.clear();
    };
  }, []);
  const dashboardTranscript = useMemo<DashboardTranscriptValue | null>(
    () =>
      board
        ? {
            cardSlots: board.cardSlots,
            focusPanel: focusDashboardPanel,
            pinCard: (cardId, occurrence, panelKey) => {
              const ref = cardRef(cardId, occurrence);
              // A card whose panel was taken off the board (its own slot or one
              // it inherited through `replaces`): bring that slot back as it
              // was, rather than pinning a second slot for the card.
              const hidden = board.removedKeysOf.get(cardId);
              if (hidden && hidden.length > 0) {
                updateBoardPrefs((p) => ({
                  ...p,
                  removed: p.removed.filter((k) => !hidden.includes(k)),
                }));
                focusDashboardPanel(hidden[0]);
                return;
              }
              updateBoardPrefs((p) => ({
                ...p,
                pinned: p.pinned.includes(ref) ? p.pinned : [...p.pinned, ref],
                // Every key the card's hidden slot answers to: it may have
                // been removed under a key it has since moved away from.
                removed: p.removed.filter(
                  (k) =>
                    k !== `c:${ref}` &&
                    (!panelKey || k !== `p:${panelKey}`) &&
                    !(board.removedKeysOf.get(cardId) ?? []).includes(k),
                ),
              }));
              focusDashboardPanel(panelKey ? `p:${panelKey}` : `c:${ref}`);
            },
            jumpRequest: dashboardJump,
          }
        : null,
    [board, focusDashboardPanel, updateBoardPrefs, dashboardJump],
  );
  // The HUD's "approvals waiting" light: the transcript is virtualized, so
  // the rail is asked to scroll the message holding the first pending
  // approval into view (ChatTranscript), which then centres the card.
  const showDashboardApprovals = useCallback(() => {
    setDashboardPane("chat");
    for (const m of messages) {
      const a = (m.approvals ?? []).find((x) => x.status === "pending");
      if (a) {
        setDashboardJump((prev) => ({
          messageId: m.id,
          approvalId: a.id,
          nonce: (prev?.nonce ?? 0) + 1,
        }));
        return;
      }
    }
  }, [messages]);

  return {
    dashboardActive,
    dashboardPane,
    setDashboardPane,
    dashboardFocus,
    setDashboardFocus,
    updateBoardPrefs,
    dashboardGenUi,
    board,
    dashboardStatus,
    toggleDashboardView,
    dashboardTranscript,
    showDashboardApprovals,
  };
}
