import * as React from "react"
import { RadioGroup as RadioGroupPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

function RadioGroup({
  className,
  ...props
}: React.ComponentProps<typeof RadioGroupPrimitive.Root>) {
  return (
    <RadioGroupPrimitive.Root
      data-slot="radio-group"
      className={cn("grid gap-3", className)}
      {...props}
    />
  )
}

// Flag COMPONENTS.md "Custom File Radio Select": 1rem circle, 1.5px
// --color-control-border edge on surface-1, a secondary edge and dot and the
// selected halo (--shadow-control-selected) when checked; disabled draws a
// muted ring and dot. Tier A: input[type=radio].ds-radio.
function RadioGroupItem({
  className,
  ...props
}: React.ComponentProps<typeof RadioGroupPrimitive.Item>) {
  return (
    <RadioGroupPrimitive.Item
      data-slot="radio-group-item"
      className={cn(
        "group/radio aspect-square size-4 shrink-0 cursor-pointer rounded-full border-[1.5px] border-(--color-control-border) bg-surface-1 [transition:var(--transition-fast)] outline-none enabled:hover:border-(--color-accent) focus-visible:shadow-(--focus-ring) disabled:cursor-not-allowed disabled:border-border disabled:bg-(--color-overlay-soft) disabled:shadow-none aria-invalid:border-destructive data-[state=checked]:border-(--color-secondary) data-[state=checked]:shadow-(--shadow-control-selected) data-[state=checked]:focus-visible:shadow-(--focus-ring) disabled:data-[state=checked]:shadow-none",
        className
      )}
      {...props}
    >
      <RadioGroupPrimitive.Indicator
        data-slot="radio-group-indicator"
        className="flex size-full items-center justify-center"
      >
        <span className="size-[38%] rounded-full bg-(--color-secondary) group-disabled/radio:bg-(--color-text-disabled)" />
      </RadioGroupPrimitive.Indicator>
    </RadioGroupPrimitive.Item>
  )
}

export { RadioGroup, RadioGroupItem }
