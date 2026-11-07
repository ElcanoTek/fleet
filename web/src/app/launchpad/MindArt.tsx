import { useId } from "react";

// MindArt — the "expanding brain" meme, drawn by hand for the Launchpad's
// "Why patrol?" ladder. One head in profile, four rungs: the brain grows,
// tints, glows and finally outgrows the head into its own galaxy. It is an
// illustration, not an icon (icons still come from Flag's sprite), and it is
// decoration only — the rung's text carries the meaning.
//
// Colours are theme tokens, applied through classes and inline styles (never
// presentation attributes, which do not resolve var()). Motion — the halo's
// breathing, the rays and the orbit turning, the stars twinkling — runs only
// under motion-safe.

export type MindLevel = 1 | 2 | 3 | 4;

// Head in profile, facing right; open at the neck. HEAD_CLIP is the same
// outline closed along the neck, used to keep the brain inside it.
const HEAD =
  "M18 61 V51 C11 47 8 40 8 31 C8 17 18 7 31 7 C44 7 54 16 54 28 C54 31 57 35 58 38 C59 40 57 41 55 41 V46 C55 49 53 50 50 50 H45 V61";
const HEAD_CLIP = `${HEAD} Z`;
// A brain in side view (lateral), facing right like the head: the cerebrum
// with its frontal lobe forward and temporal lobe below, the cerebellum tucked
// under the back, and the brainstem running down toward the neck. Centred near
// (31, 23).
const CEREBRUM =
  "M20 27 C17 24.5 17 19 20 15.8 C22.5 12.5 27 11 31.5 11.3 C36.5 11.2 41.2 12.8 43.2 16.3 C45.2 19.5 44.6 23.4 42.4 25.6 C40.8 27.4 38.6 28 36.6 27.6 C34.6 29.6 31 30.3 28.4 29.2 C25.8 30.5 22.4 29.6 20 27 Z";
const CEREBELLUM = "M21 28.4 C19.6 31 21.8 33.4 25 33.1 C27.8 32.9 29.1 30.9 27.6 29.2 C25.6 30.2 22.8 29.9 21 28.4 Z";
const STEM = "M28.2 30.2 C28.9 32.6 28.8 35 28.1 37.4";
const FOLDS = [
  // Sylvian (lateral) fissure, separating the temporal lobe.
  "M27.8 25.2 C30.8 23.4 34.6 23.2 38.6 24.6",
  // Central sulcus, top to middle.
  "M32.6 11.6 C31.6 14.6 33.8 16.8 32.2 20.4",
  // Gyri.
  "M24.2 14.6 C25.8 16.8 23.4 18.8 25 21.2",
  "M38.8 13.6 C37.2 16.2 39.8 18 38.2 20.8",
  "M21.4 21.8 C23.2 22.6 24.8 22.2 26 24",
  "M30.6 26.6 C32.6 27.2 34.4 26.8 35.6 25.8",
  // Cerebellum's ridges.
  "M22.2 30.6 C23.8 31.4 25.6 31.4 27 30.6",
];
const CX = 31;
const CY = 23;

// The brain always stays inside the head: 1.15 is the largest scale whose
// crown clears the skull (the clip path below guarantees it either way).
const SCALE: Record<MindLevel, number> = { 1: 0.5, 2: 0.72, 3: 0.95, 4: 1.15 };

const STARS: [number, number, number][] = [
  [6, 8, 0.9],
  [58, 6, 0.7],
  [62, 22, 1.1],
  [3, 30, 0.6],
  [12, 50, 0.8],
  [60, 54, 0.6],
  [40, 4, 0.6],
  [50, 58, 0.9],
];

