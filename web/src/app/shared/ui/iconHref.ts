// iconHref resolves an icon name to its symbol in Flag's core-icons sprite,
// vendored verbatim (public/icons/core-icons.svg, pinned in flag.lock).
//
// ICON_ALIASES maps names Flag deliberately does not carry because they
// duplicate one of its glyphs onto that glyph. fleet's own source uses the
// Flag ids (spriteCoverage.test.ts forbids an alias there); the aliases exist
// for names that arrive as DATA — a client bundle's empty-state card that says
// the name "book-open" (example-config's does) keeps rendering an icon instead
// of an empty box.
export const ICON_ALIASES: Readonly<Record<string, string>> = {
  "book-open": "book",
  warning: "alert-triangle",
  wrench: "tools",
};

export function iconHref(name: string): string {
  return `/icons/core-icons.svg#${ICON_ALIASES[name] ?? name}`;
}
