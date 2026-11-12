"use client"

import * as React from "react"
import { Switch as SwitchPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Switch": 30x18 (lg 38x22), on/off track tokens that
// hold 3:1, a 14px (16px) knob inset 2px (3px), a hover halo, a dimmed
// pressed track and --focus-ring-picker. Tier A: .switch / .switch--lg.
function Switch({
  className,
  size = "default",
  ...props
}: React.ComponentProps<typeof SwitchPrimitive.Root> & {
  size?: "default" | "lg"
}) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      data-size={size}
      className={cn(
        "peer group/switch relative inline-flex shrink-0 cursor-pointer items-center rounded-full border-0 [transition:var(--transition-fast)] outline-none enabled:hover:shadow-[0_0_0_3px_var(--color-pill-hover-bg)] focus-visible:shadow-(--focus-ring-picker) enabled:active:brightness-[0.92] disabled:cursor-not-allowed disabled:opacity-50 data-[size=default]:h-[18px] data-[size=default]:w-[30px] data-[size=lg]:h-[22px] data-[size=lg]:w-[38px] data-[state=checked]:bg-(--color-switch-track-on) data-[state=unchecked]:bg-(--color-switch-track-off)",
        className
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className="pointer-events-none block rounded-full bg-(--color-switch-knob) shadow-(--shadow-switch-knob) [transition:transform_var(--transition-base)] group-data-[size=default]/switch:ml-[2px] group-data-[size=default]/switch:size-[14px] group-data-[size=default]/switch:data-[state=checked]:translate-x-[12px] group-data-[size=lg]/switch:ml-[3px] group-data-[size=lg]/switch:size-[16px] group-data-[size=lg]/switch:data-[state=checked]:translate-x-[16px]"
      />
    </SwitchPrimitive.Root>
  )
}

export { Switch }
