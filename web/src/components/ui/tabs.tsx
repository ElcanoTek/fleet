"use client"

import * as React from "react"
import { cva, type VariantProps } from "class-variance-authority"
import { Tabs as TabsPrimitive } from "radix-ui"

import { cn } from "@/lib/utils"

const TabsClickContext = React.createContext<((value: string) => void) | undefined>(
  undefined
)

function Tabs({
  className,
  orientation = "horizontal",
  value,
  defaultValue,
  onValueChange,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Root>) {
  const [uncontrolledValue, setUncontrolledValue] = React.useState(defaultValue)
  const isControlled = value !== undefined
  const currentValue = isControlled ? value : uncontrolledValue
  const handleValueChange = React.useCallback(
    (next: string) => {
      if (!isControlled) {
        setUncontrolledValue(next)
      }
      onValueChange?.(next)
    },
    [isControlled, onValueChange]
  )

  return (
    <TabsClickContext.Provider value={handleValueChange}>
      <TabsPrimitive.Root
        data-slot="tabs"
        data-orientation={orientation}
        orientation={orientation}
        value={currentValue}
        onValueChange={handleValueChange}
        className={cn(
          "group/tabs flex gap-3 data-[orientation=horizontal]:flex-col",
          className
        )}
        {...props}
      />
    </TabsClickContext.Provider>
  )
}

// default: Flag's segmented control (pill track, selected option on the
// primary fill). line: Flag page tabs (accent underline on the active tab).
// Tier A: .seg / .seg-option, and .tabs / .tab.
const tabsListVariants = cva(
  "group/tabs-list inline-flex w-fit items-center justify-center text-muted-foreground group-data-[orientation=vertical]/tabs:h-fit group-data-[orientation=vertical]/tabs:flex-col",
  {
    variants: {
      variant: {
        default: "rounded-full border border-border",
        line: "gap-1 border-b border-border group-data-[orientation=vertical]/tabs:border-r group-data-[orientation=vertical]/tabs:border-b-0",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  }
)

function TabsList({
  className,
  variant = "default",
  ...props
}: React.ComponentProps<typeof TabsPrimitive.List> &
  VariantProps<typeof tabsListVariants>) {
  return (
    <TabsPrimitive.List
      data-slot="tabs-list"
      data-variant={variant}
      className={cn(tabsListVariants({ variant }), className)}
      {...props}
    />
  )
}

function TabsTrigger({
  className,
  onClick,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  const onTabClick = React.useContext(TabsClickContext)

  return (
    <TabsPrimitive.Trigger
      data-slot="tabs-trigger"
      onClick={(e) => {
        onClick?.(e)
        if (
          !e.defaultPrevented &&
          !props.disabled &&
          e.currentTarget.getAttribute("data-state") !== "active"
        ) {
          onTabClick?.(props.value)
        }
      }}
      className={cn(
        "relative inline-flex cursor-pointer items-center justify-center gap-1.5 font-medium whitespace-nowrap text-muted-foreground [transition:var(--transition-fast)] outline-none group-data-[orientation=vertical]/tabs:w-full group-data-[orientation=vertical]/tabs:justify-start focus-visible:z-10 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-(--color-accent) disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
        // default (segmented): overline text, primary fill when selected.
        "group-data-[variant=default]/tabs-list:rounded-full group-data-[variant=default]/tabs-list:px-3 group-data-[variant=default]/tabs-list:py-1 group-data-[variant=default]/tabs-list:text-caption group-data-[variant=default]/tabs-list:data-[state=active]:bg-primary group-data-[variant=default]/tabs-list:data-[state=active]:text-primary-foreground group-data-[variant=default]/tabs-list:data-[state=inactive]:hover:text-foreground",
        // line (page tabs): accent underline (a right-edge bar when vertical)
        // and primary text when active.
        "group-data-[variant=line]/tabs-list:border-transparent group-data-[orientation=horizontal]/tabs:group-data-[variant=line]/tabs-list:-mb-px group-data-[orientation=horizontal]/tabs:group-data-[variant=line]/tabs-list:border-b-2 group-data-[orientation=vertical]/tabs:group-data-[variant=line]/tabs-list:-mr-px group-data-[orientation=vertical]/tabs:group-data-[variant=line]/tabs-list:border-r-2 group-data-[variant=line]/tabs-list:px-3 group-data-[variant=line]/tabs-list:py-2 group-data-[variant=line]/tabs-list:text-body-md group-data-[variant=line]/tabs-list:hover:bg-(--color-overlay-soft) group-data-[variant=line]/tabs-list:hover:text-foreground group-data-[variant=line]/tabs-list:data-[state=active]:border-(--color-accent) group-data-[variant=line]/tabs-list:data-[state=active]:text-foreground",
        className
      )}
      {...props}
    />
  )
}

function TabsContent({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return (
    <TabsPrimitive.Content
      data-slot="tabs-content"
      className={cn(
        "flex-1 rounded-md outline-none focus-visible:shadow-(--focus-ring)",
        className
      )}
      {...props}
    />
  )
}

export { Tabs, TabsList, TabsTrigger, TabsContent, tabsListVariants }
