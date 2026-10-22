// iconHref resolves an icon name to its sprite symbol. Almost every glyph is in
// Flag's core-icons sprite, vendored verbatim (public/icons/core-icons.svg,
// pinned in flag.lock). The few fleet needs that Flag does not carry yet live
// in fleet's own sprite (public/icons/fleet-icons.svg) and are requested
// upstream; once Flag ships one, it moves out of FLEET_SPRITE_IDS and out of
// that file. spriteCoverage.test.ts pins both directions.

export const FLEET_SPRITE_IDS: ReadonlySet<string> = new Set(["book-open", "warning", "wrench"]);

export function iconHref(name: string): string {
  return FLEET_SPRITE_IDS.has(name) ? `/icons/fleet-icons.svg#${name}` : `/icons/core-icons.svg#${name}`;
}
