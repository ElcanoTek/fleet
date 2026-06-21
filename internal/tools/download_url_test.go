package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// downloadCtx returns a context carrying a per-conversation workspace
// rooted in a temp dir, so downloads land somewhere t.Cleanup will
// blow away. Mirrors withWorkspace in fastio_upload_test.go but without
// pre-seeding any file.
func downloadCtx(t *testing.T) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	const convID = "conv-download-url-test"
	dir, err := EnsureWorkspaceDir(convID)
	if err != nil {
		t.Fatalf("EnsureWorkspaceDir: %v", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return WithConversationID(context.Background(), convID), abs
}

func TestDownloadURL_SuccessUsesURLPathFilename(t *testing.T) {
	body := []byte("hello,world\n1,2\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "Mozilla") {
			t.Errorf("expected browser User-Agent, got %q", got)
		}
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ctx, dir := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/path/report.csv"})

	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if !strings.HasPrefix(res.Filename, "report__") || !strings.HasSuffix(res.Filename, ".csv") {
		t.Fatalf("unexpected filename %q (want report__<hash>.csv)", res.Filename)
	}
	if filepath.Dir(res.SavedTo) != dir {
		t.Fatalf("saved outside per-conv workspace: dir=%s saved=%s", dir, res.SavedTo)
	}
	if res.SizeBytes != int64(len(body)) {
		t.Fatalf("size mismatch: want %d got %d", len(body), res.SizeBytes)
	}
	got, err := os.ReadFile(res.SavedTo)
	if err != nil {
		t.Fatalf("read saved: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("bytes mismatch: want %q got %q", body, got)
	}
}

func TestDownloadURL_FilenameFromContentDisposition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="TWC_KOC_ABC.xlsx"`)
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		_, _ = w.Write([]byte("PK\x03\x04"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/storage/abc/read"})

	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if !strings.HasPrefix(res.Filename, "TWC_KOC_ABC__") || !strings.HasSuffix(res.Filename, ".xlsx") {
		t.Fatalf("unexpected filename %q", res.Filename)
	}
}

func TestDownloadURL_FilenameFromContentTypeFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF-1.4\n"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	// URL path has no extension and no Content-Disposition → fall back
	// to the content-type-derived `.pdf`.
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/download"})

	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if !strings.HasSuffix(res.Filename, ".pdf") {
		t.Fatalf("expected .pdf fallback, got %q", res.Filename)
	}
}

func TestDownloadURL_CallerFilenameOverrides(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="from-header.csv"`)
		_, _ = w.Write([]byte("a,b\n1,2\n"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{
		URL:      srv.URL + "/r",
		Filename: "agent_chose_this.csv",
	})

	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if !strings.HasPrefix(res.Filename, "agent_chose_this__") {
		t.Fatalf("override ignored: got %q", res.Filename)
	}
}

func TestDownloadURL_RejectsBadScheme(t *testing.T) {
	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: "file:///etc/passwd"})
	if res.Status != "error" {
		t.Fatalf("want error for file:// scheme, got %+v", res)
	}
	if !strings.Contains(res.Error, "http") {
		t.Fatalf("error message should mention scheme: %q", res.Error)
	}
}

func TestDownloadURL_RejectsEmptyURL(t *testing.T) {
	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: ""})
	if res.Status != "error" || !strings.Contains(res.Error, "url is required") {
		t.Fatalf("want url required error, got %+v", res)
	}
}

func TestDownloadURL_HTTPErrorStatusSurfacesBodyPreview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("token expired"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL})
	if res.Status != "error" {
		t.Fatalf("want error status, got %+v", res)
	}
	if res.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("http_status=%d want 401", res.HTTPStatus)
	}
	if !strings.Contains(res.Error, "401") || !strings.Contains(res.Error, "token expired") {
		t.Fatalf("error should preview upstream body, got %q", res.Error)
	}
}

func TestDownloadURL_RedirectChainRecorded(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, srvURL+"/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, srvURL+"/end.csv", http.StatusFound)
		case "/end.csv":
			w.Header().Set("Content-Type", "text/csv")
			_, _ = w.Write([]byte("ok"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/start"})
	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if len(res.RedirectChain) != 3 {
		t.Fatalf("want 3-entry chain (start → middle → end), got %v", res.RedirectChain)
	}
	if !strings.HasSuffix(res.RedirectChain[0], "/start") ||
		!strings.HasSuffix(res.RedirectChain[1], "/middle") ||
		!strings.HasSuffix(res.RedirectChain[2], "/end.csv") {
		t.Fatalf("chain entries wrong: %v", res.RedirectChain)
	}
	if !strings.HasSuffix(res.FinalURL, "/end.csv") {
		t.Fatalf("final_url should be end.csv, got %q", res.FinalURL)
	}
}

func TestDownloadURL_NoRedirectChainIsSingleEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/x.txt"})
	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	if len(res.RedirectChain) != 1 || !strings.HasSuffix(res.RedirectChain[0], "/x.txt") {
		t.Fatalf("expected single-entry chain pointing at the URL, got %v", res.RedirectChain)
	}
}

func TestDownloadURL_SizeCapEnforced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Stream past the cap, byte by byte cheaply.
		chunk := make([]byte, 1024*1024)
		for i := 0; i < 101; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, dir := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/big.bin"})
	if res.Status != "error" {
		t.Fatalf("want error past 100MB cap, got %+v", res)
	}
	if !strings.Contains(res.Error, "cap") {
		t.Fatalf("error should mention cap, got %q", res.Error)
	}
	// Partial file should have been cleaned up.
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, m := range matches {
		if strings.Contains(filepath.Base(m), "__") {
			t.Fatalf("partial download was not cleaned up: %s", m)
		}
	}
}

