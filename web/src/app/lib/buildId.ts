// Single source of truth for the current build's identifier.
//
// vite.config.ts bakes VITE_BUILD_ID into the bundle and writes the same value
// to dist/build-id.txt, which the Go server stamps on every response as
// X-App-Version — so the client can compare the two and notice a deploy
// (shared/deployDrift.ts). In dev, or a test without the stub, it is "dev".
// Never throws — a missing build id is not fatal.
export function currentBuildId(): string {
  return import.meta.env.VITE_BUILD_ID || "dev";
}

// Header name the server stamps (internal/webui.BuildIDHeader). Kept as a
// constant so client and server can't drift on the spelling.
export const BUILD_ID_HEADER = "X-App-Version";
