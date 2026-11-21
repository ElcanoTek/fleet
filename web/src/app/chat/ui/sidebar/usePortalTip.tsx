import { useEffect, useState } from "react";
import { createPortal } from "react-dom";

// A tooltip for a sidebar control, portaled to <body> so the rail's one
// scroller cannot clip it. It shows on hover, and on focus for a keyboard user
// only; it is placed once, below its button, and goes away when anything
// scrolls. Used by the row menus' buttons.
export function usePortalTip(tip: string) {
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const show = (e: React.SyntheticEvent<HTMLElement>) => {
    // Focus shows the tip for a keyboard user only. A tap or click also
    // focuses the button — and so does a menu handing the focus back when it
    // closes — and on a touch screen, with no pointer to move away, the tip
    // then sat over the list for good.
    if (e.type === "focus") {
      let keyboard = true;
      try {
        keyboard = e.currentTarget.matches(":focus-visible");
      } catch {
        // An engine without :focus-visible: show, as before.
      }
      if (!keyboard) return;
    }
    const r = e.currentTarget.getBoundingClientRect();
    // Below the button, arrow pointing back up at it (the design's conv-info
    // tooltip placement: bottom + 7, offset so the arrow lands on the anchor).
    setPos({
      top: Math.round(r.bottom + 7),
      left: Math.round(r.left + r.width / 2 - 17),
    });
  };
  const hide = () => setPos(null);
  // The tip is placed once, where its button was: when anything scrolls, the
  // button moves and the tip would be left pointing at another row.
  const shown = pos !== null;
  useEffect(() => {
    if (!shown) return;
    const onScroll = () => setPos(null);
    window.addEventListener("scroll", onScroll, true);
    return () => window.removeEventListener("scroll", onScroll, true);
  }, [shown]);
  const tipNode =
    pos && typeof document !== "undefined"
      ? createPortal(
          <span
            role="tooltip"
            className="conv-tooltip"
            style={{ top: pos.top, left: pos.left }}
          >
            {tip}
          </span>,
          document.body,
        )
      : null;
  return { show, hide, tipNode };
}
