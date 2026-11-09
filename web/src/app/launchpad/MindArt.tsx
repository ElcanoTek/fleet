import mind1 from "./assets/mind-1.webp";
import mind2 from "./assets/mind-2.webp";
import mind3 from "./assets/mind-3.webp";
import mind4 from "./assets/mind-4.webp";

// MindArt — the four panels of the Expanding Brain meme for the Launchpad's
// "Why patrol?" ladder. Decorative only; the rung's text carries the meaning.

export type MindLevel = 1 | 2 | 3 | 4;

const MIND_IMAGES: Record<MindLevel, string> = {
  1: mind1,
  2: mind2,
  3: mind3,
  4: mind4,
};

export function MindArt({ level, className = "" }: { level: MindLevel; className?: string }) {
  return (
    <img
      src={MIND_IMAGES[level]}
      alt=""
      width={160}
      height={112}
      loading="eager"
      decoding="async"
      draggable={false}
      className={`select-none rounded-[var(--radius-md)] object-cover ${className}`}
      data-testid="launchpad-mind-art"
      data-level={level}
    />
  );
}
