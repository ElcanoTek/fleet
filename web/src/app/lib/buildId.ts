// Single source of truth for the current build's identifier.
//
// In production Next sets NEXT_PUBLIC_BUILD_ID via next.config.ts's
// `env` block, which inlines the value into both server and client
// bundles. In dev (or if the config hasn't been picked up for some
// reason), we fall back to "dev" so the probe still works without
// crashing. Never throws — a missing build id is not a fatal.
export function currentBuildId(): string {
  return process.env.NEXT_PUBLIC_BUILD_ID ?? "dev";
}

// Header name used by middleware + API routes + /api/version. Kept
// as a constant so client + server code can't drift on the spelling.
export const BUILD_ID_HEADER = "X-App-Version";