export function MindArt({ level, className = "" }: { level: MindLevel; className?: string }) {
  const uid = useId().replace(/[^a-zA-Z0-9_-]/g, "");
  const glowId = `mind-glow-${uid}`;
  const cosmosId = `mind-cosmos-${uid}`;
  const clipId = `mind-head-${uid}`;
  const k = SCALE[level];
  const brainTransform = `translate(${CX} ${CY}) scale(${k}) translate(${-CX} ${-CY})`;

  const brainFill =
    level === 1
      ? "fill-[color-mix(in_srgb,var(--color-text-muted)_45%,transparent)]"
      : level === 2
        ? "fill-[color-mix(in_srgb,var(--color-primary)_40%,var(--color-surface-1))]"
        : "fill-[var(--color-primary-fill)]";
  const brainStroke =
    level === 1 ? "stroke-[var(--color-text-muted)]" : "stroke-[color-mix(in_srgb,var(--color-primary)_80%,var(--color-text-primary))]";

  return (
    <svg
      viewBox="0 0 64 64"
      className={className}
      aria-hidden="true"
      focusable="false"
      data-testid="launchpad-mind-art"
      data-level={level}
    >
      <defs>
        <clipPath id={clipId}>
          <path d={HEAD_CLIP} />
        </clipPath>
        <radialGradient id={glowId}>
          <stop offset="0%" style={{ stopColor: "var(--color-primary)", stopOpacity: level === 4 ? 0.85 : 0.6 }} />
          <stop offset="100%" style={{ stopColor: "var(--color-primary)", stopOpacity: 0 }} />
        </radialGradient>
        <radialGradient id={cosmosId} cx="45%" cy="40%" r="70%">
          <stop offset="0%" style={{ stopColor: "var(--color-primary)", stopOpacity: 0.55 }} />
          <stop offset="55%" style={{ stopColor: "var(--color-accent)", stopOpacity: 0.35 }} />
          <stop offset="100%" style={{ stopColor: "var(--color-accent)", stopOpacity: 0 }} />
        </radialGradient>
      </defs>

      {/* Galaxy brain: the cosmos behind everything, with twinkling stars. */}
      {level === 4 ? (
        <>
          <circle cx="32" cy="30" r="31" style={{ fill: `url(#${cosmosId})` }} />
          {STARS.map(([x, y, r], i) => (
            <circle
              key={i}
              cx={x}
              cy={y}
              r={r}
              className={`fill-[var(--color-accent)] ${i % 2 ? "motion-safe:animate-pulse" : ""}`}
            />
          ))}
        </>
      ) : null}

      {/* The head. */}
      <path
        d={HEAD}
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
        className={
          level === 4
            ? "fill-[color-mix(in_srgb,var(--color-surface-1)_55%,transparent)] stroke-[color-mix(in_srgb,var(--color-primary)_70%,var(--color-border-strong))]"
            : "fill-[var(--color-surface-1)] stroke-[var(--color-border-strong)]"
        }
      />

      {/* Glow halo from rung 2 up; it breathes from rung 3. */}
      {level >= 2 ? (
        <circle
          cx={CX}
          cy={CY}
          r={level === 2 ? 11 : level === 3 ? 17 : 24}
          style={{ fill: `url(#${glowId})` }}
          className={level >= 3 ? "motion-safe:animate-pulse" : ""}
        />
      ) : null}

      {/* Rays from rung 3; on the galaxy they turn. */}
      {level >= 3 ? (
        <g
          className={`stroke-[var(--color-primary)] [transform-box:fill-box] origin-center ${
            level === 4 ? "motion-safe:animate-[spin_14s_linear_infinite]" : ""
          }`}
          strokeWidth="1.4"
          strokeLinecap="round"
        >
          {Array.from({ length: level === 4 ? 12 : 8 }, (_, i) => {
            const n = level === 4 ? 12 : 8;
            const a = (i / n) * Math.PI * 2;
            const r1 = level === 4 ? 21 : 16.5;
            const r2 = level === 4 ? 27 : 20;
            return (
              <line
                key={i}
                x1={CX + Math.cos(a) * r1}
                y1={CY + Math.sin(a) * r1}
                x2={CX + Math.cos(a) * r2}
                y2={CY + Math.sin(a) * r2}
                opacity={i % 2 ? 0.55 : 0.9}
              />
            );
          })}
        </g>
      ) : null}

      {/* The brain itself. */}
      <g clipPath={`url(#${clipId})`}>
      <g transform={brainTransform}>
        <path d={STEM} fill="none" strokeWidth={2.2 / k} strokeLinecap="round" className={brainStroke} />
        <path d={CEREBELLUM} strokeWidth={1.3 / k} strokeLinejoin="round" className={`${brainFill} ${brainStroke}`} />
        <path d={CEREBRUM} strokeWidth={1.4 / k} strokeLinejoin="round" className={`${brainFill} ${brainStroke}`} />
        <g
          fill="none"
          strokeWidth={1.1 / k}
          strokeLinecap="round"
          className={level >= 3 ? "stroke-[var(--color-on-primary-fill)]" : brainStroke}
          opacity={level === 1 ? 0.7 : 0.85}
        >
          {FOLDS.map((d) => (
            <path key={d} d={d} />
          ))}
        </g>
      </g>
      </g>

      {/* Small brain's thought bubble: a lonely question mark. */}
      {level === 1 ? (
        <g className="fill-[var(--color-surface-2)] stroke-[var(--color-border-strong)]" strokeWidth="1">
          <circle cx="47" cy="12" r="1.2" />
          <circle cx="51" cy="8.5" r="2" />
          <circle cx="57.5" cy="5.5" r="4.5" />
          <path
            d="M56 4.4 C56 3 59 3 59 4.6 C59 5.6 57.5 5.7 57.5 6.8"
            fill="none"
            strokeWidth="0.9"
            strokeLinecap="round"
            className="stroke-[var(--color-text-secondary)]"
          />
          <circle cx="57.5" cy="8.2" r="0.45" className="fill-[var(--color-text-secondary)]" stroke="none" />
        </g>
      ) : null}

      {/* Galaxy brain: a little planet in orbit. */}
      {level === 4 ? (
        <g className="[transform-box:view-box] [transform-origin:31px_23px] motion-safe:animate-[spin_9s_linear_infinite]">
          <ellipse
            cx={CX}
            cy={CY}
            rx="29"
            ry="9"
            fill="none"
            strokeWidth="0.9"
            strokeDasharray="2 2.5"
            transform={`rotate(-18 ${CX} ${CY})`}
            className="stroke-[color-mix(in_srgb,var(--color-accent)_80%,transparent)]"
          />
          <circle cx={CX + 27.5} cy={CY - 9} r="2.4" className="fill-[var(--color-accent)]" />
        </g>
      ) : null}
    </svg>
  );
}
