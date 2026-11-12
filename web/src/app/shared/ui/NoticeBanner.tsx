import type { HTMLAttributes, ReactNode } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { cn } from "@/lib/utils";

// NoticeBanner — the one page-level notice/error banner recipe, backed by
// Flag's @flag/alert primitive while preserving Fleet's card-tested -strong /
// -soft status token pairs and caller-controlled ARIA roles.
export type NoticeTone = "success" | "warning" | "danger";

const TONE_CLASS: Record<NoticeTone, string> = {
  success:
    "border-[var(--color-success-strong)] bg-[color-mix(in_srgb,var(--color-success-strong)_15%,transparent)] text-[var(--color-success-soft)]",
  warning:
    "border-[var(--color-warning-strong)] bg-[color-mix(in_srgb,var(--color-warning-strong)_15%,transparent)] text-[var(--color-warning-soft)]",
  danger:
    "border-[var(--color-danger-strong)] bg-[color-mix(in_srgb,var(--color-danger-strong)_15%,transparent)] text-[var(--color-danger-soft)]",
};

const ALERT_VARIANT: Record<NoticeTone, "success" | "warning" | "destructive"> = {
  success: "success",
  warning: "warning",
  danger: "destructive",
};

export function NoticeBanner({
  tone,
  children,
  className,
  role,
  ...rest
}: {
  tone: NoticeTone;
  children: ReactNode;
  className?: string;
} & HTMLAttributes<HTMLDivElement>) {
  const resolvedRole = role ?? (tone === "danger" ? "alert" : "status");
  return (
    <Alert
      {...rest}
      variant={ALERT_VARIANT[tone]}
      role={resolvedRole}
      className={cn(
        "block rounded-[var(--radius-lg)] px-4 py-3 text-[0.875rem]",
        TONE_CLASS[tone],
        className,
      )}
    >
      <AlertDescription className="block text-[0.875rem] text-inherit">
        {children}
      </AlertDescription>
    </Alert>
  );
}
