import { cn } from "@/lib/utils"

// Loading placeholder: surface-2 block that pulses unless reduced motion is
// requested. Pair with polite live-region text for long loads.
// Tier A: .skeleton.
function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="skeleton"
      aria-hidden="true"
      className={cn("rounded-md bg-muted motion-safe:animate-pulse", className)}
      {...props}
    />
  )
}

export { Skeleton }
