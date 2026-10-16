"use client";

import { ErrorScreen } from "@/app/shared/ui/ErrorScreen";

// The error boundary for every page under the root layout. The layout itself
// (and its stylesheet, theme and brand palette) keeps rendering, so this screen
// paints in the deployment's colors. A throw in the root layout falls through
// to global-error.tsx instead.
export default function RouteError({ error }: { error: Error & { digest?: string } }) {
  return <ErrorScreen error={error} />;
}
