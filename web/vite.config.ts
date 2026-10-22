import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import { execSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

// The web tier is a static single-page app compiled into the fleet binary
// (ADR-0091): `vite build` writes ../internal/webui/bundle/dist, which
// internal/webui embeds with go:embed and serves — index.html per request
// (metadata + bootstrap JSON rendered in), /assets/* content-hashed and
// immutable. There is no Node server in production.

// resolveBuildId identifies this build. The Go server stamps it on every
// response as X-App-Version and the client compares it with the value baked
// into its bundle to notice a deploy. git SHA is nicest (backups line up with
// deploy commits); BUILD_ID overrides; random is the last resort.
function resolveBuildId(): string {
  if (process.env.BUILD_ID) return process.env.BUILD_ID;
  try {
    const sha = execSync("git rev-parse --short HEAD", {
      stdio: ["ignore", "pipe", "ignore"],
      encoding: "utf8",
    }).trim();
    if (sha) return sha;
  } catch {
    // not a git checkout (tarball installs) — fall through
  }
  return randomBytes(6).toString("hex");
}

const BUILD_ID = resolveBuildId();
const OUT_DIR = path.resolve(__dirname, "../internal/webui/bundle/dist");

// writeBuildId drops the id next to index.html for the Go server to read.
function writeBuildId(): Plugin {
  return {
    name: "fleet-build-id",
    apply: "build",
    closeBundle() {
      mkdirSync(OUT_DIR, { recursive: true });
      writeFileSync(path.join(OUT_DIR, "build-id.txt"), `${BUILD_ID}\n`);
    },
  };
}

// checkCssUrls fails the build when an emitted stylesheet still holds a
// relative url() — one Vite could not resolve to a bundled asset. That is how
// the fonts once shipped pointing at /assets/nebula-sans/… (404): Tailwind
// inlined an @import without rebasing its url()s. A moved or renamed file must
// be a build error, not a silent 404 in production.
function checkCssUrls(): Plugin {
  return {
    name: "fleet-check-css-urls",
    apply: "build",
    generateBundle(_options, bundle) {
      const bad: string[] = [];
      for (const [name, out] of Object.entries(bundle)) {
        if (out.type !== "asset" || !name.endsWith(".css")) continue;
        const css = typeof out.source === "string" ? out.source : new TextDecoder().decode(out.source);
        for (const m of css.matchAll(/url\(\s*["']?([^"')]+)["']?\s*\)/g)) {
          const u = m[1];
          if (/^(data:|#|\/|https?:)/.test(u) || u.startsWith("%23")) continue;
          // A sibling in the same output dir is a resolved, emitted asset.
          const target = path.posix.join(path.posix.dirname(name), u);
          if (!bundle[target]) bad.push(`${name}: url(${u})`);
        }
      }
      if (bad.length) this.error(`unresolved url() in emitted CSS:\n  ${bad.join("\n  ")}`);
    },
  };
}

export default defineConfig({
  plugins: [react(), writeBuildId(), checkCssUrls()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  define: {
    "import.meta.env.VITE_BUILD_ID": JSON.stringify(BUILD_ID),
  },
  build: {
    outDir: OUT_DIR,
    emptyOutDir: true,
    assetsDir: "assets",
    // Source maps stay out of the binary; the bundle is served to every
    // visitor, signed in or not.
    sourcemap: false,
    // The chat view is one large chunk by nature (markdown, highlighting,
    // virtualized transcript); it loads only on /chat.
    chunkSizeWarningLimit: 1024,
  },
  server: {
    // `make dev`: the Go server (FLEET_WEB_DEV_SERVER=http://127.0.0.1:5173)
    // fronts this dev server so the real gate, CSP and /api run; the HMR
    // socket is reached directly on this port.
    host: "127.0.0.1",
    port: 5173,
    strictPort: true,
    hmr: { host: "127.0.0.1", port: 5173, clientPort: 5173 },
  },
});
