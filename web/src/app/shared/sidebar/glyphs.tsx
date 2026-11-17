import { Icon } from "@/app/shared/ui/Icon";
import { isEmojiIcon } from "./layout";

// HomeGlyph marks the item fleet opens to. Flag's core sprite has no house, so
// it is drawn here (the same stroke style as the sprite's icons).
export function HomeGlyph({ className }: { className?: string }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.9}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M3.5 10.5 12 3.5l8.5 7" />
      <path d="M5.5 9v10.5a1 1 0 0 0 1 1H10v-6h4v6h3.5a1 1 0 0 0 1-1V9" />
    </svg>
  );
}

// ItemIcon draws a sidebar item's icon: a sprite symbol, or the emoji the
// person picked (sized to sit in the same tile).
export function ItemIcon({ icon, className }: { icon: string; className?: string }) {
  if (isEmojiIcon(icon)) {
    return (
      <span aria-hidden="true" className="inline-flex items-center justify-center text-[0.95rem] leading-none">
        {icon}
      </span>
    );
  }
  return <Icon name={icon} className={className} />;
}

// Eye / eye-off for show and hide (not in the core sprite either).
export function EyeGlyph({ off = false, className }: { off?: boolean; className?: string }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.9}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12Z" />
      <circle cx="12" cy="12" r="2.75" />
      {off ? <path d="M4 4l16 16" /> : null}
    </svg>
  );
}

// GripGlyph: the drag handle on a row in the Customize panel.
export function GripGlyph({ className }: { className?: string }) {
  return (
    <svg aria-hidden="true" viewBox="0 0 24 24" className={className} fill="currentColor">
      <circle cx="9" cy="6" r="1.5" />
      <circle cx="15" cy="6" r="1.5" />
      <circle cx="9" cy="12" r="1.5" />
      <circle cx="15" cy="12" r="1.5" />
      <circle cx="9" cy="18" r="1.5" />
      <circle cx="15" cy="18" r="1.5" />
    </svg>
  );
}
