import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { Slot } from "radix-ui"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Badges/Pills/Chips": pill radius, caption text, 1px
// border; status variants use the status fg/bg/border token triplets
// (spelled out in full so Tailwind's scanner sees every class).
// Tier A: .badge with .badge-<status>, .badge-default, .badge-secondary,
// .badge-outline or .badge-accent.
const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-full border px-[0.5625rem] py-0.5 text-caption whitespace-nowrap [transition:var(--transition-fast)] focus-visible:shadow-(--focus-ring) focus-visible:outline-none [&>svg]:pointer-events-none [&>svg]:size-3",
  {
    variants: {
      variant: {
        default:
          "border-transparent bg-primary text-primary-foreground [a&]:hover:bg-(--primary-hover)",
        secondary:
          "border-border bg-secondary text-secondary-foreground [a&]:hover:border-(--color-accent)",
        destructive: "border-(--color-status-error-border) bg-(--color-status-error-bg) text-(--color-status-error-fg)",
        outline:
          "border-border text-foreground [a&]:hover:bg-accent [a&]:hover:text-accent-foreground",
        ghost:
          "border-transparent [a&]:hover:bg-accent [a&]:hover:text-accent-foreground",
        // Accent fill: label with --accent-solid-foreground (theme.css; 8.3:1
        // dark, 5.3:1 light).
        accent:
          "border-transparent bg-accent-solid text-accent-solid-foreground underline-offset-2 [a&]:hover:underline",
        link: "border-transparent text-link underline-offset-4 [a&]:hover:underline",
        // Task status set (Flag TOKENS.md "Status mapping").
        pending: "border-(--color-status-pending-border) bg-(--color-status-pending-bg) text-(--color-status-pending-fg)",
        scheduled: "border-(--color-status-scheduled-border) bg-(--color-status-scheduled-bg) text-(--color-status-scheduled-fg)",
        assigned: "border-(--color-status-assigned-border) bg-(--color-status-assigned-bg) text-(--color-status-assigned-fg)",
        leased: "border-(--color-status-leased-border) bg-(--color-status-leased-bg) text-(--color-status-leased-fg)",
        running: "border-(--color-status-running-border) bg-(--color-status-running-bg) text-(--color-status-running-fg)",
        analyzing: "border-(--color-status-analyzing-border) bg-(--color-status-analyzing-bg) text-(--color-status-analyzing-fg)",
        success: "border-(--color-status-success-border) bg-(--color-status-success-bg) text-(--color-status-success-fg)",
        warning: "border-(--color-status-warning-border) bg-(--color-status-warning-bg) text-(--color-status-warning-fg)",
        error: "border-(--color-status-error-border) bg-(--color-status-error-bg) text-(--color-status-error-fg)",
        cancelled: "border-(--color-status-cancelled-border) bg-(--color-status-cancelled-bg) text-(--color-status-cancelled-fg)",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Badge({
  className,
  variant = "default",
  asChild = false,
  ...props
}: React.ComponentProps<"span"> &
  VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot.Root : "span"

  return (
    <Comp
      data-slot="badge"
      data-variant={variant}
      className={cn(badgeVariants({ variant }), className)}
      {...props}
    />
  )
}

export { Badge, badgeVariants }
