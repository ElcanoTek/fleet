import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { Slot } from "radix-ui"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Buttons": 2.5rem controls, radius-md, 1px border,
// hover lifts 1px and strengthens the border, active presses back, focus is
// --focus-ring, disabled is 50% with no lift. Tier A: .btn with .btn-primary,
// -secondary, -outline, -ghost, -danger or -link; sizes .btn-sm, .btn-lg, .btn-icon.
const buttonVariants = cva(
  "inline-flex shrink-0 cursor-pointer items-center justify-center gap-2 rounded-md border border-border font-sans whitespace-nowrap [transition:var(--transition-fast)] outline-none select-none focus-visible:shadow-(--focus-ring) motion-safe:hover:-translate-y-px motion-safe:active:translate-y-0 motion-safe:active:scale-[0.98] disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 aria-disabled:pointer-events-none aria-disabled:opacity-50 aria-invalid:border-destructive [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default:
          "bg-primary bg-(image:--primary-gradient) text-primary-foreground hover:border-(--color-accent) hover:bg-(--primary-hover) hover:bg-(image:--primary-gradient-hover)",
        destructive:
          "border-(--color-status-error-border) bg-(--color-status-error-bg) text-(--color-status-error-fg) hover:border-destructive",
        outline:
          "border-border-strong bg-transparent text-foreground hover:border-(--color-accent) hover:bg-(--color-overlay-soft)",
        secondary:
          "bg-secondary text-secondary-foreground hover:border-(--color-accent) hover:bg-surface-1",
        ghost:
          "bg-transparent text-subtle hover:border-(--color-accent) hover:bg-(--color-overlay-soft) hover:text-foreground",
        link: "border-transparent bg-transparent text-link underline-offset-4 hover:underline motion-safe:hover:translate-y-0",
      },
      size: {
        default: "h-10 px-3 text-base",
        xs: "h-6 gap-1 rounded-sm px-2 text-overline [&_svg:not([class*='size-'])]:size-3",
        sm: "h-8 gap-1.5 rounded-sm px-2.5 text-caption",
        lg: "h-11 px-4 text-base",
        icon: "size-10",
        "icon-xs": "size-6 rounded-sm [&_svg:not([class*='size-'])]:size-3",
        "icon-sm": "size-8 rounded-sm",
        "icon-lg": "size-11",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  }
)

function Button({
  className,
  variant = "default",
  size = "default",
  asChild = false,
  ...props
}: React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & {
    asChild?: boolean
  }) {
  const Comp = asChild ? Slot.Root : "button"

  return (
    <Comp
      data-slot="button"
      data-variant={variant}
      data-size={size}
      className={cn(buttonVariants({ variant, size, className }))}
      {...props}
    />
  )
}

export { Button, buttonVariants }
