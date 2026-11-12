import * as React from "react"

import { cn } from "@/lib/utils"

// Flag COMPONENTS.md "Tables and List Rows": bordered, radius-md wrapper;
// uppercase caption-size muted head; 0.625rem cells; tabular figures; row
// hover only for clickable rows (pass `interactive`). Pass `scrollLabel` when
// a table may overflow horizontally so keyboard users can focus and scroll the
// region without turning static tables into dead tab stops. Tier A:
// .table-wrap > table, tr.is-interactive.
function Table({
  className,
  containerClassName,
  scrollLabel,
  ...props
}: React.ComponentProps<"table"> & {
  containerClassName?: string
  scrollLabel?: string
}) {
  return (
    <div
      data-slot="table-container"
      {...(scrollLabel
        ? { role: "region", "aria-label": scrollLabel, tabIndex: 0 }
        : undefined)}
      className={cn(
        "relative w-full overflow-x-auto rounded-md border border-border outline-none focus-visible:shadow-(--focus-ring)",
        containerClassName
      )}
    >
      <table
        data-slot="table"
        className={cn(
          "w-full caption-bottom border-collapse text-body-md tabular-nums",
          className
        )}
        {...props}
      />
    </div>
  )
}

function TableHeader({ className, ...props }: React.ComponentProps<"thead">) {
  return (
    <thead
      data-slot="table-header"
      className={cn("[&_tr]:border-b", className)}
      {...props}
    />
  )
}

function TableBody({ className, ...props }: React.ComponentProps<"tbody">) {
  return (
    <tbody
      data-slot="table-body"
      className={cn("[&_tr:last-child]:border-0", className)}
      {...props}
    />
  )
}

function TableFooter({ className, ...props }: React.ComponentProps<"tfoot">) {
  return (
    <tfoot
      data-slot="table-footer"
      className={cn(
        "border-t bg-surface-inset font-semibold [&>tr]:last:border-b-0",
        className
      )}
      {...props}
    />
  )
}

function TableRow({
  className,
  interactive = false,
  ...props
}: React.ComponentProps<"tr"> & { interactive?: boolean }) {
  return (
    <tr
      data-slot="table-row"
      data-interactive={interactive || undefined}
      className={cn(
        "border-b border-border [transition:background-color_var(--transition-fast)] data-[interactive]:cursor-pointer data-[interactive]:hover:bg-(--color-overlay-soft) data-[interactive]:active:bg-accent data-[state=selected]:bg-accent",
        className
      )}
      {...props}
    />
  )
}

function TableHead({ className, ...props }: React.ComponentProps<"th">) {
  return (
    <th
      data-slot="table-head"
      scope="col"
      className={cn(
        "h-10 px-2.5 text-left align-middle text-caption font-normal whitespace-nowrap text-muted-foreground uppercase [&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[2px]",
        className
      )}
      {...props}
    />
  )
}

function TableCell({ className, ...props }: React.ComponentProps<"td">) {
  return (
    <td
      data-slot="table-cell"
      className={cn(
        "p-2.5 align-middle whitespace-nowrap [&:has([role=checkbox])]:pr-0 [&>[role=checkbox]]:translate-y-[2px]",
        className
      )}
      {...props}
    />
  )
}

function TableCaption({
  className,
  ...props
}: React.ComponentProps<"caption">) {
  return (
    <caption
      data-slot="table-caption"
      className={cn("my-3 text-caption text-muted-foreground", className)}
      {...props}
    />
  )
}

export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableHead,
  TableRow,
  TableCell,
  TableCaption,
}
