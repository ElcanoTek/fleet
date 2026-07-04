// Package rampartservice embeds the reference Rampart detection service —
// server, manifest, and Containerfile — so the fleet binary can build and host
// the service itself (the admin panel's one-click install,
// internal/rampartinstall) without depending on a source checkout being
// present at a known path. The files in THIS directory remain the canonical,
// runnable copies for the manual paths (install.sh, npm start); the embed just
// carries the same bytes inside the binary.
package rampartservice

import "embed"

// Files holds the container build context for the reference service.
//
//go:embed server.mjs package.json Containerfile
var Files embed.FS
