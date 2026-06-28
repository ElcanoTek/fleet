"use client";

// Icon renders one of the symbols from the bundled core-icons SVG sprite via
// an <svg><use> reference. Extracted from chat-experience.tsx (slice 3 of
// #169) so the chat presentational components can share it without pulling in
// the whole monolith. Behavior is byte-identical to the original in-module
// definition.
export function Icon({ name, className }: { name: string; className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
      <use href={`/icons/core-icons.svg#${name}`} />
    </svg>
  );
}
