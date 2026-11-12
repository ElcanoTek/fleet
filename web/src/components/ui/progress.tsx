"use client"

import * as React from "react"
import { Progress as ProgressPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

// Pill progress bar: surface-2 track with --color-primary-fill indicator
// (measured >= 3:1 against the track in both themes). Pass `aria-label` or
// `aria-labelledby` so assistive technology can name the progressbar.
// Tier A: .progress > .progress__bar.
function Progress({
  className,
  value,
  max = 100,
  ...props
}: React.ComponentProps<typeof ProgressPrimitive.Root>) {
  const safeMax =
    typeof max === "number" && Number.isFinite(max) && max > 0 ? max : 100
  const safeValue =
    typeof value === "number" && Number.isFinite(value)
      ? Math.min(safeMax, Math.max(0, value))
      : null
  const pct = safeValue === null ? 0 : (safeValue / safeMax) * 100
  return (
    <ProgressPrimitive.Root
      data-slot="progress"
      value={safeValue}
      max={safeMax}
      className={cn(
        "relative h-2 w-full overflow-hidden rounded-full border border-border bg-surface-2",
        className
      )}
      {...props}
    >
      <ProgressPrimitive.Indicator
        data-slot="progress-indicator"
        className="h-full w-full flex-1 bg-primary [transition:transform_var(--transition-base)]"
        style={{ transform: `translateX(-${100 - pct}%)` }}
      />
    </ProgressPrimitive.Root>
  )
}

export { Progress }
