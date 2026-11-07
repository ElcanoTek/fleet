// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"strings"
	"testing"
)

func TestCheckBuildInfoRefusesTheHostExecutor(t *testing.T) {
	good := `{"version":"2026.11.07.3","goos":"linux","goarch":"amd64","host_executor":false,"web_bundle":"abc123"}`
	if err := checkBuildInfo([]byte(good), "amd64"); err != nil {
		t.Fatalf("good build info refused: %v", err)
	}
	for name, tc := range map[string]struct{ json, want string }{
		"host executor": {`{"goarch":"amd64","host_executor":true,"web_bundle":"x"}`, "host executor"},
		"tag recorded":  {`{"goarch":"amd64","host_executor":false,"tags":"fleet_host_executor","web_bundle":"x"}`, "host executor"},
		"no marker":     {`{"goarch":"amd64","web_bundle":"x"}`, "host executor"},
		"no web bundle": {`{"goarch":"amd64","host_executor":false,"web_bundle":""}`, "web bundle"},
		"wrong arch":    {`{"goarch":"arm64","host_executor":false,"web_bundle":"x"}`, "GOARCH"},
		"not json":      {`fleet 2026.11.07.3`, "unreadable"},
	} {
		err := checkBuildInfo([]byte(tc.json), "amd64")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error mentioning %q, got %v", name, tc.want, err)
		}
	}
}

func TestReleaseVerbUsage(t *testing.T) {
	if rc := cmdRelease(nil); rc != 2 {
		t.Errorf("bare `fleet release` = %d, want 2", rc)
	}
	if rc := cmdRelease([]string{"bogus"}); rc != 2 {
		t.Errorf("unknown subcommand = %d, want 2", rc)
	}
	for _, argv := range [][]string{
		{"fetch", "--dir", "/tmp/x"},                    // no tag
		{"fetch", "--tag", "latest", "--dir", "/tmp/x"}, // not a release tag
		{"fetch", "--tag", "v2026.11.07.1"},             // no dir
		{"install", "--tag", "main", "--dir", "/tmp/x"},
		{"sandbox", "--tag", "v2026.11.07.1", "--dir", "/tmp/x", "extra"},
	} {
		if rc := cmdRelease(argv); rc != 2 {
			t.Errorf("%v = %d, want usage error 2", argv, rc)
		}
	}
}
