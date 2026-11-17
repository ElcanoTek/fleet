import { Icon } from "@/app/shared/ui/Icon";
import { isEmojiIcon } from "./layout";

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


// HomeIcon marks the item fleet opens to: Flag's map pin ("where you land").
export function HomeIcon({ className }: { className?: string }) {
  return <Icon name="map-pin" className={className} />;
}
