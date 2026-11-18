// The two share audiences, as two distinct glyphs (ADR-0057).
//
// A chat can be shared BY LINK (anyone with the URL) or WITH THE TEAM (people
// in your team, read-only), independently — one, both, or neither. Until now a
// single unlabeled chain-link icon stood for the only one that existed, which
// meant that inside a team-shared project a user could reasonably read it as
// "my team can see this". They couldn't; it was a public link. Two shapes,
// each always labeled with its audience, is what makes the distinction
// legible.
//
// ShareGlyph moved here verbatim from ConversationSidebar so every surface
// that shows a share state draws the same mark.

// ── The glyphs fleet draws itself ────────────────────────────────────────────
// Every other icon in fleet comes from Flag's sprite (web/AGENTS.md). The ones
// in this file are the few the sprite has no symbol for. So that a drawn glyph
// and a sprite icon look like one set wherever they sit side by side, they are
// all drawn to the sprite's own spec, in ONE place: a 24-unit box, no fill,
// currentColor, round caps and joins, and the sprite's 1.9 stroke. A new glyph
// goes here, with these props, and is offered to Flag; when Flag ships it, the
// component is replaced by the sprite id.
const GLYPH = {
  "aria-hidden": true,
  viewBox: "0 0 24 24",
  fill: "none",
  stroke: "currentColor",
  strokeWidth: 1.9,
  strokeLinecap: "round",
  strokeLinejoin: "round",
} as const;

// ShareGlyph is the chain link: sharing by URL. `off` adds the slash for the
// "stop sharing" variant.
export function ShareGlyph({ className, off }: { className?: string; off?: boolean }) {
  return (
    <svg {...GLYPH} className={className}>
      <path d="M10 13a5 5 0 0 0 7.07 0l2-2a5 5 0 0 0-7.07-7.07l-1 1" />
      <path d="M14 11a5 5 0 0 0-7.07 0l-2 2a5 5 0 0 0 7.07 7.07l1-1" />
      {off ? <path d="M4 4l16 16" /> : null}
    </svg>
  );
}

// TeamGlyph is two people: sharing with the team. Deliberately nothing like a
// chain link — the point of the pair is that they cannot be mistaken for each
// other at 12px.
export function TeamGlyph({ className }: { className?: string }) {
  return (
    <svg {...GLYPH} className={className}>
      <path d="M16 19v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 17.5V19" />
      <circle cx="10" cy="8" r="3" />
      <path d="M20 19v-1.5a3.5 3.5 0 0 0-2.6-3.4" />
      <path d="M15.5 5.2a3 3 0 0 1 0 5.6" />
    </svg>
  );
}

// LockGlyph is "Only you" — the private state, and the mark an unavailable
// share control carries (A3a). Drawn here beside the two audience glyphs it
// sits next to in the same chips, to the same spec as they are.
export function LockGlyph({ className }: { className?: string }) {
  return (
    <svg {...GLYPH} className={className}>
      <rect x="5" y="11" width="14" height="9" rx="2" />
      <path d="M8 11V8a4 4 0 0 1 8 0v3" />
    </svg>
  );
}

// UnarchiveGlyph is the archive box with an arrow leaving it: bring an
// archived chat back. The sprite's `archive` is the box going in; it has no
// way out, so this is its counterpart, drawn on the same box.
export function UnarchiveGlyph({ className }: { className?: string }) {
  return (
    <svg {...GLYPH} className={className}>
      <rect x="3" y="4" width="18" height="4" rx="1" />
      <path d="M5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8" />
      <path d="M12 18v-6" />
      <path d="M9.5 14.5 12 12l2.5 2.5" />
    </svg>
  );
}
