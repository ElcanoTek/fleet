// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseLibDownloadsAreBounded — the bootstrap verifier must not let a
// hostile or broken server fill the disk: a body over the ceiling is refused,
// with or without an advertised length, and no partial file is left behind.
func TestReleaseLibDownloadsAreBounded(t *testing.T) {
	big := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chunked" {
			// No Content-Length: flush in pieces so the response is chunked.
			for i := 0; i < 4; i++ {
				_, _ = w.Write([]byte(big[:1024]))
				w.(http.Flusher).Flush()
			}
			return
		}
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()
	for _, path := range []string{"/sized", "/chunked"} {
		out := filepath.Join(t.TempDir(), "f")
		res, err := runReleaseLib(t, []string{"URL=" + srv.URL + path, "OUT=" + out}, `fleet_release_get "$URL" "$OUT" 1024`)
		if err == nil {
			t.Errorf("%s: a 4 KiB body passed a 1 KiB ceiling\n%s", path, res)
		}
		if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
			t.Errorf("%s: a refused download left %s behind", path, out)
		}
	}
	out := filepath.Join(t.TempDir(), "f")
	if res, err := runReleaseLib(t, []string{"URL=" + srv.URL + "/sized", "OUT=" + out}, `fleet_release_get "$URL" "$OUT" 8192`); err != nil {
		t.Fatalf("a body under the ceiling was refused: %v\n%s", err, res)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("download not saved 0600: %v %v", fi, err)
	}
}

// TestReleaseLibParsesCompactJSON — GitHub's API answer may be compact.
func TestReleaseLibParsesCompactJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"x","tag_name":"v2026.11.07.3","name":"fleet"}`))
	}))
	defer srv.Close()
	out, err := runReleaseLib(t, []string{"FLEET_RELEASE_LATEST_URL=" + srv.URL}, `fleet_release_latest_tag`)
	if err != nil || out != "v2026.11.07.3" {
		t.Fatalf("got %q, %v", out, err)
	}
}
