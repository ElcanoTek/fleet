// A scrolling menu that is too short for its rows should end partway through
// one: a row cut in half says "there is more" in a way a fade alone does not.
// When the edge happens to land between two rows the menu looks complete, so
// it is shortened until it cuts a row again. It is never made taller: the
// caller's height is the room it really has.

export type MenuRow = { top: number; height: number };

// The least of a row that must show above the edge, and the least that must be
// hidden below it, for the row to read as cut off.
export const MENU_PEEK_MIN_PX = 12;

// visibleHeight is the menu's inner height with its scroll position at the
// top; rows are measured from the top of its content. Returns the inner height
// to use instead, or null to leave the menu as it is.
export function menuPeekHeight(
  visibleHeight: number,
  rows: readonly MenuRow[],
): number | null {
  const cutIndex = rows.findIndex((row) => row.top + row.height > visibleHeight + 1);
  if (cutIndex < 0) return null; // every row fits
  const cut = rows[cutIndex];
  const shown = visibleHeight - cut.top;
  if (shown >= MENU_PEEK_MIN_PX && shown <= cut.height - MENU_PEEK_MIN_PX) return null;
  // Nearly all of the row shows: end halfway through it instead.
  if (shown > cut.height - MENU_PEEK_MIN_PX) return Math.round(cut.top + cut.height / 2);
  // The edge sits in the gap above the row: end halfway through the one before.
  const previous = rows[cutIndex - 1];
  return previous ? Math.round(previous.top + previous.height / 2) : null;
}