func TestDownloadURL_DeduplicatesByURLHash(t *testing.T) {
	body := []byte("same bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	ctx, dir := downloadCtx(t)
	url := srv.URL + "/same.txt"

	first := runDownloadURL(ctx, DownloadURLParams{URL: url})
	second := runDownloadURL(ctx, DownloadURLParams{URL: url})

	if first.Status != "success" || second.Status != "success" {
		t.Fatalf("downloads failed: first=%q second=%q", first.Error, second.Error)
	}
	if first.SavedTo == second.SavedTo {
		t.Fatalf("second download should not have clobbered the first: %s == %s", first.SavedTo, second.SavedTo)
	}
	// Both files should still exist; the second one carries the `_1` suffix.
	if _, err := os.Stat(first.SavedTo); err != nil {
		t.Fatalf("first file vanished: %v", err)
	}
	base := filepath.Base(second.SavedTo)
	if !strings.Contains(base, "_1.") {
		t.Fatalf("expected _1 suffix on duplicate, got %s in dir=%s", base, dir)
	}
}

func TestDownloadURL_AbsoluteOutputDirOutsideAllowedRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	// /root is generally NOT inside any allowed dir for the test
	// process (cwd is the chat server dir; temp dir is whitelisted).
	// Pick an outside-allowed absolute path that exists.
	res := runDownloadURL(ctx, DownloadURLParams{
		URL:       srv.URL,
		OutputDir: "/etc",
	})
	if res.Status != "error" {
		t.Fatalf("want error for disallowed absolute output_dir, got %+v", res)
	}
	if !strings.Contains(res.Error, "allowed") {
		t.Fatalf("error should mention allowed dirs, got %q", res.Error)
	}
}

func TestDownloadURL_RelativeOutputDirAnchorsToWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	ctx, workspaceDir := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{
		URL:       srv.URL + "/r.csv",
		OutputDir: "subdir/nested",
	})
	if res.Status != "success" {
		t.Fatalf("status=%q err=%q", res.Status, res.Error)
	}
	want := filepath.Join(workspaceDir, "subdir", "nested")
	if filepath.Dir(res.SavedTo) != want {
		t.Fatalf("saved under %s, want %s", filepath.Dir(res.SavedTo), want)
	}
}

func TestDownloadURL_ResultMarshalsAsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/api"})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(b, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"status", "url", "filename", "saved_to", "size_bytes", "content_type"} {
		if _, ok := roundTrip[key]; !ok {
			t.Errorf("missing %q in marshaled result: %s", key, b)
		}
	}
}

func TestPickDownloadFilename_SanitizesPathTraversal(t *testing.T) {
	got := sanitizeDownloadFilename("../../etc/passwd")
	if strings.ContainsAny(got, `/\`) {
		t.Fatalf("path separators leaked through sanitize: %q", got)
	}
	if got == "" {
		t.Fatalf("expected a sanitized name, got empty")
	}
}

func TestExtensionFromContentType(t *testing.T) {
	cases := map[string]string{
		"text/csv":                ".csv",
		"application/pdf":         ".pdf",
		"text/csv; charset=utf-8": ".csv",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
	}
	for ct, want := range cases {
		if got := extensionFromContentType(ct); got != want {
			t.Errorf("extensionFromContentType(%q) = %q, want %q", ct, got, want)
		}
	}
}

func TestDownloadURL_RegisteredInDefaultTools(t *testing.T) {
	found := false
	for _, tool := range DefaultTools() {
		if tool.Info().Name == "download_url" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("download_url is not registered in DefaultTools()")
	}
}

// guards against a regression where the marshaled error payload would
// silently drop the upstream HTTP status, which downstream code keys on.
func TestDownloadURL_ErrorJSONPreservesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, "nope")
	}))
	defer srv.Close()

	ctx, _ := downloadCtx(t)
	res := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL})
	if res.HTTPStatus != http.StatusForbidden {
		t.Fatalf("http_status=%d want 403", res.HTTPStatus)
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"http_status":403`) {
		t.Fatalf("http_status field missing from JSON: %s", b)
	}
}
