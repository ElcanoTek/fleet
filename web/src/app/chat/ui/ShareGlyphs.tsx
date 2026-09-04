"use client";

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

// ShareGlyph is the chain link: sharing by URL. `off` adds the slash for the
// "stop sharing" variant.
export function ShareGlyph({ className, off }: { className?: string; off?: boolean }) {
  return (
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
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
    <svg
      aria-hidden="true"
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M16 19v-1.5a3.5 3.5 0 0 0-3.5-3.5h-5A3.5 3.5 0 0 0 4 17.5V19" />
      <circle cx="10" cy="8" r="3" />
      <path d="M20 19v-1.5a3.5 3.5 0 0 0-2.6-3.4" />
      <path d="M15.5 5.2a3 3 0 0 1 0 5.6" />
    </svg>
  );
}
