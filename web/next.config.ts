import type { NextConfig } from "next";
import { execSync } from "node:child_process";
import { randomBytes } from "node:crypto";

// resolveBuildId picks the string that identifies this deploy. The
// cache-busting story depends on it: the request proxy stamps it on every
// response as X-App-Version, and the client compares subsequent
// probes against the value baked into its bundle. Anything unique
// per `next build` works; git SHA is nicest because `chat backup`
// dumps line up with deploy commits without extra bookkeeping.
function resolveBuildId(): string {
  if (process.env.BUILD_ID) return process.env.BUILD_ID;
  try {
    const sha = execSync("git rev-parse --short HEAD", {
      stdio: ["ignore", "pipe", "ignore"],
      encoding: "utf8",
    }).trim();
    if (sha) return sha;
  } catch {
    // not a git repo (container builds, tarball installs) — fall through
  }
  return randomBytes(6).toString("hex");
}

const BUILD_ID = resolveBuildId();

const nextConfig: NextConfig = {
  allowedDevOrigins: ["*.ngrok-free.dev"],

  // Lift the per-request body buffer cap. Next.js 16 introduced
  // `experimental.proxyClientMaxBodySize` (default 10 MB) that
  // truncates the body buffered for every request — including
  // streaming proxies like /api/attachments. With the default,
  // any upload >10 MB reaches chat-server with a chopped-off
  // multipart stream and `r.ParseMultipartForm` fails with
  // "unexpected EOF". Match the server's whole-request cap
  // (2× the 1 GiB per-file default; see FLEET_UPLOAD_MAX_BYTES)
  // so the front- and back-end limits stay aligned. Raise both
  // together if a deployment lifts the per-file limit.
  experimental: {
    proxyClientMaxBodySize: "2gb",
  },

  // Pin the Next.js build id so the hashed asset paths match the
  // value we stamp on X-App-Version. Without this, Next generates a
  // random id per build and our NEXT_PUBLIC_BUILD_ID below drifts
  // away from the on-disk chunk IDs.
  generateBuildId: async () => BUILD_ID,

  // Exposed to both server and client code (NEXT_PUBLIC_ prefix
  // inlines it at build time). chat-experience.tsx reads it on
  // mount as the baseline for drift detection.
  env: {
    NEXT_PUBLIC_BUILD_ID: BUILD_ID,
  },
};

export default nextConfig;
