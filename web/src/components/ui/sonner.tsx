import {
  CircleCheckIcon,
  InfoIcon,
  Loader2Icon,
  OctagonXIcon,
  TriangleAlertIcon,
} from "lucide-react"
import { Toaster as Sonner, type ToasterProps } from "sonner"

import { useFlagTheme } from "@/hooks/use-flag-theme"

// Toasts on the Flag popover surface. The theme follows html[data-theme]
// (Flag's contract) instead of next-themes; status icons take the base
// status colors, measured at 4.5:1 on every surface. Tier A: .toast-region / .toast .toast--<type>.
const Toaster = ({ ...props }: ToasterProps) => {
  const theme = useFlagTheme()

  return (
    <Sonner
      theme={theme}
      className="toaster group"
      icons={{
        success: (
          <CircleCheckIcon className="size-4 text-success" />
        ),
        info: <InfoIcon className="size-4 text-(--color-link)" />,
        warning: (
          <TriangleAlertIcon className="size-4 text-warning" />
        ),
        error: <OctagonXIcon className="size-4 text-destructive" />,
        loading: <Loader2Icon className="size-4 motion-safe:animate-spin" />,
      }}
      toastOptions={{
        classNames: {
          toast: "font-sans shadow-(--shadow-popover)!",
          description: "text-subtle!",
          actionButton: "bg-primary! text-primary-foreground!",
          cancelButton: "bg-secondary! text-secondary-foreground!",
        },
      }}
      style={
        {
          "--normal-bg": "var(--popover)",
          "--normal-text": "var(--popover-foreground)",
          "--normal-border": "var(--color-selected-border)",
          "--border-radius": "var(--radius-lg)",
        } as React.CSSProperties
      }
      {...props}
    />
  )
}

export { Toaster }
