"use client"

import * as React from "react"
import { CheckIcon, MinusIcon } from "lucide-react"
import { Checkbox as CheckboxPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Checkbox": 1.5px --color-control-border edge (3:1 on
// every surface), --color-accent edge on the running tint when checked or
// indeterminate, focus ring on keyboard focus only, no hover while disabled.
// Tier A: .ds-checkbox / .ds-checkbox__box.
function Checkbox({
  className,
  ...props
}: React.ComponentProps<typeof CheckboxPrimitive.Root>) {
  return (
    <CheckboxPrimitive.Root
      data-slot="checkbox"
      className={cn(
        "peer group/checkbox size-4 shrink-0 cursor-pointer rounded-[0.25rem] border-[1.5px] border-(--color-control-border) bg-(--color-overlay-soft) text-foreground [transition:var(--transition-fast)] outline-none enabled:hover:border-(--color-accent) focus-visible:shadow-(--focus-ring) disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive data-[state=checked]:border-(--color-accent) data-[state=checked]:bg-(--color-status-running-bg) data-[state=indeterminate]:border-(--color-accent) data-[state=indeterminate]:bg-(--color-status-running-bg)",
        className
      )}
      {...props}
    >
      <CheckboxPrimitive.Indicator
        data-slot="checkbox-indicator"
        className="grid place-content-center text-current"
      >
        <CheckIcon
          className="hidden size-3 group-data-[state=checked]/checkbox:block"
          strokeWidth={3}
        />
        <MinusIcon
          className="hidden size-3 group-data-[state=indeterminate]/checkbox:block"
          strokeWidth={3}
        />
      </CheckboxPrimitive.Indicator>
    </CheckboxPrimitive.Root>
  )
}

export { Checkbox }
