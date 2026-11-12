"use client"

import * as React from "react"
import { CheckIcon, CopyIcon } from "lucide-react"

import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"

// Clipboard action button built on Flag's Button: swaps CopyIcon -> CheckIcon
// (in --color-success) for `copiedDurationMs`, keeps an accessible label, and
// announces completion politely. Tier A: .btn.btn-copy[data-copied].
function CopyButton({
  value,
  label = "Copy",
  copiedLabel = "Copied",
  copiedDurationMs = 1800,
  variant = "ghost",
  size = "icon-sm",
  type = "button",
  className,
  children,
  onClick,
  onCopySuccess,
  "aria-label": ariaLabel,
  ...props
}: Omit<React.ComponentProps<typeof Button>, "asChild"> & {
  value: string
  label?: string
  copiedLabel?: string
  copiedDurationMs?: number
  onCopySuccess?: (value: string) => void
}) {
  const [copiedValue, setCopiedValue] = React.useState<string | null>(null)
  const mountedRef = React.useRef(true)
  const copyIdRef = React.useRef(0)
  const prevValueRef = React.useRef(value)
  const timeoutRef = React.useRef<ReturnType<typeof setTimeout> | null>(null)
  const copied = copiedValue !== null && copiedValue === value

  React.useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
      copyIdRef.current += 1
      if (timeoutRef.current !== null) {
        clearTimeout(timeoutRef.current)
        timeoutRef.current = null
      }
    }
  }, [])

  React.useEffect(() => {
    if (prevValueRef.current === value) return
    prevValueRef.current = value
    copyIdRef.current += 1
    if (timeoutRef.current !== null) {
      clearTimeout(timeoutRef.current)
      timeoutRef.current = setTimeout(() => {
        if (!mountedRef.current) return
        setCopiedValue(null)
        timeoutRef.current = null
      }, 0)
    }
  }, [value])

  const resolvedAriaLabel =
    ariaLabel ?? (children ? undefined : copied ? copiedLabel : label)

  return (
    <Button
      type={type}
      variant={variant}
      size={size}
      aria-label={resolvedAriaLabel}
      className={cn(className)}
      onClick={(event) => {
        onClick?.(event)
        if (event.defaultPrevented || !navigator.clipboard?.writeText) return
        const targetValue = value
        const copyId = ++copyIdRef.current
        if (timeoutRef.current !== null) {
          clearTimeout(timeoutRef.current)
          timeoutRef.current = null
        }
        void (async () => {
          try {
            await navigator.clipboard.writeText(targetValue)
            if (!mountedRef.current || copyId !== copyIdRef.current) return
            setCopiedValue(targetValue)
            onCopySuccess?.(targetValue)
            timeoutRef.current = setTimeout(() => {
              if (!mountedRef.current || copyId !== copyIdRef.current) return
              setCopiedValue(null)
              timeoutRef.current = null
            }, copiedDurationMs)
          } catch {
            if (!mountedRef.current || copyId !== copyIdRef.current) return
            /* clipboard write may fail in non-secure contexts */
          }
        })()
      }}
      {...props}
      data-slot="copy-button"
      data-copied={copied ? "true" : undefined}
    >
      {copied ? (
        <CheckIcon aria-hidden="true" className="text-success" />
      ) : (
        <CopyIcon aria-hidden="true" />
      )}
      {children}
      <span className="sr-only" aria-live="polite">
        {copied ? copiedLabel : ""}
      </span>
    </Button>
  )
}

export { CopyButton }
