import * as React from "react"

import { cn } from "@/lib/utils"

// Same contract as Input, multi-line. Tier A: textarea (bare element).
function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "flex field-sizing-content min-h-20 w-full rounded-md border border-input bg-(--color-overlay-soft) px-3 py-2 text-base text-foreground [transition:var(--transition-fast)] outline-none placeholder:text-muted-foreground focus-visible:border-(--color-accent) focus-visible:shadow-(--focus-ring) disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive",
        className
      )}
      {...props}
    />
  )
}

export { Textarea }
