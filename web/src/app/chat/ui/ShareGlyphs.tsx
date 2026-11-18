// The marks for who can see a chat, a project or a file — and the names fleet
// gives them. Every one is an icon from Flag's sprite (web/AGENTS.md); this
// file draws nothing. It exists so that every surface that shows a share
// state uses the same mark for the same audience, by name, instead of each
// picking a sprite id.
//
// The two share audiences are two distinct glyphs (ADR-0057). A chat can be
// shared BY LINK (anyone with the URL) or WITH THE TEAM (people in your team,
// read-only), independently — one, both, or neither. A single unlabeled
// chain-link icon once stood for the only one that existed, which meant that
// inside a team-shared project a user could reasonably read it as "my team
// can see this". They couldn't; it was a public link. Two shapes, each always
// labeled with its audience, is what makes the distinction legible.

import { Icon } from "./Icon";

// ShareGlyph is the chain link: sharing by URL.
export function ShareGlyph({ className }: { className?: string }) {
  return <Icon name="link" className={className} />;
}

// TeamGlyph is two people: sharing with the team. Deliberately nothing like a
// chain link — the point of the pair is that they cannot be mistaken for each
// other at 12px.
export function TeamGlyph({ className }: { className?: string }) {
  return <Icon name="users" className={className} />;
}

// LockGlyph is "Only you" — the private state, and the mark an unavailable
// share control carries (A3a).
export function LockGlyph({ className }: { className?: string }) {
  return <Icon name="lock" className={className} />;
}
