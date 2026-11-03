// The transcript's view of dashboard mode (docs/DASHBOARD-VIEW.md), in its
// own module so ChatTranscript does not pull the board's components in.
import { createContext } from "react";

/**
 * What the transcript needs while dashboard view is on: which cards live on
 * the board (drawn there, so the rail shows a pointer instead of a copy), and
 * the two actions a card in the rail offers. null outside dashboard view.
 */
export type DashboardTranscriptValue = {
  cardSlots: Map<string, string>;
  focusPanel: (key: string) => void;
  pinCard: (cardId: string, occurrence: number, panelKey?: string) => void;
  /**
   * A request to bring a message into the rail (the HUD's "approvals
   * waiting" light): the transcript is virtualized, so a card far from the
   * viewport is not mounted until its row is scrolled to. `nonce` makes a
   * repeated request for the same message fire again.
   */
  jumpRequest?: { messageId: number; approvalId: string; nonce: number } | null;
};

export const DashboardTranscriptContext = createContext<DashboardTranscriptValue | null>(null);

/**
 * The DOM id of a panel, for focus and scroll-into-view. Injective: every
 * character outside [A-Za-z0-9-] (including "_" itself) becomes _<hex>_, so
 * two slot keys never share an id (card ids are opaque provider strings).
 */
export const panelDomId = (key: string) =>
  `dash-panel-${key.replace(/[^A-Za-z0-9-]/g, (c) => `_${c.charCodeAt(0).toString(16)}_`)}`;
