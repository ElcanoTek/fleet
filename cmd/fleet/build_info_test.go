// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// TestBuildInfoReportsTheHostExecutorMarker pins the contract release.yml and
// `fleet release fetch` rely on: `fleet version --build-info` is JSON whose
// host_executor field is the compile-time fact, present in every build.
func TestBuildInfoReportsTheHostExecutorMarker(t *testing.T) {
	var buf bytes.Buffer
	if rc := printBuildInfo(&buf); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	he, ok := got["host_executor"].(bool)
	if !ok {
		t.Fatalf("host_executor missing or not a bool: %s", buf.String())
	}
	if he != sandbox.HostExecutorCompiledIn() {
		t.Errorf("host_executor=%v, compiled in=%v", he, sandbox.HostExecutorCompiledIn())
	}
	if got["goarch"] != runtime.GOARCH {
		t.Errorf("goarch %v", got["goarch"])
	}
	if _, ok := got["web_bundle"]; !ok {
		t.Error("web_bundle must always be present (empty when missing)")
	}
}
