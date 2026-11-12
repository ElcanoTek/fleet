import * as React from "react"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Form Inputs": min 2.5rem, radius-md, overlay-soft fill,
// token border, accent border + --focus-ring on keyboard focus, danger border
// when invalid, 50% when disabled. No hover, like Flag's Tier A fields. Tier A: input (bare element).
function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "h-10 w-full min-w-0 rounded-md border border-input bg-(--color-overlay-soft) px-3 text-base text-foreground [transition:var(--transition-fast)] outline-none selection:bg-primary selection:text-primary-foreground file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-caption file:font-medium file:text-foreground placeholder:text-muted-foreground disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50",
        "focus-visible:border-(--color-accent) focus-visible:shadow-(--focus-ring)",
        "aria-invalid:border-destructive",
        className
      )}
      {...props}
    />
  )
}

export { Input }
