// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"embed"
	"io/fs"
)

// bundle/ holds the Vite build (bundle/dist, written by `make web`; ignored
// by git) next to a committed README, so the embed pattern always matches and
// a plain `go build`/`go test` works without a web build. Such a binary
// serves the API but answers page routes with a 503 explaining how to build
// the bundle; the release workflow builds the bundle first and checks the
// artifact embeds it (`fleet version` reports the bundle's build id).
//
//go:embed all:bundle
var bundleFS embed.FS

// EmbeddedBundle is the SPA compiled into this binary.
func EmbeddedBundle() fs.FS {
	sub, err := fs.Sub(bundleFS, "bundle/dist")
	if err != nil {
		return emptyFS{}
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// EmbeddedBuildID is the embedded bundle's build id, or "" when the binary
// carries no bundle. `fleet version` prints it; the release workflow refuses an
// artifact that reports none.
func EmbeddedBuildID() string {
	s, err := newSPA(EmbeddedBundle(), "")
	if err != nil || !s.present {
		return ""
	}
	return s.buildID
}
