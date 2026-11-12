import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"

import { cn } from "@/lib/utils"

// Flag "Error States" banner and the state-panel look: 1px border on a tint,
// radius-md, caption-to-body text, one short message plus corrective
// guidance. Tier A: .alert .alert--<variant>.
const alertVariants = cva(
  "relative grid w-full grid-cols-[0_1fr] items-start gap-y-0.5 rounded-md border px-3.5 py-2.5 text-body-md has-[>svg]:grid-cols-[calc(var(--spacing)*4)_1fr] has-[>svg]:gap-x-3 [&>svg]:size-4 [&>svg]:translate-y-0.5 [&>svg]:text-current",
  {
    variants: {
      variant: {
        default: "border-border bg-card text-card-foreground",
        info: "border-(--color-status-assigned-border) bg-(--color-status-assigned-bg) text-(--color-status-assigned-fg)",
        success:
          "border-(--color-status-success-border) bg-(--color-status-success-bg) text-(--color-status-success-fg)",
        warning:
          "border-(--color-status-warning-border) bg-(--color-status-warning-bg) text-(--color-status-warning-fg)",
        destructive:
          "border-(--color-status-error-border) bg-(--color-status-error-bg) text-(--color-status-error-fg)",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function Alert({
  className,
  variant,
  ...props
}: React.ComponentProps<"div"> & VariantProps<typeof alertVariants>) {
  return (
    <div
      data-slot="alert"
      data-variant={variant ?? "default"}
      role="alert"
      className={cn(alertVariants({ variant }), className)}
      {...props}
    />
  )
}

function AlertTitle({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="alert-title"
      className={cn(
        "col-start-2 line-clamp-1 min-h-4 font-semibold tracking-tight",
        className
      )}
      {...props}
    />
  )
}

// Description inherits the variant's text color: the status fg tokens are
// the ones measured at 4.5:1 on their own tint.
function AlertDescription({
  className,
  ...props
}: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="alert-description"
      className={cn(
        "col-start-2 grid justify-items-start gap-1 text-body-md in-data-[variant=default]:text-subtle [&_p]:leading-relaxed",
        className
      )}
      {...props}
    />
  )
}

export { Alert, AlertTitle, AlertDescription }
