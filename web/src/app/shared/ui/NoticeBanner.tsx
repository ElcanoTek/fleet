import type { HTMLAttributes, ReactNode } from "react";

// NoticeBanner — the one page-level notice/error banner recipe. Pages used to
// hand-roll `rounded-[0.95rem] border px-4 py-3 text-[0.875rem]` with raw hex
// status colors; the shape now sits on the shared radius scale and the colors
// on the semantic --color-*-strong / --color-*-soft token pairs, so banners
// theme with light/dark automatically. See web/src/app/DESIGN.md.
//
// Purely presentational: pass `className` for layout (margins/width) and any
// div attributes (role, data-testid) through as-is — no behavior is added.

export type NoticeTone = "success" | "warning" | "danger";

const TONE_CLASS: Record<NoticeTone, string> = {
  success:
    "border-[var(--color-success-strong)] bg-[color-mix(in_srgb,var(--color-success-strong)_15%,transparent)] text-[var(--color-success-soft)]",
  warning:
    "border-[var(--color-warning-strong)] bg-[color-mix(in_srgb,var(--color-warning-strong)_15%,transparent)] text-[var(--color-warning-soft)]",
  danger:
    "border-[var(--color-danger-strong)] bg-[color-mix(in_srgb,var(--color-danger-strong)_15%,transparent)] text-[var(--color-danger-soft)]",
};

export function NoticeBanner({
  tone,
  children,
  className,
  ...rest
}: {
  tone: NoticeTone;
  children: ReactNode;
  className?: string;
} & HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      {...rest}
      className={`rounded-[var(--radius-lg)] border px-4 py-3 text-[0.875rem] ${TONE_CLASS[tone]}${className ? ` ${className}` : ""}`}
    >
      {children}
    </div>
  );
}
