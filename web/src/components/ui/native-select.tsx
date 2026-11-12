import * as React from "react"
import { ChevronDownIcon } from "lucide-react"

import { cn } from "@/lib/utils"

// True when the caller explicitly controls the right / inline-end gutter that
// reserves space for <ChevronDownIcon>; shorthand `p-*` / `px-*` tokens passed
// by callers sharing input field classes must not strip `pr-8` / `pr-7`.
const EXPLICIT_RIGHT_PADDING_RE =
  /(?:^|\s)!?(?:pr|pe)-\S+|(?:^|\s)!?\[padding-(?:right|inline-end):[^\]]+\]/
const EXPLICIT_SM_RIGHT_PADDING_RE =
  /(?:^|\s)data-\[size=sm\]:!?(?:pr|pe)-\S+/

// Flag COMPONENTS.md "Form Inputs" / "Dropdowns": native <select> with a
// chevron affordance, matching Flag's field shell (2.5rem default, 2rem sm,
// overlay-soft fill, --input edge, --focus-ring). Tier A: .select-wrap >
// select + .select-icon, .select-wrap--sm.
function NativeSelect({
  className,
  wrapperClassName,
  size = "default",
  ...props
}: Omit<React.ComponentProps<"select">, "size"> & {
  size?: "default" | "sm"
  wrapperClassName?: string
}) {
  const hasExplicitRightPadding = className
    ? EXPLICIT_RIGHT_PADDING_RE.test(className)
    : false
  const hasExplicitSmRightPadding = className
    ? EXPLICIT_SM_RIGHT_PADDING_RE.test(className)
    : false

  return (
    <div
      data-slot="native-select-wrapper"
      className={cn(
        "group/native-select relative w-full has-[>select:not(.w-full)]:w-fit",
        wrapperClassName
      )}
    >
      <select
        data-slot="native-select"
        data-size={size}
        className={cn(
          "peer w-full min-w-0 cursor-pointer appearance-none rounded-md border border-input bg-(--color-overlay-soft) py-1 pr-8 pl-3 text-body-md text-foreground [transition:var(--transition-fast)] outline-none enabled:hover:border-(--color-border-strong) focus-visible:border-ring focus-visible:shadow-(--focus-ring) disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:enabled:hover:border-destructive data-[size=default]:h-10 data-[size=sm]:h-8 data-[size=sm]:rounded-sm data-[size=sm]:pr-7 data-[size=sm]:pl-2.5 data-[size=sm]:text-caption [&_optgroup]:bg-popover [&_optgroup]:text-popover-foreground [&_option]:bg-popover [&_option]:text-popover-foreground",
          className,
          !hasExplicitRightPadding && "pr-8",
          !hasExplicitRightPadding &&
            !hasExplicitSmRightPadding &&
            "data-[size=sm]:pr-7"
        )}
        {...props}
      />
      <ChevronDownIcon
        data-slot="native-select-icon"
        aria-hidden="true"
        className="pointer-events-none absolute top-1/2 right-2.5 size-4 -translate-y-1/2 text-muted-foreground select-none peer-disabled:opacity-50"
      />
    </div>
  )
}

function NativeSelectOption({
  ...props
}: React.ComponentProps<"option">) {
  return <option data-slot="native-select-option" {...props} />
}

function NativeSelectOptGroup({
  className,
  ...props
}: React.ComponentProps<"optgroup">) {
  return (
    <optgroup
      data-slot="native-select-optgroup"
      className={cn("bg-popover text-popover-foreground", className)}
      {...props}
    />
  )
}

export { NativeSelect, NativeSelectOptGroup, NativeSelectOption }
