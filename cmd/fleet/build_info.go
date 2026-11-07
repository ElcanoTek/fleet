// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/version"
	"github.com/ElcanoTek/fleet/internal/webui"
)

// buildInfo is `fleet version --build-info`: the facts a release pipeline and
// an updater check about a binary, as JSON. host_executor is the compile-time
// marker for the `fleet_host_executor` build tag (#159): release.yml refuses to
// publish, and `fleet release fetch` refuses to install, a binary where it is
// true — on top of the `go version -m` build-settings check, which reads the
// same fact from the toolchain's own record.
type buildInfo struct {
	Version      string `json:"version"`
	Revision     string `json:"revision,omitempty"`
	GOOS         string `json:"goos"`
	GOARCH       string `json:"goarch"`
	CGO          string `json:"cgo_enabled,omitempty"`
	Tags         string `json:"tags,omitempty"`
	HostExecutor bool   `json:"host_executor"`
	WebBundle    string `json:"web_bundle"`
}

func collectBuildInfo() buildInfo {
	bi := buildInfo{
		Version:      version.Version(),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		HostExecutor: sandbox.HostExecutorCompiledIn(),
		WebBundle:    webui.EmbeddedBuildID(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				bi.Revision = s.Value
			case "CGO_ENABLED":
				bi.CGO = s.Value
			case "-tags":
				bi.Tags = strings.TrimSpace(s.Value)
			}
		}
	}
	return bi
}

// printBuildInfo writes the build info as one JSON object. It exits 0 even for
// a binary with the host executor compiled in: reporting is its job, and the
// callers decide.
func printBuildInfo(w io.Writer) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(collectBuildInfo()); err != nil {
		return 1
	}
	return 0
}
