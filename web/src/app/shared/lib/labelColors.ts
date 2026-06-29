// Deterministic categorical colors for conversation labels (#258/#279 UI).
//
// Labels are free text with no registry, so their color is derived from a hash
// of the name — the same label always renders the same chip color, across
// sessions and across the sidebar/menu without any stored state. The palette is
// deliberately distinct from the purple accent ramp so chips read as their own
// categorical system rather than as accent UI.
//
// This is the one place raw hex is allowed in component-adjacent code: it is a
// fixed categorical palette, not a themeable surface/text color, so it is
// isolated here (and documented) rather than scattered through components. The
// later theming PR repaints the semantic tokens; this palette stays put.

export const LABEL_COLORS = [
  "#4FB0AE",
  "#E08AC0",
  "#E0A55A",
  "#7FB069",
  "#E07A7A",
  "#5AA9D8",
  "#9B6FD9",
  "#6FBF8E",
  "#D9794F",
  "#A9C24F",
] as const;

// labelColor maps a label name to a stable palette entry via a small string
// hash (the classic 31-multiplier rolling hash, masked to an unsigned 32-bit
// int so it never goes negative). Empty names fall back to the first color.
export function labelColor(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i += 1) {
    h = (h * 31 + name.charCodeAt(i)) >>> 0;
  }
  return LABEL_COLORS[h % LABEL_COLORS.length];
}

// labelChipStyle returns the inline style that drives a chip's color. The chip
// CSS reads the `--chip` custom property and mixes it for fill/border/text so a
// single variable themes the whole chip in both light and dark mode.
export function labelChipStyle(name: string): React.CSSProperties {
  return { ["--chip" as string]: labelColor(name) } as React.CSSProperties;
}
